// Package events writes one analytics record per request to ClickHouse.
//
// The design constraint that shapes everything here: this must never slow down or fail a user
// request. Analytics are valuable, but not as valuable as the response the customer is waiting
// for. So Emit is non-blocking, the flush happens on a background goroutine, and a full buffer
// drops rather than applying backpressure -- with a counter, so a drop is visible rather than
// silent.
//
// Rows are batched because ClickHouse strongly prefers few large inserts to many small ones:
// every insert creates a part, and a part per request would leave the merge scheduler doing
// more work than the queries.
package events

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// columns is the insert column list. It must match infra/clickhouse/init.sql and the TSV that
// seed/generate_traffic.py writes, so the seeded history and the live traffic are the same shape.
var columns = []string{
	"ts", "request_id", "tenant_id", "policy", "model", "provider",
	"prompt_tokens", "completion_tokens", "cached", "cache_similarity",
	"guardrail_blocked", "guardrail_findings", "failover_count",
	"status_code", "latency_ms", "cost_usd",
}

// Dropped is the metric the sink reports when it discards an event.
type Dropped interface {
	EventDropped()
}

// ClickHouse is a batching, non-blocking event sink. It satisfies app.EventSink.
type ClickHouse struct {
	url      string
	database string
	user     string
	password string
	client   *http.Client

	batchSize int
	interval  time.Duration
	log       *slog.Logger
	dropped   Dropped

	queue chan domain.RequestEvent
	wg    sync.WaitGroup
	stop  context.CancelFunc

	// closed guards against Emit racing with Close.
	closed sync.Once
}

var _ app.EventSink = (*ClickHouse)(nil)

// Options configures the sink.
type Options struct {
	URL       string
	Database  string
	User      string
	Password  string
	BatchSize int
	Interval  time.Duration
	// QueueSize bounds memory. Roughly BatchSize * 20 is a sensible default: enough to absorb a
	// burst, small enough that a ClickHouse outage cannot exhaust the heap.
	QueueSize int
	Log       *slog.Logger
	Dropped   Dropped
}

// New builds the sink and starts its flusher.
func New(opts Options) *ClickHouse {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = opts.BatchSize * 20
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Database == "" {
		opts.Database = "llmrouter"
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &ClickHouse{
		url:       strings.TrimSuffix(opts.URL, "/"),
		database:  opts.Database,
		user:      opts.User,
		password:  opts.Password,
		batchSize: opts.BatchSize,
		interval:  opts.Interval,
		log:       opts.Log,
		dropped:   opts.Dropped,
		queue:     make(chan domain.RequestEvent, opts.QueueSize),
		stop:      cancel,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}

	c.wg.Add(1)
	go c.run(ctx)
	return c
}

// Emit queues an event. It never blocks.
//
// A full queue means ClickHouse is down or too slow. Dropping is the right answer: the
// alternative is to block a user's request on an analytics write, which trades a
// nice-to-have for the thing the customer is actually paying for.
func (c *ClickHouse) Emit(_ context.Context, e domain.RequestEvent) {
	select {
	case c.queue <- e:
	default:
		if c.dropped != nil {
			c.dropped.EventDropped()
		}
	}
}

// run batches events and flushes them.
func (c *ClickHouse) run(ctx context.Context) {
	defer c.wg.Done()

	batch := make([]domain.RequestEvent, 0, c.batchSize)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		if err := c.insert(batch); err != nil {
			// A failed insert loses the batch. Retrying would need a durable buffer, which is
			// a real design decision rather than an oversight: the correct fix at scale is to
			// write to Kafka and let a consumer own delivery. Documented in ADR-0004.
			c.log.Warn("dropping analytics batch",
				"events", len(batch), "reason", reason, "error", err)
			if c.dropped != nil {
				for range batch {
					c.dropped.EventDropped()
				}
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is queued so a graceful shutdown does not lose the last few
			// seconds of traffic.
			for {
				select {
				case e := <-c.queue:
					batch = append(batch, e)
					if len(batch) >= c.batchSize {
						flush("shutdown")
					}
				default:
					flush("shutdown")
					return
				}
			}

		case e := <-c.queue:
			batch = append(batch, e)
			if len(batch) >= c.batchSize {
				flush("batch full")
			}

		case <-ticker.C:
			flush("interval")
		}
	}
}

// insert writes a batch as TabSeparated, which is ClickHouse's cheapest text format to parse.
func (c *ClickHouse) insert(batch []domain.RequestEvent) error {
	var body bytes.Buffer
	body.Grow(len(batch) * 160)
	for _, e := range batch {
		writeRow(&body, e)
	}

	query := fmt.Sprintf("INSERT INTO events (%s) FORMAT TabSeparated", strings.Join(columns, ","))
	endpoint := fmt.Sprintf("%s/?database=%s&query=%s",
		c.url, url.QueryEscape(c.database), url.QueryEscape(query))

	req, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("building insert: %w", err)
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to clickhouse: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("clickhouse returned status %d", resp.StatusCode)
	}
	return nil
}

// writeRow serialises one event as a tab-separated line.
//
// Written by hand rather than with encoding/csv because this is on a hot-ish path and the
// escaping rules for TabSeparated are simple enough to be obviously correct: tabs, newlines and
// backslashes are escaped, and nothing else needs quoting.
func writeRow(b *bytes.Buffer, e domain.RequestEvent) {
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now()
	}

	b.WriteString(ts.UTC().Format("2006-01-02 15:04:05.000"))
	b.WriteByte('\t')
	writeEscaped(b, e.RequestID)
	b.WriteByte('\t')
	writeEscaped(b, e.TenantID)
	b.WriteByte('\t')
	writeEscaped(b, e.Policy)
	b.WriteByte('\t')
	writeEscaped(b, e.Model)
	b.WriteByte('\t')
	writeEscaped(b, e.Provider)
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.PromptTokens))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.CompletionTokens))
	b.WriteByte('\t')
	b.WriteString(boolDigit(e.Cached))
	b.WriteByte('\t')
	b.WriteString(strconv.FormatFloat(e.CacheSimilarity, 'f', 4, 64))
	b.WriteByte('\t')
	b.WriteString(boolDigit(e.GuardrailBlocked))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.GuardrailFindings))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.FailoverCount))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.StatusCode))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.LatencyMS))
	b.WriteByte('\t')
	b.WriteString(strconv.FormatFloat(e.CostUSD, 'f', 8, 64))
	b.WriteByte('\n')
}

// writeEscaped applies TabSeparated escaping. A prompt id containing a tab would otherwise
// shift every subsequent column, which corrupts the row silently rather than failing.
func writeEscaped(b *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(s[i])
		}
	}
}

func boolDigit(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// Ping checks connectivity, for readiness reporting.
func (c *ClickHouse) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/ping", nil)
	if err != nil {
		return fmt.Errorf("building ping: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse ping: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("clickhouse ping: status %d", resp.StatusCode)
	}
	return nil
}

// Close stops the flusher and drains the queue.
func (c *ClickHouse) Close() error {
	c.closed.Do(func() {
		c.stop()
		c.wg.Wait()
	})
	return nil
}
