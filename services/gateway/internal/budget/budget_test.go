package budget_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/budget"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// fakeRedis interprets the two Lua scripts well enough to test the enforcer's behaviour without
// a Redis process.
//
// Reimplementing the scripts is a real limitation of this test: it verifies the enforcer's
// contract, not that the Lua is correct. The Lua's correctness is exercised by the integration
// job against a real Redis. What it does prove is the part that would otherwise be untested --
// that a refused reservation does not consume budget, and that Commit reconciles the estimate.
type fakeRedis struct {
	mu     sync.Mutex
	values map[string]int64
	failOn string
}

func newFakeRedis() *fakeRedis { return &fakeRedis{values: map[string]int64{}} }

func (f *fakeRedis) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	cmd := redis.NewCmd(ctx)
	if f.failOn != "" {
		cmd.SetErr(errors.New(f.failOn))
		return cmd
	}

	// Distinguish the two scripts by a token unique to each.
	if strings.Contains(script, "INCRBY KEYS[1], ARGV[1]") || strings.Contains(script, "local used") {
		f.reserve(cmd, keys, args)
		return cmd
	}
	f.commit(cmd, keys, args)
	return cmd
}

func (f *fakeRedis) reserve(cmd *redis.Cmd, keys []string, args []any) {
	tokens := toInt(args[0])
	limit := toInt(args[1])
	warnFraction := toFloat(args[2])

	used := f.values[keys[0]] + tokens
	f.values[keys[0]] = used

	if used > limit {
		// The rollback that matters: a denied request must not consume budget, or a tenant
		// hammering a full budget would keep its own counter climbing and never recover.
		f.values[keys[0]] = used - tokens
		cmd.SetVal([]any{int64(0), int64(0), used - tokens, limit})
		return
	}

	warn := int64(0)
	if float64(used) >= float64(limit)*warnFraction {
		warn = 1
	}
	cmd.SetVal([]any{int64(1), warn, used, limit})
}

func (f *fakeRedis) commit(cmd *redis.Cmd, keys []string, args []any) {
	f.values[keys[0]] += toInt(args[0])
	if f.values[keys[0]] < 0 {
		f.values[keys[0]] = 0
	}
	f.values[keys[1]] += toInt(args[1])
	cmd.SetVal(int64(1))
}

func (f *fakeRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	cmd := redis.NewStringCmd(ctx)
	v, ok := f.values[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(strconv.FormatInt(v, 10))
	return cmd
}

func (f *fakeRedis) Ping(ctx context.Context) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetVal("PONG")
	return cmd
}

func (f *fakeRedis) Close() error { return nil }

func (f *fakeRedis) value(key string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[key]
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case string:
		parsed, _ := strconv.ParseInt(n, 10, 64)
		return parsed
	default:
		return 0
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0.8
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newEnforcer(client budget.Redis, now func() time.Time) *budget.Enforcer {
	return budget.NewWithClient(client, budget.Options{Log: quietLogger(), Now: now})
}

func tenant(limit int64) domain.Tenant {
	return domain.Tenant{ID: "tenant-a", DailyTokenBudget: limit, WarnThreshold: 0.8}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestReserveAdmitsWithinBudget(t *testing.T) {
	t.Parallel()

	e := newEnforcer(newFakeRedis(), fixedClock(time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)))

	verdict, err := e.Reserve(context.Background(), tenant(1000), 100)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !verdict.Allowed || verdict.Warn {
		t.Errorf("verdict = %+v, want allowed without a warning", verdict)
	}
	if verdict.UsedTokens != 100 || verdict.LimitTokens != 1000 {
		t.Errorf("verdict = %+v", verdict)
	}
}

func TestReserveWarnsAtTheThreshold(t *testing.T) {
	t.Parallel()

	e := newEnforcer(newFakeRedis(), fixedClock(time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)))
	ctx := context.Background()

	// 800 of 1000 is exactly the 80% warn threshold.
	if _, err := e.Reserve(ctx, tenant(1000), 799); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	verdict, err := e.Reserve(ctx, tenant(1000), 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !verdict.Allowed {
		t.Error("a warning must not block")
	}
	if !verdict.Warn {
		t.Error("expected a warning at the threshold")
	}
}

func TestReserveRefusesOverBudgetAndDoesNotConsumeIt(t *testing.T) {
	t.Parallel()

	fake := newFakeRedis()
	clock := fixedClock(time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	e := newEnforcer(fake, clock)
	ctx := context.Background()

	if _, err := e.Reserve(ctx, tenant(1000), 900); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	verdict, err := e.Reserve(ctx, tenant(1000), 200)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("expected the over-budget request to be refused")
	}
	if verdict.RetryAfterSec <= 0 {
		t.Error("a refusal must tell the caller when to come back; SDKs honour Retry-After")
	}
	if verdict.Reason == "" {
		t.Error("a refusal should explain itself")
	}

	// The rollback: a refused request must leave the counter where it was, or a tenant
	// retrying against a full budget would push its own counter ever higher.
	key := "llmrouter:budget:tokens:tenant-a:2025-06-01"
	if got := fake.value(key); got != 900 {
		t.Errorf("counter = %d after a refusal, want 900: a denied request consumed budget", got)
	}
}

func TestZeroBudgetMeansUnlimited(t *testing.T) {
	t.Parallel()

	e := newEnforcer(newFakeRedis(), time.Now)

	verdict, err := e.Reserve(context.Background(), domain.Tenant{ID: "t"}, 1_000_000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !verdict.Allowed {
		t.Error("a tenant with no configured budget must not be blocked by one")
	}
}

func TestCommitReconcilesAndRecordsSpend(t *testing.T) {
	t.Parallel()

	fake := newFakeRedis()
	clock := fixedClock(time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	e := newEnforcer(fake, clock)
	ctx := context.Background()

	if err := e.Commit(ctx, tenant(10000), domain.Usage{PromptTokens: 100, CompletionTokens: 50}, 0.0025); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tokens, spend, err := e.Usage(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if tokens != 150 {
		t.Errorf("tokens = %d, want 150", tokens)
	}
	// Spend is stored in micro-dollars so the counter stays an integer: a float counter in a
	// shared store accumulates rounding error nobody can reconcile afterwards.
	if spend < 0.00249 || spend > 0.00251 {
		t.Errorf("spend = %v, want ~0.0025", spend)
	}
}

func TestKeysRollOverAtTheDayBoundary(t *testing.T) {
	t.Parallel()

	fake := newFakeRedis()
	day1 := time.Date(2025, 6, 1, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2025, 6, 2, 0, 1, 0, 0, time.UTC)

	now := day1
	e := newEnforcer(fake, func() time.Time { return now })
	ctx := context.Background()

	if _, err := e.Reserve(ctx, tenant(1000), 900); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	now = day2
	verdict, err := e.Reserve(ctx, tenant(1000), 900)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !verdict.Allowed {
		t.Error("the budget must reset at the UTC day boundary")
	}
	if verdict.UsedTokens != 900 {
		t.Errorf("used = %d, want 900 on a fresh day", verdict.UsedTokens)
	}
}

func TestRetryAfterShrinksTowardsMidnight(t *testing.T) {
	t.Parallel()

	late := time.Date(2025, 6, 1, 23, 30, 0, 0, time.UTC)
	e := newEnforcer(newFakeRedis(), fixedClock(late))
	ctx := context.Background()

	if _, err := e.Reserve(ctx, tenant(100), 100); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	verdict, err := e.Reserve(ctx, tenant(100), 100)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("expected a refusal")
	}
	// Half an hour to midnight, so Retry-After should be about 1800 seconds rather than a day.
	if verdict.RetryAfterSec > 1900 || verdict.RetryAfterSec < 1700 {
		t.Errorf("RetryAfterSec = %d, want roughly 1800", verdict.RetryAfterSec)
	}
}

func TestPing(t *testing.T) {
	t.Parallel()

	if err := newEnforcer(newFakeRedis(), time.Now).Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestNewRejectsAnInvalidURL(t *testing.T) {
	t.Parallel()

	if _, err := budget.New(budget.Options{RedisURL: "not a url"}); err == nil {
		t.Error("expected an error for a malformed Redis URL")
	}
}
