package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// --- doubles -----------------------------------------------------------------

type fakeProvider struct {
	name string

	mu       sync.Mutex
	calls    int
	failWith error
	// failFirstN makes the first N calls fail, so failover and recovery can both be exercised.
	failFirstN int
	content    string
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Models() []domain.ModelDescriptor { return nil }

func (p *fakeProvider) Chat(_ context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++

	if p.failWith != nil && (p.failFirstN == 0 || p.calls <= p.failFirstN) {
		return domain.ChatResponse{}, p.failWith
	}
	content := p.content
	if content == "" {
		content = "answer from " + p.name
	}
	return domain.ChatResponse{
		ID:           "resp-" + p.name,
		Model:        req.RequestedModel,
		Provider:     p.name,
		Content:      content,
		FinishReason: "stop",
		Usage:        domain.Usage{PromptTokens: 100, CompletionTokens: 50},
	}, nil
}

func (p *fakeProvider) ChatStream(context.Context, domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk)
	close(ch)
	return ch, nil
}

func (p *fakeProvider) Embed(context.Context, domain.EmbedRequest) (domain.EmbedResponse, error) {
	return domain.EmbedResponse{}, nil
}

func (p *fakeProvider) HealthCheck(context.Context) error { return nil }

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeRegistry struct {
	providers map[string]app.Provider
}

func (r *fakeRegistry) Get(name string) (app.Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

func (r *fakeRegistry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}

func (r *fakeRegistry) Healthy(string) bool   { return true }
func (r *fakeRegistry) Available(string) bool { return true }

type fakeRouter struct {
	primary   domain.RouteDecision
	fallbacks []domain.RouteDecision
	err       error
}

func (r *fakeRouter) Route(context.Context, domain.ChatRequest, domain.Tenant) (domain.RouteDecision, error) {
	if r.err != nil {
		return domain.RouteDecision{}, r.err
	}
	return r.primary, nil
}

func (r *fakeRouter) Fallbacks(context.Context, domain.ChatRequest, domain.Tenant, domain.RouteDecision) []domain.RouteDecision {
	return r.fallbacks
}

type fakeCache struct {
	mu        sync.Mutex
	hit       bool
	cached    domain.CachedResponse
	lookupErr error
	stored    []string
	// storedPrompt records the prompt as the cache saw it, which is how the redaction test
	// proves that unredacted text never reaches the cache.
	storedPrompt string
}

func (c *fakeCache) Lookup(_ context.Context, _ domain.CacheKey, _ string) (domain.CachedResponse, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lookupErr != nil {
		return domain.CachedResponse{}, false, c.lookupErr
	}
	return c.cached, c.hit, nil
}

func (c *fakeCache) Store(_ context.Context, _ domain.CacheKey, prompt string, resp domain.ChatResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = append(c.stored, resp.Content)
	c.storedPrompt = prompt
	return nil
}

type fakeGuardrails struct {
	result domain.ScreenResult
	err    error
	// seen records the text that was screened.
	seen string
}

func (g *fakeGuardrails) ScreenInput(_ context.Context, _, text string) (domain.ScreenResult, error) {
	g.seen = text
	if g.err != nil {
		return domain.ScreenResult{}, g.err
	}
	return g.result, nil
}

func (g *fakeGuardrails) ScreenOutput(context.Context, string, string) (domain.ScreenResult, error) {
	return domain.ScreenResult{Allowed: true}, nil
}

type fakeBudget struct {
	verdict   domain.BudgetVerdict
	err       error
	committed []domain.Usage
	reserved  int
}

func (b *fakeBudget) Reserve(_ context.Context, _ domain.Tenant, est int) (domain.BudgetVerdict, error) {
	b.reserved = est
	if b.err != nil {
		return domain.BudgetVerdict{}, b.err
	}
	return b.verdict, nil
}

func (b *fakeBudget) Commit(_ context.Context, _ domain.Tenant, u domain.Usage, _ float64) error {
	b.committed = append(b.committed, u)
	return nil
}

type fakeEvents struct {
	mu     sync.Mutex
	events []domain.RequestEvent
}

func (e *fakeEvents) Emit(_ context.Context, ev domain.RequestEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *fakeEvents) last() (domain.RequestEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) == 0 {
		return domain.RequestEvent{}, false
	}
	return e.events[len(e.events)-1], true
}

// retryableError lets a test say whether failover should advance.
type retryableError struct {
	msg       string
	retryable bool
}

func (e *retryableError) Error() string   { return e.msg }
func (e *retryableError) Retryable() bool { return e.retryable }

// --- helpers -----------------------------------------------------------------

func decision(provider, model string, quality, priceIn, priceOut float64) domain.RouteDecision {
	return domain.RouteDecision{
		Provider: provider,
		Policy:   "test",
		Model: domain.ModelDescriptor{
			ID: model, Provider: provider, Quality: quality,
			PriceInPerM: priceIn, PriceOutPerM: priceOut,
		},
	}
}

func testRequest() domain.ChatRequest {
	return domain.ChatRequest{
		RequestID:      "req-1",
		TenantID:       "tenant-a",
		RequestedModel: "auto",
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: "Be helpful."},
			{Role: domain.RoleUser, Content: "what is the capital of France"},
		},
	}
}

func testTenant() domain.Tenant {
	return domain.Tenant{ID: "tenant-a", AllowedModels: []string{"*"}, DailyTokenBudget: 10000}
}

func newService(t *testing.T, deps app.ChatServiceDeps) *app.ChatService {
	t.Helper()
	if deps.Log == nil {
		deps.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	svc, err := app.NewChatService(deps)
	if err != nil {
		t.Fatalf("NewChatService: %v", err)
	}
	return svc
}

// --- tests -------------------------------------------------------------------

func TestNewChatServiceRequiresItsMandatoryCollaborators(t *testing.T) {
	t.Parallel()

	if _, err := app.NewChatService(app.ChatServiceDeps{Registry: &fakeRegistry{}}); err == nil {
		t.Error("expected an error when no router is supplied")
	}
	if _, err := app.NewChatService(app.ChatServiceDeps{Router: &fakeRouter{}}); err == nil {
		t.Error("expected an error when no registry is supplied")
	}
}

func TestCompleteHappyPath(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	events := &fakeEvents{}
	budget := &fakeBudget{verdict: domain.BudgetVerdict{Allowed: true}}
	cache := &fakeCache{}

	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Cache:    cache,
		Budget:   budget,
		Events:   events,
	})

	res, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if res.Response.Content != "answer from openai" {
		t.Errorf("content = %q", res.Response.Content)
	}
	if res.Cached {
		t.Error("this was a cache miss and must not be reported as a hit")
	}
	// 100 prompt tokens at $0.15/M plus 50 completion tokens at $0.60/M.
	wantCost := 100*0.15/1e6 + 50*0.60/1e6
	if diff := res.CostUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %.10f, want %.10f", res.CostUSD, wantCost)
	}
	if len(cache.stored) != 1 {
		t.Errorf("expected one write-through cache store, got %d", len(cache.stored))
	}
	if len(budget.committed) != 1 {
		t.Errorf("expected the real usage to be committed to the budget, got %d commits", len(budget.committed))
	}

	ev, ok := events.last()
	if !ok {
		t.Fatal("no analytics event was emitted")
	}
	if ev.TenantID != "tenant-a" || ev.Model != "gpt-4o-mini" || ev.StatusCode != 200 {
		t.Errorf("event = %+v", ev)
	}
	if ev.PromptTokens != 100 || ev.CompletionTokens != 50 {
		t.Errorf("event token counts = %d/%d, want 100/50", ev.PromptTokens, ev.CompletionTokens)
	}
}

func TestCacheHitShortCircuitsTheProviderAndTheBudget(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	budget := &fakeBudget{verdict: domain.BudgetVerdict{Allowed: false}} // would reject if consulted
	events := &fakeEvents{}

	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Cache: &fakeCache{hit: true, cached: domain.CachedResponse{
			Content: "cached answer", Model: "gpt-4o-mini", Provider: "openai",
			Similarity: 0.94, Usage: domain.Usage{PromptTokens: 100, CompletionTokens: 40},
		}},
		Budget: budget,
		Events: events,
	})

	res, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if !res.Cached || res.Response.Content != "cached answer" {
		t.Fatalf("expected a cache hit, got %+v", res)
	}
	if provider.callCount() != 0 {
		t.Error("a cache hit must not reach the provider")
	}
	// A cached answer costs nothing, so serving it to a tenant that is over budget is correct:
	// refusing would be a worse trade than serving a free response.
	if res.CostUSD != 0 {
		t.Errorf("a cache hit must be free, got $%v", res.CostUSD)
	}
	if res.CacheScore != 0.94 {
		t.Errorf("cache score = %v, want 0.94", res.CacheScore)
	}

	ev, _ := events.last()
	if !ev.Cached || ev.CostUSD != 0 {
		t.Errorf("the analytics event should record a free cache hit: %+v", ev)
	}

	hits, lookups := svc.CacheStats()
	if hits != 1 || lookups != 1 {
		t.Errorf("cache stats = %d/%d, want 1/1", hits, lookups)
	}
}

func TestBrokenCacheDegradesToAMiss(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Cache:    &fakeCache{lookupErr: errors.New("redis is down")},
	})

	res, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err != nil {
		t.Fatalf("a broken cache must not fail the request: %v", err)
	}
	if res.Cached {
		t.Error("a cache error must not be reported as a hit")
	}
	if provider.callCount() != 1 {
		t.Error("the request should have gone to the provider")
	}
}

func TestGuardrailBlockStopsBeforeAnyProviderCall(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	cache := &fakeCache{hit: true, cached: domain.CachedResponse{Content: "cached"}}
	events := &fakeEvents{}

	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Cache:    cache,
		Events:   events,
		Guardrails: &fakeGuardrails{result: domain.ScreenResult{
			Allowed: false,
			Findings: []domain.Finding{
				{Detector: "prompt_injection", Category: "instruction_override", OWASP: "LLM01", Severity: "high"},
			},
		}},
	})

	_, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if !errors.Is(err, app.ErrGuardrailBlocked) {
		t.Fatalf("error = %v, want ErrGuardrailBlocked", err)
	}
	if provider.callCount() != 0 {
		t.Error("a blocked request must not reach a provider")
	}
	// Screening runs before the cache, so a blocked prompt cannot be laundered through a hit.
	ev, ok := events.last()
	if !ok || !ev.GuardrailBlocked {
		t.Errorf("the block should be recorded in analytics: %+v", ev)
	}
	if ev.Cached {
		t.Error("a blocked request must never be reported as a cache hit")
	}
}

func TestRedactionRewritesThePromptBeforeItLeaves(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	cache := &fakeCache{}
	guard := &fakeGuardrails{result: domain.ScreenResult{
		Allowed:      true,
		RedactedText: "my email is [REDACTED]",
		Findings:     []domain.Finding{{Detector: "pii", Category: "email", OWASP: "LLM06", Action: "redact"}},
	}}

	svc := newService(t, app.ChatServiceDeps{
		Router:     &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry:   &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Cache:      cache,
		Guardrails: guard,
	})

	req := testRequest()
	req.Messages[1].Content = "my email is alice@example.com"

	res, err := svc.Complete(context.Background(), req, testTenant())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Redacted {
		t.Error("the result should report that redaction happened")
	}
	// The redacted text, not the original, must be what gets cached: otherwise the PII is
	// simply moved from the provider to our own datastore.
	if cache.storedPrompt != "my email is [REDACTED]" {
		t.Errorf("cache stored %q; the unredacted prompt must never be persisted", cache.storedPrompt)
	}
	if len(res.Findings) != 1 {
		t.Errorf("expected the findings to be surfaced, got %d", len(res.Findings))
	}
}

func TestBudgetRejectionIsA429WithRetryAfter(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Budget: &fakeBudget{verdict: domain.BudgetVerdict{
			Allowed: false, UsedTokens: 12000, LimitTokens: 10000, RetryAfterSec: 3600,
		}},
	})

	_, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if !errors.Is(err, app.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	var be *app.BudgetError
	if !errors.As(err, &be) || be.Verdict.RetryAfterSec != 3600 {
		t.Errorf("the verdict must survive so the handler can set Retry-After: %v", err)
	}
	if provider.callCount() != 0 {
		t.Error("a rejected request must not reach a provider")
	}
}

func TestBudgetWarningDoesNotBlock(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Budget:   &fakeBudget{verdict: domain.BudgetVerdict{Allowed: true, Warn: true}},
	})

	res, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err != nil {
		t.Fatalf("a warning must not block the request: %v", err)
	}
	if !res.BudgetWarn {
		t.Error("the warning should be surfaced so the handler can set a response header")
	}
	if provider.callCount() != 1 {
		t.Error("the request should still have been served")
	}
}

func TestABrokenBudgetStoreDoesNotBlockTraffic(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{name: "openai"}
	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": provider}},
		Budget:   &fakeBudget{err: errors.New("redis is down")},
	})

	// A Redis outage must degrade budget enforcement, not the whole gateway.
	if _, err := svc.Complete(context.Background(), testRequest(), testTenant()); err != nil {
		t.Fatalf("a broken budget store must not fail the request: %v", err)
	}
	if provider.callCount() != 1 {
		t.Error("the request should still have been served")
	}
}

func TestFailoverAdvancesOnRetryableErrors(t *testing.T) {
	t.Parallel()

	primary := &fakeProvider{name: "openai", failWith: &retryableError{msg: "503 from upstream", retryable: true}}
	secondary := &fakeProvider{name: "anthropic", content: "answer from the fallback"}

	svc := newService(t, app.ChatServiceDeps{
		Router: &fakeRouter{
			primary:   decision("openai", "gpt-4o", 0.95, 2.5, 10),
			fallbacks: []domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet", 0.948, 3, 15)},
		},
		Registry: &fakeRegistry{providers: map[string]app.Provider{
			"openai": primary, "anthropic": secondary,
		}},
		MaxAttempts: 3,
	})

	res, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Response.Content != "answer from the fallback" {
		t.Errorf("content = %q, want the fallback's answer", res.Response.Content)
	}
	if res.Decision.Provider != "anthropic" {
		t.Errorf("the reported decision must be the one that served: %q", res.Decision.Provider)
	}
	if len(res.FailoverPath) != 2 {
		t.Errorf("failover path = %v, want both attempts recorded", res.FailoverPath)
	}
	// Cost must be attributed to the model that actually answered, not the one that failed.
	wantCost := 100*3.0/1e6 + 50*15.0/1e6
	if diff := res.CostUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %.10f, want %.10f (the fallback's pricing)", res.CostUSD, wantCost)
	}
}

func TestFailoverStopsOnNonRetryableErrors(t *testing.T) {
	t.Parallel()

	primary := &fakeProvider{name: "openai", failWith: &retryableError{msg: "400 bad request", retryable: false}}
	secondary := &fakeProvider{name: "anthropic"}

	svc := newService(t, app.ChatServiceDeps{
		Router: &fakeRouter{
			primary:   decision("openai", "gpt-4o", 0.95, 2.5, 10),
			fallbacks: []domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet", 0.948, 3, 15)},
		},
		Registry: &fakeRegistry{providers: map[string]app.Provider{
			"openai": primary, "anthropic": secondary,
		}},
		MaxAttempts: 3,
	})

	_, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if !errors.Is(err, app.ErrUpstream) {
		t.Fatalf("error = %v, want ErrUpstream", err)
	}
	// A caller error fails identically everywhere; fanning it out just multiplies latency.
	if secondary.callCount() != 0 {
		t.Error("a non-retryable error must not advance the failover chain")
	}
}

func TestFailoverIsCappedByMaxAttempts(t *testing.T) {
	t.Parallel()

	fail := func(name string) *fakeProvider {
		return &fakeProvider{name: name, failWith: &retryableError{msg: "503", retryable: true}}
	}
	a, b, c := fail("openai"), fail("anthropic"), fail("google")

	svc := newService(t, app.ChatServiceDeps{
		Router: &fakeRouter{
			primary: decision("openai", "gpt-4o", 0.95, 2.5, 10),
			fallbacks: []domain.RouteDecision{
				decision("anthropic", "claude-3-5-sonnet", 0.948, 3, 15),
				decision("google", "gemini-1.5-pro", 0.932, 1.25, 5),
			},
		},
		Registry: &fakeRegistry{providers: map[string]app.Provider{
			"openai": a, "anthropic": b, "google": c,
		}},
		MaxAttempts: 2,
	})

	_, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if !errors.Is(err, app.ErrUpstream) {
		t.Fatalf("error = %v, want ErrUpstream", err)
	}
	if a.callCount() != 1 || b.callCount() != 1 {
		t.Errorf("expected exactly one call each to the first two providers, got %d and %d",
			a.callCount(), b.callCount())
	}
	if c.callCount() != 0 {
		t.Error("max_attempts=2 must stop before the third provider")
	}

	var ue *app.UpstreamError
	if !errors.As(err, &ue) || len(ue.Attempts) != 2 {
		t.Errorf("the error should record every failed attempt for debugging: %v", err)
	}
}

func TestCancelledContextStopsTheChain(t *testing.T) {
	t.Parallel()

	primary := &fakeProvider{name: "openai", failWith: context.Canceled}
	secondary := &fakeProvider{name: "anthropic"}

	svc := newService(t, app.ChatServiceDeps{
		Router: &fakeRouter{
			primary:   decision("openai", "gpt-4o", 0.95, 2.5, 10),
			fallbacks: []domain.RouteDecision{decision("anthropic", "claude-3-5-sonnet", 0.948, 3, 15)},
		},
		Registry: &fakeRegistry{providers: map[string]app.Provider{
			"openai": primary, "anthropic": secondary,
		}},
		MaxAttempts: 3,
	})

	_, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err == nil {
		t.Fatal("expected an error")
	}
	// The client has gone; further attempts are pure waste.
	if secondary.callCount() != 0 {
		t.Error("a cancelled request must not fan out to more providers")
	}
}

func TestRoutingFailureIsSurfaced(t *testing.T) {
	t.Parallel()

	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{err: app.ErrNoProviderAvailable},
		Registry: &fakeRegistry{providers: map[string]app.Provider{}},
	})

	if _, err := svc.Complete(context.Background(), testRequest(), testTenant()); !errors.Is(err, app.ErrNoProviderAvailable) {
		t.Fatalf("error = %v, want ErrNoProviderAvailable", err)
	}
}

func TestBudgetReservationIncludesTheCompletion(t *testing.T) {
	t.Parallel()

	budget := &fakeBudget{verdict: domain.BudgetVerdict{Allowed: true}}
	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": &fakeProvider{name: "openai"}}},
		Budget:   budget,
	})

	req := testRequest()
	maxTokens := 2000
	req.MaxTokens = &maxTokens

	if _, err := svc.Complete(context.Background(), req, testTenant()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Reserving only the prompt would let a tenant blow its budget on output tokens.
	if budget.reserved <= maxTokens {
		t.Errorf("reserved %d tokens, which does not account for the %d-token completion",
			budget.reserved, maxTokens)
	}
}

func TestClockIsInjectable(t *testing.T) {
	t.Parallel()

	svc := newService(t, app.ChatServiceDeps{
		Router:   &fakeRouter{primary: decision("openai", "gpt-4o-mini", 0.86, 0.15, 0.60)},
		Registry: &fakeRegistry{providers: map[string]app.Provider{"openai": &fakeProvider{name: "openai"}}},
		Clock:    stepClock{step: 250 * time.Millisecond},
	})

	res, err := svc.Complete(context.Background(), testRequest(), testTenant())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.LatencyMS <= 0 {
		t.Errorf("latency = %d ms, want a positive measured value from the injected clock", res.LatencyMS)
	}
}

// stepClock advances by a fixed amount on every read, so latency is deterministic in tests.
// It is a value type with no fields of its own beyond the step, because a Clock is copied when
// stored in an interface; the tick counter therefore lives in a package-level monotonic source.
type stepClock struct {
	step time.Duration
}

func (c stepClock) Now() time.Time {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(nextTick()) * c.step)
}

var (
	tickMu sync.Mutex
	ticks  int
)

func nextTick() int {
	tickMu.Lock()
	defer tickMu.Unlock()
	ticks++
	return ticks
}
