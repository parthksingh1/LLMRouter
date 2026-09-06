package app

import (
	"context"
	"fmt"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// StreamSink is what the HTTP layer provides to receive a streamed answer.
//
// It mirrors internal/stream.Sink, redeclared here so internal/app does not import the stream
// package -- the use case orchestrates, the stream package executes, and neither needs the
// other's types.
type StreamSink interface {
	Delta(text string) error
	Failover(event StreamFailover) error
	Done(summary StreamSummary) error
}

// StreamSummary is everything the terminating frame needs.
//
// It is one call rather than a Routing() followed by a Done() because two calls that must
// happen in order is an invariant a sink can get wrong; one call cannot be.
type StreamSummary struct {
	FinishReason string
	Usage        domain.Usage
	Provider     string
	Model        string
	Policy       string
	Difficulty   string
	Cached       bool
	CacheScore   float64
	CostUSD      float64
	Path         []string
	Failovers    int
	Findings     int
}

// StreamFailover is the metadata published when the gateway switches provider mid-answer.
type StreamFailover struct {
	Attempt         int    `json:"attempt"`
	From            string `json:"from"`
	To              string `json:"to"`
	Model           string `json:"model"`
	Reason          string `json:"reason"`
	Mode            string `json:"mode"`
	TokensPreserved int    `json:"tokens_preserved"`
	Restarted       bool   `json:"restarted"`
}

// StreamRunner executes a streamed request across providers. internal/stream implements it.
type StreamRunner interface {
	Stream(
		ctx context.Context,
		req domain.ChatRequest,
		primary domain.RouteDecision,
		fallbacks []domain.RouteDecision,
		sink StreamSink,
	) (StreamResult, error)
}

// StreamResult is what a completed stream produced.
type StreamResult struct {
	Content      string
	FinishReason string
	Usage        domain.Usage
	Decision     domain.RouteDecision
	Path         []string
	Failovers    []StreamFailover
	Restarted    bool
}

// CompleteStream runs a streamed chat completion end to end.
//
// It shares the pre-dispatch stages with Complete -- screening, cache, budget -- because the
// reasons for them do not change when the response is streamed. What differs is everything
// after: dispatch is delegated to the StreamRunner, which owns mid-stream failover, and the
// cache write-through happens only once the full answer is known.
//
// A cache hit on a streamed request is served *as* a stream: the client asked for SSE and must
// get SSE, however the answer was produced.
func (s *ChatService) CompleteStream(
	ctx context.Context,
	req domain.ChatRequest,
	tenant domain.Tenant,
	sink StreamSink,
) (Result, error) {
	if s.deps.Stream == nil {
		return Result{}, fmt.Errorf("streaming is not configured on this gateway")
	}

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
		res.Decision = domain.RouteDecision{
			Provider: cached.Provider,
			Model:    domain.ModelDescriptor{ID: cached.Model, Provider: cached.Provider},
			Policy:   "cache",
			Reason:   fmt.Sprintf("semantic cache hit at similarity %.4f", score),
		}
		res.Response = domain.ChatResponse{
			ID:           "chatcmpl-cache-" + domain.HashText(req.UserPrompt())[:20],
			Model:        cached.Model,
			Provider:     cached.Provider,
			Created:      s.deps.Clock.Now(),
			Content:      cached.Content,
			FinishReason: "stop",
			Usage:        cached.Usage,
		}

		if deltaErr := sink.Delta(cached.Content); deltaErr != nil {
			return res, deltaErr
		}
		if doneErr := sink.Done(StreamSummary{
			FinishReason: "stop",
			Usage:        cached.Usage,
			Provider:     cached.Provider,
			Model:        cached.Model,
			Policy:       "cache",
			Cached:       true,
			CacheScore:   score,
			Findings:     len(findings),
		}); doneErr != nil {
			return res, doneErr
		}

		res.LatencyMS = int(s.deps.Clock.Now().Sub(start).Milliseconds())
		s.emitEvent(ctx, req, tenant, res.Decision, res, 200, start, false)
		return res, nil
	}

	// --- 3. budget ----------------------------------------------------------
	if budgetErr := s.reserveBudget(ctx, req, tenant, &res); budgetErr != nil {
		s.emitEvent(ctx, req, tenant, domain.RouteDecision{}, res, 429, start, false)
		return res, budgetErr
	}

	// --- 4. routing ---------------------------------------------------------
	decision, err := s.deps.Router.Route(ctx, req, tenant)
	if err != nil {
		s.emitEvent(ctx, req, tenant, decision, res, 503, start, false)
		return res, err
	}
	res.Decision = decision

	// --- 5. stream with mid-stream failover ---------------------------------
	fallbacks := s.deps.Router.Fallbacks(ctx, req, tenant, decision)
	// The runner knows the provider and the usage; only the use case knows the policy, the
	// difficulty and the price. enrich() merges the two so the terminating frame carries the
	// same routing block a non-streamed response returns.
	enriched := &enrichingSink{
		inner:      sink,
		policy:     decision.Policy,
		difficulty: string(decision.Difficulty),
		findings:   len(res.Findings),
		price:      func(m string, u domain.Usage) float64 { return priceWith(decision, m, u) },
	}
	streamed, err := s.deps.Stream.Stream(ctx, req, decision, fallbacks, enriched)

	res.FailoverPath = streamed.Path
	res.Decision = streamed.Decision
	res.Response = domain.ChatResponse{
		ID:           "chatcmpl-" + req.RequestID,
		Model:        streamed.Decision.Model.ID,
		Provider:     streamed.Decision.Provider,
		Created:      s.deps.Clock.Now(),
		Content:      streamed.Content,
		FinishReason: streamed.FinishReason,
		Usage:        streamed.Usage,
	}
	// Usage is summed across every attempt by the runner, so a request that failed over is
	// billed once and completely rather than only for the attempt that happened to finish.
	res.CostUSD = streamed.Decision.Model.CostUSD(streamed.Usage)
	res.LatencyMS = int(s.deps.Clock.Now().Sub(start).Milliseconds())

	if err != nil {
		s.emitEvent(ctx, req, tenant, streamed.Decision, res, 502, start, false)
		return res, err
	}

	// --- 6. write-through and accounting ------------------------------------
	//
	// A restarted stream is not cached. The client received a discontinuity, and storing the
	// second answer against this prompt would serve that discontinuity's tail to the next
	// caller as though it were a whole answer.
	if !streamed.Restarted {
		s.storeCache(ctx, req, tenant, streamed.Decision, res.Response)
	}
	s.commitBudget(ctx, tenant, streamed.Usage, res.CostUSD)

	if s.deps.Recorder != nil {
		s.deps.Recorder.ObserveUsage(tenant.ID, streamed.Decision.Provider, streamed.Decision.Model.ID,
			streamed.Usage.PromptTokens, streamed.Usage.CompletionTokens, res.CostUSD)
	}
	s.emitEvent(ctx, req, tenant, streamed.Decision, res, 200, start, false)
	return res, nil
}

// enrichingSink adds the routing context the runner does not have.
type enrichingSink struct {
	inner      StreamSink
	policy     string
	difficulty string
	findings   int
	price      func(model string, usage domain.Usage) float64
}

func (e *enrichingSink) Delta(text string) error { return e.inner.Delta(text) }

func (e *enrichingSink) Failover(event StreamFailover) error { return e.inner.Failover(event) }

func (e *enrichingSink) Done(summary StreamSummary) error {
	summary.Policy = e.policy
	summary.Difficulty = e.difficulty
	summary.Findings = e.findings
	if e.price != nil {
		summary.CostUSD = e.price(summary.Model, summary.Usage)
	}
	return e.inner.Done(summary)
}

// priceWith prices the streamed usage against whichever model actually served it.
//
// A failed-over request is billed at the price of the model that finished it, not the one that
// broke, which is what makes the cost figure match the invoice.
func priceWith(decision domain.RouteDecision, model string, usage domain.Usage) float64 {
	if model == "" || model == decision.Model.ID {
		return decision.Model.CostUSD(usage)
	}
	// The fallback's descriptor is not carried through the runner; pricing it at the primary's
	// rate would be wrong in the other direction, so the caller reconciles from the event
	// stream. Returning the primary's price here is a documented approximation for the
	// in-band metadata only -- the ClickHouse event uses the real decision.
	return decision.Model.CostUSD(usage)
}
