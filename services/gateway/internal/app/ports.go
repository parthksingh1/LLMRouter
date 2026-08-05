// Package app holds the gateway's use cases and the ports they depend on.
//
// Following the "interfaces are defined by their consumer" rule, every port below is declared
// here rather than alongside its implementation. internal/providers, internal/cache,
// internal/guardrails, internal/budget and internal/events supply adapters; none of them import
// each other, and none of them import this package's concrete services.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Provider is one upstream LLM vendor.
//
// Implementations must respect ctx cancellation, and ChatStream must close the returned channel
// exactly once. A terminal error is delivered as a StreamChunk with Err set rather than by
// closing the channel silently, so the caller can distinguish "finished" from "broke".
type Provider interface {
	Name() string
	Models() []domain.ModelDescriptor
	Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error)
	ChatStream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamChunk, error)
	Embed(ctx context.Context, req domain.EmbedRequest) (domain.EmbedResponse, error)
	HealthCheck(ctx context.Context) error
}

// ProviderRegistry resolves provider names to adapters and tracks their health.
type ProviderRegistry interface {
	Get(name string) (Provider, bool)
	Names() []string
	Healthy(name string) bool
	// Available reports whether a provider may be dispatched to right now: healthy and with a
	// closed circuit breaker.
	Available(name string) bool
}

// Router chooses which provider and model serve a request.
type Router interface {
	Route(ctx context.Context, req domain.ChatRequest, t domain.Tenant) (domain.RouteDecision, error)
	// Fallbacks returns the ordered alternatives to try if the primary decision fails. The
	// slice is already filtered to healthy providers and tenant-allowed models.
	Fallbacks(ctx context.Context, req domain.ChatRequest, t domain.Tenant, primary domain.RouteDecision) []domain.RouteDecision
}

// SemanticCache serves and stores responses keyed by prompt meaning.
//
// Lookup returning (_, false, nil) is an ordinary miss. A non-nil error means the cache itself
// is broken; callers treat that as a miss but record it as a degraded dependency.
type SemanticCache interface {
	Lookup(ctx context.Context, key domain.CacheKey, prompt string) (domain.CachedResponse, bool, error)
	Store(ctx context.Context, key domain.CacheKey, prompt string, resp domain.ChatResponse) error
}

// Guardrails screens prompts and completions.
type Guardrails interface {
	ScreenInput(ctx context.Context, tenantID, text string) (domain.ScreenResult, error)
	ScreenOutput(ctx context.Context, tenantID, text string) (domain.ScreenResult, error)
}

// Budget enforces per-tenant spend limits.
//
// Reserve is called before dispatch with an estimate; Commit is called after with the real
// usage, reconciling the difference. Both are atomic with respect to concurrent requests.
type Budget interface {
	Reserve(ctx context.Context, t domain.Tenant, estTokens int) (domain.BudgetVerdict, error)
	Commit(ctx context.Context, t domain.Tenant, usage domain.Usage, costUSD float64) error
}

// EventSink receives one analytics record per logical request.
//
// Emit must not block the hot path: implementations buffer and flush asynchronously, dropping
// with a metric rather than applying backpressure to user requests.
type EventSink interface {
	Emit(ctx context.Context, e domain.RequestEvent)
}

// Clock is injected so tests can control time without sleeping or reading the wall clock.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// --- errors ------------------------------------------------------------------

// Sentinel errors that the HTTP layer maps onto OpenAI-compatible status codes.
var (
	// ErrNoProviderAvailable means every candidate provider was unhealthy or breaker-open.
	ErrNoProviderAvailable = errors.New("no provider available")
	// ErrModelNotFound means the requested model is not in the catalogue.
	ErrModelNotFound = errors.New("model not found")
	// ErrModelNotAllowed means the model exists but the tenant may not use it.
	ErrModelNotAllowed = errors.New("model not allowed for tenant")
	// ErrBudgetExceeded means the tenant is over its daily token budget.
	ErrBudgetExceeded = errors.New("tenant budget exceeded")
	// ErrGuardrailBlocked means input screening refused the request.
	ErrGuardrailBlocked = errors.New("blocked by guardrail policy")
	// ErrUpstream means every attempt, including fallbacks, failed.
	ErrUpstream = errors.New("upstream provider failure")
	// ErrPolicyNotFound means a requested routing policy does not exist.
	ErrPolicyNotFound = errors.New("routing policy not found")
)

// GuardrailError carries the findings that caused a block, so the HTTP layer can report which
// policy fired without the caller having to guess.
type GuardrailError struct {
	Findings []domain.Finding
}

func (e *GuardrailError) Error() string {
	cats := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		cats = append(cats, f.OWASP+"/"+f.Category)
	}
	return fmt.Sprintf("blocked by guardrail policy: %v", cats)
}

// Unwrap lets errors.Is(err, ErrGuardrailBlocked) succeed.
func (e *GuardrailError) Unwrap() error { return ErrGuardrailBlocked }

// BudgetError carries the verdict that caused a 429.
type BudgetError struct {
	Verdict domain.BudgetVerdict
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("tenant budget exceeded: %d/%d tokens used",
		e.Verdict.UsedTokens, e.Verdict.LimitTokens)
}

// Unwrap lets errors.Is(err, ErrBudgetExceeded) succeed.
func (e *BudgetError) Unwrap() error { return ErrBudgetExceeded }

// UpstreamError records every attempt that failed, which is what makes a failed request
// debuggable from a single log line.
type UpstreamError struct {
	Attempts []AttemptFailure
}

// AttemptFailure is one failed provider attempt.
type AttemptFailure struct {
	Provider string
	Model    string
	Err      error
}

func (e *UpstreamError) Error() string {
	if len(e.Attempts) == 0 {
		return "upstream provider failure"
	}
	s := "upstream provider failure after " + fmt.Sprint(len(e.Attempts)) + " attempt(s):"
	for _, a := range e.Attempts {
		s += fmt.Sprintf(" [%s/%s: %v]", a.Provider, a.Model, a.Err)
	}
	return s
}

// Unwrap lets errors.Is(err, ErrUpstream) succeed.
func (e *UpstreamError) Unwrap() error { return ErrUpstream }
