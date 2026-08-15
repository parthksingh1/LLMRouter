package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Recorder is the telemetry this use case emits. Declared here so internal/app never imports
// internal/telemetry, and so tests can assert on what was recorded.
type Recorder interface {
	ObserveUpstream(provider, model, outcome string, d time.Duration)
	ObserveCache(result string, similarity, hitRatio float64)
	ObserveGuardrail(d time.Duration, blocked bool, owasp, detector string)
	GuardrailFailedOpen()
	ObserveFailover(from, to, reason string, midstream bool)
	ObserveBudget(tenant, action string)
	ObserveUsage(tenant, provider, model string, promptTokens, completionTokens int, costUSD float64)
}

// ChatServiceDeps is the injected collaborator set.
//
// Cache, Guardrails, Budget and Events are optional: a nil value disables that stage rather than
// panicking. That is what lets the gateway boot and serve when Redis or the guardrails sidecar
// is not yet up, degrading a feature instead of the whole service.
type ChatServiceDeps struct {
	Router     Router
	Registry   ProviderRegistry
	Cache      SemanticCache
	Guardrails Guardrails
	Budget     Budget
	Events     EventSink
	Stream     StreamRunner
	Recorder   Recorder
	Clock      Clock
	Log        *slog.Logger

	// MaxAttempts caps providers tried per logical request, including the first.
	MaxAttempts int
}

// ChatService orchestrates one chat completion.
//
// The ordering of the stages is a deliberate cost/safety trade-off:
//
//	guardrails -> cache -> budget -> route -> dispatch -> commit
//
// Screening runs before the cache so a prompt-injection attempt cannot be laundered through a
// cache hit. The cache runs before the budget so that a tenant at its limit can still be served
// an answer that costs nothing. Budget runs before routing so a rejected request never reaches
// a provider.
type ChatService struct {
	deps ChatServiceDeps

	// cache counters back the hit-ratio gauge without a round trip to Prometheus.
	cacheHits, cacheLookups atomic.Int64
}

// NewChatService builds the use case, filling in safe defaults for optional collaborators.
func NewChatService(deps ChatServiceDeps) (*ChatService, error) {
	if deps.Router == nil {
		return nil, errors.New("chat service requires a router")
	}
	if deps.Registry == nil {
		return nil, errors.New("chat service requires a provider registry")
	}
	if deps.Clock == nil {
		deps.Clock = SystemClock{}
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.MaxAttempts <= 0 {
		deps.MaxAttempts = 3
	}
	return &ChatService{deps: deps}, nil
}

// Result is the outcome of a completed chat request, including everything the HTTP layer needs
// to build the response and the analytics event.
type Result struct {
	Response     domain.ChatResponse
	Decision     domain.RouteDecision
	Cached       bool
	CacheScore   float64
	Findings     []domain.Finding
	Redacted     bool
	FailoverPath []string
	CostUSD      float64
	LatencyMS    int
	BudgetWarn   bool
}

// Complete runs a non-streaming chat completion end to end.
func (s *ChatService) Complete(ctx context.Context, req domain.ChatRequest, tenant domain.Tenant) (Result, error) {
	start := s.deps.Clock.Now()
	res := Result{}

	// --- 1. input screening -------------------------------------------------
	screened, findings, redacted, err := s.screenInput(ctx, req, tenant)
	if err != nil {
		s.emitEvent(ctx, req, tenant, domain.RouteDecision{}, res, 400, start, true)
		return res, err
	}
	req = screened
	res.Findings = findings
	res.Redacted = redacted

	// --- 2. semantic cache --------------------------------------------------
	if cached, score, ok := s.lookupCache(ctx, req, tenant); ok {
		res.Cached = true
		res.CacheScore = score
		res.Response = domain.ChatResponse{
			ID:           "chatcmpl-cache-" + domain.HashText(req.UserPrompt())[:20],
			Model:        cached.Model,
			Provider:     cached.Provider,
			Created:      s.deps.Clock.Now(),
			Content:      cached.Content,
			FinishReason: "stop",
			Usage:        cached.Usage,
		}
		res.Decision = domain.RouteDecision{
			Provider: cached.Provider,
			Model:    domain.ModelDescriptor{ID: cached.Model, Provider: cached.Provider},
			Policy:   "cache",
			Reason:   fmt.Sprintf("semantic cache hit at similarity %.4f", score),
		}
		// A cache hit costs nothing, so it is not billed and does not consume budget. It is
		// still recorded, because "how much did caching save" is the point of the feature.
		res.LatencyMS = int(s.deps.Clock.Now().Sub(start).Milliseconds())
		s.emitEvent(ctx, req, tenant, res.Decision, res, 200, start, false)
		return res, nil
	}

	// --- 3. budget ----------------------------------------------------------
	if err := s.reserveBudget(ctx, req, tenant, &res); err != nil {
		s.emitEvent(ctx, req, tenant, domain.RouteDecision{}, res, 429, start, false)
		return res, err
	}

	// --- 4. routing ---------------------------------------------------------
	decision, err := s.deps.Router.Route(ctx, req, tenant)
	if err != nil {
		s.emitEvent(ctx, req, tenant, decision, res, 503, start, false)
		return res, err
	}
	res.Decision = decision

	// --- 5. dispatch with failover -----------------------------------------
	resp, finalDecision, path, err := s.dispatch(ctx, req, tenant, decision)
	res.FailoverPath = path
	if err != nil {
		s.emitEvent(ctx, req, tenant, decision, res, 502, start, false)
		return res, err
	}
	res.Decision = finalDecision
	res.Response = resp
	res.CostUSD = finalDecision.Model.CostUSD(resp.Usage)
	res.LatencyMS = int(s.deps.Clock.Now().Sub(start).Milliseconds())

	// --- 6. write-through and accounting ------------------------------------
	s.storeCache(ctx, req, tenant, finalDecision, resp)
	s.commitBudget(ctx, tenant, resp.Usage, res.CostUSD)

	if s.deps.Recorder != nil {
		s.deps.Recorder.ObserveUsage(tenant.ID, finalDecision.Provider, finalDecision.Model.ID,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, res.CostUSD)
	}
	s.emitEvent(ctx, req, tenant, finalDecision, res, 200, start, false)
	return res, nil
}

// dispatch attempts the primary decision and then each fallback in turn.
//
// It returns the response, the decision that produced it, and the provider path taken. Only
// retryable failures advance the chain: a 400 from the upstream will fail identically everywhere
// and retrying it just multiplies the latency the caller sees.
func (s *ChatService) dispatch(
	ctx context.Context,
	req domain.ChatRequest,
	tenant domain.Tenant,
	primary domain.RouteDecision,
) (domain.ChatResponse, domain.RouteDecision, []string, error) {
	attempts := append([]domain.RouteDecision{primary},
		s.deps.Router.Fallbacks(ctx, req, tenant, primary)...)
	if len(attempts) > s.deps.MaxAttempts {
		attempts = attempts[:s.deps.MaxAttempts]
	}

	path := make([]string, 0, len(attempts))
	failures := make([]AttemptFailure, 0, len(attempts))

	for i, d := range attempts {
		select {
		case <-ctx.Done():
			return domain.ChatResponse{}, d, path, ctx.Err()
		default:
		}

		provider, ok := s.deps.Registry.Get(d.Provider)
		if !ok {
			failures = append(failures, AttemptFailure{
				Provider: d.Provider, Model: d.Model.ID,
				Err: fmt.Errorf("no adapter registered"),
			})
			continue
		}

		breaker := s.breakerFor(d.Provider)
		if breaker != nil && !breaker.Allow() {
			failures = append(failures, AttemptFailure{
				Provider: d.Provider, Model: d.Model.ID,
				Err: fmt.Errorf("circuit breaker is open"),
			})
			continue
		}

		attemptReq := req
		attemptReq.RequestedModel = d.Model.ID

		startAttempt := s.deps.Clock.Now()
		resp, err := provider.Chat(ctx, attemptReq)
		elapsed := s.deps.Clock.Now().Sub(startAttempt)
		path = append(path, d.Provider+"/"+d.Model.ID)

		if err == nil {
			if breaker != nil {
				breaker.Success()
			}
			if s.deps.Recorder != nil {
				s.deps.Recorder.ObserveUpstream(d.Provider, d.Model.ID, "success", elapsed)
			}
			if i > 0 && s.deps.Recorder != nil {
				s.deps.Recorder.ObserveFailover(primary.Provider, d.Provider, "upstream_error", false)
			}
			return resp, d, path, nil
		}

		if breaker != nil {
			breaker.Failure()
		}
		if s.deps.Recorder != nil {
			s.deps.Recorder.ObserveUpstream(d.Provider, d.Model.ID, "error", elapsed)
		}
		failures = append(failures, AttemptFailure{Provider: d.Provider, Model: d.Model.ID, Err: err})

		s.deps.Log.Warn("provider attempt failed",
			"request_id", req.RequestID, "attempt", i, "provider", d.Provider,
			"model", d.Model.ID, "error", err)

		// A caller error will fail the same way everywhere. Stop rather than fan it out.
		if !isRetryable(err) {
			break
		}
		// A cancelled or timed-out context means the client is gone; further attempts are waste.
		if ctx.Err() != nil {
			break
		}
	}

	return domain.ChatResponse{}, primary, path, &UpstreamError{Attempts: failures}
}

// breakerFor fetches a provider's breaker if the registry exposes one.
//
// The optional interface keeps app.ProviderRegistry minimal: a test registry does not have to
// implement circuit breaking to be usable.
func (s *ChatService) breakerFor(provider string) Breaker {
	bp, ok := s.deps.Registry.(BreakerProvider)
	if !ok {
		return nil
	}
	b, ok := bp.Breaker(provider)
	if !ok {
		return nil
	}
	return b
}

// Breaker is the circuit-breaker behaviour the dispatcher needs.
type Breaker interface {
	Allow() bool
	Success()
	Failure()
}

// BreakerProvider is optionally implemented by a ProviderRegistry.
type BreakerProvider interface {
	Breaker(provider string) (Breaker, bool)
}

// Retryable is implemented by upstream errors that know whether another provider might succeed.
type Retryable interface {
	Retryable() bool
}

// isRetryable decides whether to advance the failover chain.
//
// The default is to retry: an unclassified error is usually a transport failure, and trying the
// next provider is the whole point of a gateway. Only an error that explicitly declares itself
// non-retryable, or a dead context, stops the chain.
func isRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var r Retryable
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return true
}

// --- stages -------------------------------------------------------------------

func (s *ChatService) screenInput(
	ctx context.Context,
	req domain.ChatRequest,
	tenant domain.Tenant,
) (domain.ChatRequest, []domain.Finding, bool, error) {
	if s.deps.Guardrails == nil {
		return req, nil, false, nil
	}

	start := s.deps.Clock.Now()
	result, err := s.deps.Guardrails.ScreenInput(ctx, tenant.ID, req.UserPrompt())
	elapsed := s.deps.Clock.Now().Sub(start)

	if err != nil {
		// The adapter owns the fail-open/fail-closed decision; an error here means it chose to
		// fail closed, or the failure was not a connectivity problem.
		return req, nil, false, fmt.Errorf("screening input: %w", err)
	}
	if result.FailedOpen && s.deps.Recorder != nil {
		s.deps.Recorder.GuardrailFailedOpen()
	}

	owasp, detector := "", ""
	if len(result.Findings) > 0 {
		owasp, detector = result.Findings[0].OWASP, result.Findings[0].Detector
	}
	if s.deps.Recorder != nil {
		s.deps.Recorder.ObserveGuardrail(elapsed, !result.Allowed, owasp, detector)
	}

	if !result.Allowed {
		return req, result.Findings, false, &GuardrailError{Findings: result.Findings}
	}

	// Redaction rewrites the last user turn in place. Everything downstream -- the cache key,
	// the embedding, the provider call -- then sees the redacted text, which is the point:
	// nothing unredacted must leave the perimeter.
	redacted := false
	if result.RedactedText != "" && result.RedactedText != req.UserPrompt() {
		req = replaceLastUserMessage(req, result.RedactedText)
		redacted = true
	}
	return req, result.Findings, redacted, nil
}

func (s *ChatService) lookupCache(ctx context.Context, req domain.ChatRequest, tenant domain.Tenant) (domain.CachedResponse, float64, bool) {
	if s.deps.Cache == nil {
		return domain.CachedResponse{}, 0, false
	}

	s.cacheLookups.Add(1)
	key := domain.NewCacheKey(tenant.ID, cacheFamily(req), req.SystemPrompt())

	cached, hit, err := s.deps.Cache.Lookup(ctx, key, req.UserPrompt())
	switch {
	case err != nil:
		// A broken cache is a degraded dependency, not a failed request.
		s.deps.Log.Warn("semantic cache lookup failed", "request_id", req.RequestID, "error", err)
		s.record("error", 0)
		return domain.CachedResponse{}, 0, false
	case !hit:
		s.record("miss", cached.Similarity)
		return domain.CachedResponse{}, 0, false
	default:
		s.cacheHits.Add(1)
		s.record("hit", cached.Similarity)
		return cached, cached.Similarity, true
	}
}

func (s *ChatService) storeCache(
	ctx context.Context,
	req domain.ChatRequest,
	tenant domain.Tenant,
	decision domain.RouteDecision,
	resp domain.ChatResponse,
) {
	if s.deps.Cache == nil || resp.Content == "" {
		return
	}
	key := domain.NewCacheKey(tenant.ID, cacheFamily(req), req.SystemPrompt())
	if err := s.deps.Cache.Store(ctx, key, req.UserPrompt(), resp); err != nil {
		s.deps.Log.Warn("semantic cache write failed",
			"request_id", req.RequestID, "model", decision.Model.ID, "error", err)
	}
}

// cacheFamily is the model-family component of the cache namespace.
//
// It is derived from the *request*, not from the model the router happened to pick, so that a
// cached answer is reusable by the next equivalent request regardless of which model served it.
// Using the served model would fragment the cache and collapse the hit rate.
func cacheFamily(req domain.ChatRequest) string {
	if req.RequestedModel == "" {
		return "auto"
	}
	return req.RequestedModel
}

func (s *ChatService) reserveBudget(ctx context.Context, req domain.ChatRequest, tenant domain.Tenant, res *Result) error {
	if s.deps.Budget == nil {
		return nil
	}

	est := estimateTokens(req)
	verdict, err := s.deps.Budget.Reserve(ctx, tenant, est)
	if err != nil {
		// A broken budget store must not block traffic; that would turn a Redis blip into a
		// full outage. It is logged and surfaced as a metric instead.
		s.deps.Log.Warn("budget reservation failed; allowing the request",
			"request_id", req.RequestID, "tenant_id", tenant.ID, "error", err)
		return nil
	}

	if verdict.Warn {
		res.BudgetWarn = true
		if s.deps.Recorder != nil {
			s.deps.Recorder.ObserveBudget(tenant.ID, "warn")
		}
	}
	if !verdict.Allowed {
		if s.deps.Recorder != nil {
			s.deps.Recorder.ObserveBudget(tenant.ID, "block")
		}
		return &BudgetError{Verdict: verdict}
	}
	return nil
}

func (s *ChatService) commitBudget(ctx context.Context, tenant domain.Tenant, usage domain.Usage, costUSD float64) {
	if s.deps.Budget == nil {
		return
	}
	if err := s.deps.Budget.Commit(ctx, tenant, usage, costUSD); err != nil {
		s.deps.Log.Warn("budget commit failed", "tenant_id", tenant.ID, "error", err)
	}
}

func (s *ChatService) emitEvent(
	ctx context.Context,
	req domain.ChatRequest,
	tenant domain.Tenant,
	decision domain.RouteDecision,
	res Result,
	status int,
	start time.Time,
	guardrailBlocked bool,
) {
	if s.deps.Events == nil {
		return
	}
	s.deps.Events.Emit(ctx, domain.RequestEvent{
		TS:                start,
		RequestID:         req.RequestID,
		TenantID:          tenant.ID,
		Policy:            decision.Policy,
		Model:             decision.Model.ID,
		Provider:          decision.Provider,
		PromptTokens:      res.Response.Usage.PromptTokens,
		CompletionTokens:  res.Response.Usage.CompletionTokens,
		Cached:            res.Cached,
		CacheSimilarity:   res.CacheScore,
		GuardrailBlocked:  guardrailBlocked,
		GuardrailFindings: len(res.Findings),
		FailoverCount:     maxInt(0, len(res.FailoverPath)-1),
		StatusCode:        status,
		LatencyMS:         int(s.deps.Clock.Now().Sub(start).Milliseconds()),
		CostUSD:           res.CostUSD,
	})
}

func (s *ChatService) record(result string, similarity float64) {
	if s.deps.Recorder == nil {
		return
	}
	lookups := s.cacheLookups.Load()
	ratio := 0.0
	if lookups > 0 {
		ratio = float64(s.cacheHits.Load()) / float64(lookups)
	}
	s.deps.Recorder.ObserveCache(result, similarity, ratio)
}

// CacheStats reports the process-lifetime hit ratio, used by /readyz and the bench harness.
func (s *ChatService) CacheStats() (hits, lookups int64) {
	return s.cacheHits.Load(), s.cacheLookups.Load()
}

// --- small helpers ------------------------------------------------------------

// estimateTokens approximates a request's token cost before it runs, for budget reservation.
// It uses the same four-characters-per-token rule as the rest of the system so that the estimate
// and the eventual actual are directly comparable.
func estimateTokens(req domain.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += (len(m.Content)+3)/4 + 3
	}
	// Reserve for the completion too, or a tenant could blow its budget on output tokens that
	// were never reserved.
	completion := 512
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		completion = *req.MaxTokens
	}
	return n + completion
}

func replaceLastUserMessage(req domain.ChatRequest, text string) domain.ChatRequest {
	msgs := make([]domain.Message, len(req.Messages))
	copy(msgs, req.Messages)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == domain.RoleUser {
			msgs[i].Content = text
			break
		}
	}
	req.Messages = msgs
	return req
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
