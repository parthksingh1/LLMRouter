// Package budget enforces per-tenant spend limits.
//
// The whole design turns on one requirement: two concurrent requests from the same tenant must
// not both be admitted when only one fits inside the remaining budget. A read-then-write in Go
// cannot provide that -- between the GET and the SET, any number of other requests can read the
// same value. So the increment-and-check happens inside Redis as a single Lua script, which
// Redis executes atomically.
//
// Budgets are enforced on a UTC day boundary. That is a deliberate simplification over a rolling
// window: a rolling window needs a sorted set per tenant and an eviction sweep, and a daily reset
// is what finance actually reconciles against.
package budget

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// reserveScript atomically adds an estimate to a tenant's daily usage and reports the verdict.
//
// KEYS[1] the usage counter for this tenant and day
// ARGV[1] tokens to reserve
// ARGV[2] the daily limit
// ARGV[3] the warn threshold as a fraction
// ARGV[4] TTL in seconds
//
// Returns {allowed, warn, used, limit}.
//
// Note the rollback on refusal: a request that is denied must not consume budget, or a tenant
// hammering a full budget would keep its own counter climbing and never recover. This is exactly
// the race a Go-side read-modify-write cannot express.
const reserveScript = `
local used = redis.call('INCRBY', KEYS[1], ARGV[1])
if used == tonumber(ARGV[1]) then
  redis.call('EXPIRE', KEYS[1], ARGV[4])
end

local limit = tonumber(ARGV[2])
local warn_at = limit * tonumber(ARGV[3])

if used > limit then
  redis.call('DECRBY', KEYS[1], ARGV[1])
  return {0, 0, used - tonumber(ARGV[1]), limit}
end

local warn = 0
if used >= warn_at then warn = 1 end
return {1, warn, used, limit}
`

// commitScript reconciles an estimate against the real usage once a request has completed.
//
// KEYS[1] the token counter
// KEYS[2] the spend counter, in micro-dollars so the value stays an integer
// ARGV[1] the difference between actual and estimated tokens (may be negative)
// ARGV[2] the cost in micro-dollars
// ARGV[3] TTL in seconds
//
// Reservation uses an estimate because the real token count is unknown until the provider
// answers. Committing the difference rather than the total is what keeps the counter honest
// without ever letting it drift below zero.
const commitScript = `
local tokens = redis.call('INCRBY', KEYS[1], ARGV[1])
if tokens < 0 then
  redis.call('SET', KEYS[1], 0)
end
redis.call('EXPIRE', KEYS[1], ARGV[3])

redis.call('INCRBY', KEYS[2], ARGV[2])
redis.call('EXPIRE', KEYS[2], ARGV[3])
return 1
`

// Redis is the subset of the client this package needs, so tests can supply a fake.
type Redis interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Ping(ctx context.Context) *redis.StatusCmd
	Close() error
}

// Enforcer is the Redis-backed budget. It satisfies app.Budget.
type Enforcer struct {
	client Redis
	log    *slog.Logger
	now    func() time.Time
}

var _ app.Budget = (*Enforcer)(nil)

// Options configures the enforcer.
type Options struct {
	RedisURL string
	Log      *slog.Logger
	// Now is injectable so tests can cross a day boundary without waiting for midnight.
	Now func() time.Time
}

// New connects to Redis and builds the enforcer.
func New(opts Options) (*Enforcer, error) {
	parsed, err := redis.ParseURL(opts.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parsing BUDGET_REDIS_URL: %w", err)
	}
	parsed.DialTimeout = 2 * time.Second
	parsed.ReadTimeout = 500 * time.Millisecond
	parsed.WriteTimeout = 500 * time.Millisecond

	return NewWithClient(redis.NewClient(parsed), opts), nil
}

// NewWithClient builds the enforcer over an existing client.
func NewWithClient(client Redis, opts Options) *Enforcer {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Enforcer{client: client, log: opts.Log, now: opts.Now}
}

// Reserve admits or refuses a request against the tenant's daily token budget.
func (e *Enforcer) Reserve(ctx context.Context, t domain.Tenant, estTokens int) (domain.BudgetVerdict, error) {
	if t.DailyTokenBudget <= 0 {
		return domain.BudgetVerdict{Allowed: true}, nil
	}
	if estTokens < 0 {
		estTokens = 0
	}

	warn := t.WarnThreshold
	if warn <= 0 || warn > 1 {
		warn = 0.8
	}

	key := e.tokenKey(t.ID)
	ttl := e.secondsUntilTomorrow()

	raw, err := e.client.Eval(ctx, reserveScript, []string{key},
		estTokens, t.DailyTokenBudget, warn, ttl).Result()
	if err != nil {
		return domain.BudgetVerdict{}, fmt.Errorf("reserving budget: %w", err)
	}

	values, ok := raw.([]any)
	if !ok || len(values) != 4 {
		return domain.BudgetVerdict{}, fmt.Errorf("unexpected budget script result %T", raw)
	}

	allowed := asInt64(values[0]) == 1
	verdict := domain.BudgetVerdict{
		Allowed:     allowed,
		Warn:        asInt64(values[1]) == 1,
		UsedTokens:  asInt64(values[2]),
		LimitTokens: asInt64(values[3]),
	}
	if !allowed {
		verdict.RetryAfterSec = ttl
		verdict.Reason = fmt.Sprintf(
			"daily token budget exhausted (%d/%d); resets at 00:00 UTC",
			verdict.UsedTokens, verdict.LimitTokens)
	}
	return verdict, nil
}

// Commit reconciles the estimate against the actual usage and records spend.
func (e *Enforcer) Commit(ctx context.Context, t domain.Tenant, usage domain.Usage, costUSD float64) error {
	if t.DailyTokenBudget <= 0 {
		return nil
	}

	// The reservation already added an estimate; only the difference is applied here. The
	// estimate is recomputed the same way ChatService computed it, so the two agree.
	actual := int64(usage.TotalTokens())

	// Spend is kept in micro-dollars so Redis stores an integer: floating point counters in a
	// shared store accumulate rounding error that nobody can reconcile later.
	micros := int64(costUSD * 1_000_000)

	_, err := e.client.Eval(ctx, commitScript,
		[]string{e.tokenKey(t.ID), e.spendKey(t.ID)},
		actual, micros, e.secondsUntilTomorrow()).Result()
	if err != nil {
		return fmt.Errorf("committing budget: %w", err)
	}
	return nil
}

// Usage reports a tenant's consumption today, for the dashboard and /readyz.
func (e *Enforcer) Usage(ctx context.Context, tenantID string) (tokens int64, spendUSD float64, err error) {
	tokenStr, err := e.client.Get(ctx, e.tokenKey(tenantID)).Result()
	if err != nil && err != redis.Nil {
		return 0, 0, fmt.Errorf("reading token usage: %w", err)
	}
	spendStr, err := e.client.Get(ctx, e.spendKey(tenantID)).Result()
	if err != nil && err != redis.Nil {
		return 0, 0, fmt.Errorf("reading spend: %w", err)
	}

	return parseInt(tokenStr), float64(parseInt(spendStr)) / 1_000_000, nil
}

// Ping checks connectivity.
func (e *Enforcer) Ping(ctx context.Context) error {
	if err := e.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("budget redis ping: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (e *Enforcer) Close() error { return e.client.Close() }

func (e *Enforcer) day() string {
	return e.now().UTC().Format("2006-01-02")
}

func (e *Enforcer) tokenKey(tenantID string) string {
	return fmt.Sprintf("llmrouter:budget:tokens:%s:%s", tenantID, e.day())
}

func (e *Enforcer) spendKey(tenantID string) string {
	return fmt.Sprintf("llmrouter:budget:spend_micros:%s:%s", tenantID, e.day())
}

// secondsUntilTomorrow is the TTL for today's counters, so they expire rather than accumulate.
func (e *Enforcer) secondsUntilTomorrow() int {
	now := e.now().UTC()
	tomorrow := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	seconds := int(tomorrow.Sub(now).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		return parseInt(n)
	default:
		return 0
	}
}

func parseInt(s string) int64 {
	var out int64
	negative := false
	for i, c := range s {
		if i == 0 && c == '-' {
			negative = true
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		out = out*10 + int64(c-'0')
	}
	if negative {
		return -out
	}
	return out
}
