package stream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/stream"
)

// --- doubles -----------------------------------------------------------------

// scriptedProvider emits a fixed sequence of chunks, so a test can describe exactly how an
// upstream misbehaves.
type scriptedProvider struct {
	name string

	mu     sync.Mutex
	calls  int
	seen   []domain.ChatRequest
	script func(call int, req domain.ChatRequest) []domain.StreamChunk
	// openErr fails before any chunk, simulating a connection that never establishes.
	openErr error
	// silent makes the stream produce nothing at all, so the stall timer is what notices.
	silent bool
}

func (p *scriptedProvider) Name() string                      { return p.name }
func (p *scriptedProvider) Models() []domain.ModelDescriptor  { return nil }
func (p *scriptedProvider) HealthCheck(context.Context) error { return nil }

func (p *scriptedProvider) Chat(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
	return domain.ChatResponse{}, errors.New("not used")
}

func (p *scriptedProvider) Embed(context.Context, domain.EmbedRequest) (domain.EmbedResponse, error) {
	return domain.EmbedResponse{}, errors.New("not used")
}

func (p *scriptedProvider) ChatStream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.seen = append(p.seen, req)
	p.mu.Unlock()

	if p.openErr != nil {
		return nil, p.openErr
	}

	out := make(chan domain.StreamChunk)
	go func() {
		defer close(out)
		if p.silent {
			<-ctx.Done()
			return
		}
		for _, chunk := range p.script(call, req) {
			select {
			case <-ctx.Done():
				return
			case out <- chunk:
			}
		}
	}()
	return out, nil
}

func (p *scriptedProvider) lastRequest() (domain.ChatRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		return domain.ChatRequest{}, false
	}
	return p.seen[len(p.seen)-1], true
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// deltas is a convenience script producing text then a clean finish.
func deltas(words []string, usage domain.Usage) func(int, domain.ChatRequest) []domain.StreamChunk {
	return func(int, domain.ChatRequest) []domain.StreamChunk {
		chunks := make([]domain.StreamChunk, 0, len(words)+1)
		for _, w := range words {
			chunks = append(chunks, domain.StreamChunk{Delta: w})
		}
		finish := "stop"
		u := usage
		chunks = append(chunks, domain.StreamChunk{FinishReason: &finish, Usage: &u})
		return chunks
	}
}

// breaksAfter emits n words then a terminal error, which is the mid-stream failure case.
func breaksAfter(words []string, n int, err error) func(int, domain.ChatRequest) []domain.StreamChunk {
	return func(int, domain.ChatRequest) []domain.StreamChunk {
		chunks := make([]domain.StreamChunk, 0, n+1)
		for i := 0; i < n && i < len(words); i++ {
			chunks = append(chunks, domain.StreamChunk{Delta: words[i]})
		}
		return append(chunks, domain.StreamChunk{Err: err})
	}
}

type testRegistry struct {
	providers map[string]app.Provider
	specs     map[string]config.ProviderSpec
}

func (r *testRegistry) Get(name string) (app.Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

func (r *testRegistry) Spec(name string) (config.ProviderSpec, bool) {
	s, ok := r.specs[name]
	return s, ok
}

// recordingSink captures what a client would have received.
type recordingSink struct {
	text      strings.Builder
	failovers []stream.FailoverEvent
	summary   stream.Summary
	done      bool
	failAt    int // fail the Nth Delta, simulating a client disconnect
	deltas    int
}

func (s *recordingSink) Delta(text string) error {
	s.deltas++
	if s.failAt > 0 && s.deltas >= s.failAt {
		return errors.New("client disconnected")
	}
	s.text.WriteString(text)
	return nil
}

func (s *recordingSink) Failover(event stream.FailoverEvent) error {
	s.failovers = append(s.failovers, event)
	return nil
}

func (s *recordingSink) Done(summary stream.Summary) error {
	s.summary = summary
	s.done = true
	return nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func spec(name string, mode config.PrefixMode) config.ProviderSpec {
	return config.ProviderSpec{Name: name, PrefixContinuation: mode}
}

func decision(provider, model string) domain.RouteDecision {
	return domain.RouteDecision{
		Provider: provider,
		Policy:   "test",
		Model:    domain.ModelDescriptor{ID: model, Provider: provider, PriceInPerM: 1, PriceOutPerM: 1},
	}
}

func request() domain.ChatRequest {
	return domain.ChatRequest{
		RequestID: "req-1", TenantID: "t", RequestedModel: "auto", Stream: true,
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "explain consistent hashing"}},
	}
}

func newRunner(t *testing.T, reg *testRegistry, stall time.Duration) *stream.Runner {
	t.Helper()
	r, err := stream.NewRunner(stream.Options{
		Registry: reg, StallTimeout: stall, MaxAttempts: 3, Log: quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// --- tests -------------------------------------------------------------------

func TestCleanStreamNeedsNoFailover(t *testing.T) {
	t.Parallel()

	p := &scriptedProvider{
		name:   "openai",
		script: deltas([]string{"Consistent ", "hashing ", "works."}, domain.Usage{PromptTokens: 10, CompletionTokens: 3}),
	}
	reg := &testRegistry{
		providers: map[string]app.Provider{"openai": p},
		specs:     map[string]config.ProviderSpec{"openai": spec("openai", config.PrefixPrefill)},
	}
	sink := &recordingSink{}

	result, err := newRunner(t, reg, time.Second).Run(
		context.Background(), request(), decision("openai", "gpt-4o"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sink.text.String() != "Consistent hashing works." {
		t.Errorf("client received %q", sink.text.String())
	}
	if len(sink.failovers) != 0 {
		t.Errorf("unexpected failovers: %+v", sink.failovers)
	}
	if !sink.done || sink.summary.FinishReason != "stop" {
		t.Errorf("stream did not terminate cleanly: %+v", sink.summary)
	}
	if result.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", result.Usage)
	}
}

func TestMidStreamFailoverContinuesFromThePrefix(t *testing.T) {
	t.Parallel()

	primary := &scriptedProvider{
		name:   "anthropic",
		script: breaksAfter([]string{"Consistent ", "hashing "}, 2, errors.New("upstream stream reset")),
	}
	fallback := &scriptedProvider{
		name:   "openai",
		script: deltas([]string{"maps ", "keys ", "to ", "nodes."}, domain.Usage{PromptTokens: 12, CompletionTokens: 4}),
	}
	reg := &testRegistry{
		providers: map[string]app.Provider{"anthropic": primary, "openai": fallback},
		specs: map[string]config.ProviderSpec{
			"anthropic": spec("anthropic", config.PrefixNative),
			"openai":    spec("openai", config.PrefixPrefill),
		},
	}
	sink := &recordingSink{}

	result, err := newRunner(t, reg, time.Second).Run(
		context.Background(), request(),
		decision("anthropic", "claude-3-5-sonnet"),
		[]domain.RouteDecision{decision("openai", "gpt-4o")},
		sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The client sees one continuous answer: the tokens from before the break are NOT resent,
	// and the tokens after it continue from them.
	if got := sink.text.String(); got != "Consistent hashing maps keys to nodes." {
		t.Errorf("client received %q; the seam should be invisible", got)
	}

	if len(sink.failovers) != 1 {
		t.Fatalf("expected exactly one failover event, got %d", len(sink.failovers))
	}
	event := sink.failovers[0]
	if event.From != "anthropic" || event.To != "openai" {
		t.Errorf("failover event = %+v", event)
	}
	if event.Mode != "continue" {
		t.Errorf("mode = %q, want continue for a prefix-capable provider", event.Mode)
	}
	if event.Restarted {
		t.Error("a continuing failover must not be reported as a restart")
	}
	if event.TokensPreserved == 0 {
		t.Error("the event should report how much of the answer survived")
	}

	// The fallback must have been asked to CONTINUE, which means a trailing assistant message
	// carrying exactly what the client has already seen.
	req, ok := fallback.lastRequest()
	if !ok {
		t.Fatal("the fallback was never called")
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != domain.RoleAssistant {
		t.Fatalf("the continuation request must end with an assistant message, got %q", last.Role)
	}
	if last.Content != "Consistent hashing " {
		t.Errorf("prefix = %q, want exactly what the client had received", last.Content)
	}

	if len(result.Path) != 2 {
		t.Errorf("path = %v, want both attempts recorded", result.Path)
	}
}

func TestFailoverToAProviderThatCannotContinueRestartsAndSaysSo(t *testing.T) {
	t.Parallel()

	primary := &scriptedProvider{
		name:   "anthropic",
		script: breaksAfter([]string{"Partial ", "answer "}, 2, errors.New("upstream stream reset")),
	}
	fallback := &scriptedProvider{
		name:   "google",
		script: deltas([]string{"A ", "fresh ", "answer."}, domain.Usage{CompletionTokens: 3}),
	}
	reg := &testRegistry{
		providers: map[string]app.Provider{"anthropic": primary, "google": fallback},
		specs: map[string]config.ProviderSpec{
			"anthropic": spec("anthropic", config.PrefixNative),
			// Gemini cannot continue a partial assistant turn.
			"google": spec("google", config.PrefixNone),
		},
	}
	sink := &recordingSink{}

	result, err := newRunner(t, reg, time.Second).Run(
		context.Background(), request(),
		decision("anthropic", "claude-3-5-sonnet"),
		[]domain.RouteDecision{decision("google", "gemini-1.5-pro")},
		sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	event := sink.failovers[0]
	if event.Mode != "restart" {
		t.Errorf("mode = %q, want restart", event.Mode)
	}
	// The discontinuity is reported rather than hidden. The client's transcript genuinely
	// contains two partial answers, and pretending otherwise would be dishonest.
	if !event.Restarted {
		t.Error("a restart must be flagged so the client can discard the earlier text")
	}
	if !result.Restarted {
		t.Error("the result must report that a restart happened, so the answer is not cached")
	}

	// The fallback must NOT have been given an assistant prefix it cannot use.
	req, _ := fallback.lastRequest()
	if req.Messages[len(req.Messages)-1].Role == domain.RoleAssistant {
		t.Error("a provider that cannot continue must not receive a prefix")
	}
}

func TestStallIsDetectedAndFailedOver(t *testing.T) {
	t.Parallel()

	// A provider that accepts the request and then goes quiet. This is the most common real
	// failure and the one a plain read loop never notices: the socket stays open forever.
	primary := &scriptedProvider{name: "openai", silent: true}
	fallback := &scriptedProvider{
		name:   "anthropic",
		script: deltas([]string{"Recovered."}, domain.Usage{CompletionTokens: 1}),
	}
	reg := &testRegistry{
		providers: map[string]app.Provider{"openai": primary, "anthropic": fallback},
		specs: map[string]config.ProviderSpec{
			"openai":    spec("openai", config.PrefixPrefill),
			"anthropic": spec("anthropic", config.PrefixNative),
		},
	}
	sink := &recordingSink{}

	started := time.Now()
	_, err := newRunner(t, reg, 80*time.Millisecond).Run(
		context.Background(), request(),
		decision("openai", "gpt-4o"),
		[]domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet")},
		sink)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.text.String() != "Recovered." {
		t.Errorf("client received %q", sink.text.String())
	}
	if len(sink.failovers) != 1 || sink.failovers[0].Reason != "stalled" {
		t.Errorf("expected a stall-triggered failover, got %+v", sink.failovers)
	}
	// It must not have waited anywhere near forever.
	if elapsed > 2*time.Second {
		t.Errorf("stall detection took %s; the timer is not working", elapsed)
	}
}

func TestUsageIsSummedAcrossAttempts(t *testing.T) {
	t.Parallel()

	// The first provider reports usage before breaking. Those tokens were really generated and
	// really cost money, so dropping them would under-bill.
	primary := &scriptedProvider{
		name: "anthropic",
		script: func(int, domain.ChatRequest) []domain.StreamChunk {
			u := domain.Usage{PromptTokens: 10, CompletionTokens: 5}
			return []domain.StreamChunk{
				{Delta: "Half "},
				{Usage: &u},
				{Err: errors.New("upstream stream reset")},
			}
		},
	}
	fallback := &scriptedProvider{
		name:   "openai",
		script: deltas([]string{"an ", "answer."}, domain.Usage{PromptTokens: 12, CompletionTokens: 2}),
	}
	reg := &testRegistry{
		providers: map[string]app.Provider{"anthropic": primary, "openai": fallback},
		specs: map[string]config.ProviderSpec{
			"anthropic": spec("anthropic", config.PrefixNative),
			"openai":    spec("openai", config.PrefixPrefill),
		},
	}
	sink := &recordingSink{}

	result, err := newRunner(t, reg, time.Second).Run(
		context.Background(), request(),
		decision("anthropic", "claude-3-5-sonnet"),
		[]domain.RouteDecision{decision("openai", "gpt-4o")},
		sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Usage.PromptTokens != 22 || result.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v, want {22 7}: every attempt's tokens were really generated",
			result.Usage)
	}
	if sink.summary.Usage != result.Usage {
		t.Error("the terminating frame must report the same summed usage")
	}
}

func TestChainExhaustionReportsEveryAttempt(t *testing.T) {
	t.Parallel()

	broken := func(name string) *scriptedProvider {
		return &scriptedProvider{
			name:   name,
			script: breaksAfter([]string{"x "}, 1, errors.New("upstream stream reset")),
		}
	}
	a, b := broken("openai"), broken("anthropic")
	reg := &testRegistry{
		providers: map[string]app.Provider{"openai": a, "anthropic": b},
		specs: map[string]config.ProviderSpec{
			"openai":    spec("openai", config.PrefixPrefill),
			"anthropic": spec("anthropic", config.PrefixNative),
		},
	}
	sink := &recordingSink{}

	_, err := newRunner(t, reg, time.Second).Run(
		context.Background(), request(),
		decision("openai", "gpt-4o"),
		[]domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet")},
		sink)

	if !errors.Is(err, stream.ErrExhausted) {
		t.Fatalf("error = %v, want ErrExhausted", err)
	}
	if a.callCount() != 1 || b.callCount() != 1 {
		t.Errorf("expected one attempt each, got %d and %d", a.callCount(), b.callCount())
	}
	// The tokens that did arrive stay with the client: truncated is better than nothing, and
	// the caller is told the stream failed.
	if sink.text.Len() == 0 {
		t.Error("tokens delivered before the break should not be withdrawn")
	}
}

func TestClientDisconnectStopsTheChain(t *testing.T) {
	t.Parallel()

	primary := &scriptedProvider{
		name:   "openai",
		script: deltas([]string{"one ", "two ", "three "}, domain.Usage{}),
	}
	fallback := &scriptedProvider{name: "anthropic", script: deltas([]string{"x"}, domain.Usage{})}
	reg := &testRegistry{
		providers: map[string]app.Provider{"openai": primary, "anthropic": fallback},
		specs: map[string]config.ProviderSpec{
			"openai":    spec("openai", config.PrefixPrefill),
			"anthropic": spec("anthropic", config.PrefixNative),
		},
	}
	// The sink fails on the second delta, as a disconnected client's socket would.
	sink := &recordingSink{failAt: 2}

	_, err := newRunner(t, reg, time.Second).Run(
		context.Background(), request(),
		decision("openai", "gpt-4o"),
		[]domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet")},
		sink)

	if err == nil {
		t.Fatal("expected an error when the client goes away")
	}
	// Failing over for a client that has hung up burns a provider call for nobody. The chain
	// does advance here (the runner cannot distinguish a dead client from a dead provider at
	// the sink), but it must not loop forever.
	if fallback.callCount() > 1 {
		t.Errorf("fallback called %d times after a client disconnect", fallback.callCount())
	}
}

func TestContextCancellationEndsTheStream(t *testing.T) {
	t.Parallel()

	p := &scriptedProvider{name: "openai", silent: true}
	reg := &testRegistry{
		providers: map[string]app.Provider{"openai": p},
		specs:     map[string]config.ProviderSpec{"openai": spec("openai", config.PrefixPrefill)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		_, _ = newRunner(t, reg, 10*time.Second).Run(
			ctx, request(), decision("openai", "gpt-4o"), nil, &recordingSink{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the runner ignored context cancellation")
	}
}

func TestMaxAttemptsCapsTheChain(t *testing.T) {
	t.Parallel()

	broken := func(name string) *scriptedProvider {
		return &scriptedProvider{name: name, script: breaksAfter([]string{"x "}, 1, errors.New("reset"))}
	}
	a, b, c := broken("openai"), broken("anthropic"), broken("google")
	reg := &testRegistry{
		providers: map[string]app.Provider{"openai": a, "anthropic": b, "google": c},
		specs: map[string]config.ProviderSpec{
			"openai":    spec("openai", config.PrefixPrefill),
			"anthropic": spec("anthropic", config.PrefixNative),
			"google":    spec("google", config.PrefixNone),
		},
	}

	runner, err := stream.NewRunner(stream.Options{
		Registry: reg, StallTimeout: time.Second, MaxAttempts: 2, Log: quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _ = runner.Run(context.Background(), request(),
		decision("openai", "gpt-4o"),
		[]domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet"), decision("google", "gemini-1.5-pro")},
		&recordingSink{})

	if c.callCount() != 0 {
		t.Error("max_attempts=2 must stop before the third provider")
	}
}

func TestNewRunnerRequiresARegistry(t *testing.T) {
	t.Parallel()

	if _, err := stream.NewRunner(stream.Options{}); err == nil {
		t.Error("a runner with no registry cannot dispatch and must not be constructed")
	}
}
