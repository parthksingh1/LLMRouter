// Package stream implements streamed generation with mid-stream provider failover.
//
// The problem this solves: a provider can die *after* the client has already received tokens.
// At that point the HTTP status is long since 200 and half an answer is on the wire, so the
// options are to truncate (the client sees a sentence stop mid-word) or to recover. Recovering
// is what this package does.
//
// The mechanism:
//
//  1. Every delta is accumulated as it is forwarded, so the gateway always knows exactly what
//     the client has seen.
//  2. A stall timer runs alongside. An upstream that goes quiet is as dead as one that errors,
//     and it is the more common failure -- it just takes longer to notice.
//  3. On a break, the client's SSE connection is left open. A fallback provider is selected and
//     asked to continue from the accumulated prefix.
//  4. How it continues depends on what the fallback supports (config: prefix_continuation):
//     native  the prefix is sent as a trailing assistant message and the model carries on
//     prefill the same trick, with a caveat noted in ADR-0003
//     none    the provider cannot continue, so generation restarts and the client is told
//  5. Token usage from every attempt is summed, so a failed-over request bills once and
//     completely.
//
// A synthetic `event: failover` frame carries the metadata. Vanilla OpenAI SDKs read only
// unnamed `data:` frames, so they see one uninterrupted stream; a client that wants to know can
// listen for the named event.
package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// ErrStalled means an upstream stopped sending for longer than the stall timeout.
var ErrStalled = errors.New("upstream stalled")

// ErrExhausted means every provider in the chain failed.
var ErrExhausted = errors.New("every provider attempt failed")

// Sink receives the reassembled stream. The HTTP layer implements it over an SSE writer.
//
// Delta is called for every token the client should see; Failover is called when the gateway
// switches provider; Done is called once with the final accounting.
type Sink interface {
	Delta(text string) error
	Failover(event FailoverEvent) error
	Done(summary Summary) error
}

// Summary is the final accounting for a streamed request.
type Summary struct {
	FinishReason string
	Usage        domain.Usage
	Provider     string
	Model        string
	Path         []string
	Failovers    int
	Restarted    bool
}

// FailoverEvent is the metadata carried by the synthetic `event: failover` frame.
type FailoverEvent struct {
	Attempt int    `json:"attempt"`
	From    string `json:"from"`
	To      string `json:"to"`
	Model   string `json:"model"`
	Reason  string `json:"reason"`
	// Mode is how the fallback resumed: "continue" or "restart".
	Mode string `json:"mode"`
	// TokensPreserved is how many tokens the client had already received at the break.
	TokensPreserved int `json:"tokens_preserved"`
	// Restarted is true when the client will now see a fresh answer rather than a continuation.
	// It is surfaced explicitly because it is the case where the client's transcript will
	// contain a discontinuity, and pretending otherwise would be dishonest.
	Restarted bool `json:"restarted"`
}

// Result is the outcome of a streamed request.
type Result struct {
	Content      string
	FinishReason string
	Usage        domain.Usage
	Decision     domain.RouteDecision
	Path         []string
	Failovers    []FailoverEvent
	Restarted    bool
}

// Registry is the slice of the provider registry the runner needs.
type Registry interface {
	Get(name string) (app.Provider, bool)
	Spec(name string) (config.ProviderSpec, bool)
}

// Recorder is the telemetry the runner emits.
type Recorder interface {
	ObserveUpstream(provider, model, outcome string, d time.Duration)
	ObserveFailover(from, to, reason string, midstream bool)
}

// Runner drives one streamed request across one or more providers.
type Runner struct {
	registry     Registry
	stallTimeout time.Duration
	maxAttempts  int
	recorder     Recorder
	log          *slog.Logger
	now          func() time.Time
}

// Options configures the runner.
type Options struct {
	Registry Registry
	// StallTimeout is how long an upstream may go quiet before it is declared dead.
	StallTimeout time.Duration
	MaxAttempts  int
	Recorder     Recorder
	Log          *slog.Logger
	Now          func() time.Time
}

// NewRunner builds the runner.
func NewRunner(opts Options) (*Runner, error) {
	if opts.Registry == nil {
		return nil, fmt.Errorf("stream runner requires a provider registry")
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = 4 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Runner{
		registry:     opts.Registry,
		stallTimeout: opts.StallTimeout,
		maxAttempts:  opts.MaxAttempts,
		recorder:     opts.Recorder,
		log:          opts.Log,
		now:          opts.Now,
	}, nil
}

// Run streams a request, failing over as needed, and writes to the sink.
//
// The client's connection is never closed by a provider failure: only an exhausted chain or a
// client disconnect ends the stream.
func (r *Runner) Run(
	ctx context.Context,
	req domain.ChatRequest,
	primary domain.RouteDecision,
	fallbacks []domain.RouteDecision,
	sink Sink,
) (Result, error) {
	attempts := append([]domain.RouteDecision{primary}, fallbacks...)
	if len(attempts) > r.maxAttempts {
		attempts = attempts[:r.maxAttempts]
	}

	result := Result{Decision: primary}
	// accumulated is exactly what the client has seen. It is the source of truth for both the
	// continuation prefix and the "did we already commit to an answer" question.
	var accumulated strings.Builder
	var failures []app.AttemptFailure

	for i, decision := range attempts {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		provider, ok := r.registry.Get(decision.Provider)
		if !ok {
			failures = append(failures, app.AttemptFailure{
				Provider: decision.Provider, Model: decision.Model.ID,
				Err: errors.New("no adapter registered"),
			})
			continue
		}

		prefix := accumulated.String()
		mode := r.continuationMode(decision.Provider, prefix)

		// A restart means the client is about to receive a second, different answer. Telling it
		// to discard what it has is the only honest option, and the sink decides how -- an SSE
		// client cannot unsend bytes, so the failover event carries the fact.
		if prefix != "" && mode == modeRestart {
			result.Restarted = true
			accumulated.Reset()
		}

		if i > 0 {
			event := FailoverEvent{
				Attempt:         i,
				From:            attempts[i-1].Provider,
				To:              decision.Provider,
				Model:           decision.Model.ID,
				Reason:          reasonOf(failures),
				Mode:            string(mode),
				TokensPreserved: countTokens(prefix),
				Restarted:       mode == modeRestart && prefix != "",
			}
			result.Failovers = append(result.Failovers, event)
			if err := sink.Failover(event); err != nil {
				return result, fmt.Errorf("writing failover event: %w", err)
			}
			if r.recorder != nil {
				r.recorder.ObserveFailover(event.From, event.To, event.Reason, prefix != "")
			}
		}

		attemptReq := r.buildRequest(req, decision, accumulated.String(), mode)
		started := r.now()

		usage, finish, err := r.pump(ctx, provider, attemptReq, sink, &accumulated)
		elapsed := r.now().Sub(started)
		result.Path = append(result.Path, decision.Provider+"/"+decision.Model.ID)
		result.Usage = result.Usage.Add(usage)

		if err == nil {
			if r.recorder != nil {
				r.recorder.ObserveUpstream(decision.Provider, decision.Model.ID, "success", elapsed)
			}
			result.Decision = decision
			result.Content = accumulated.String()
			result.FinishReason = finish
			if err := sink.Done(Summary{
				FinishReason: finish,
				Usage:        result.Usage,
				Provider:     decision.Provider,
				Model:        decision.Model.ID,
				Path:         result.Path,
				Failovers:    len(result.Failovers),
				Restarted:    result.Restarted,
			}); err != nil {
				return result, fmt.Errorf("writing stream terminator: %w", err)
			}
			return result, nil
		}

		if r.recorder != nil {
			r.recorder.ObserveUpstream(decision.Provider, decision.Model.ID, "error", elapsed)
		}
		failures = append(failures, app.AttemptFailure{
			Provider: decision.Provider, Model: decision.Model.ID, Err: err,
		})
		r.log.Warn("stream attempt failed",
			"request_id", req.RequestID,
			"attempt", i,
			"provider", decision.Provider,
			"model", decision.Model.ID,
			"tokens_already_sent", countTokens(accumulated.String()),
			"error", err)

		// The client going away is not a provider failure, and retrying for a client that has
		// hung up burns a provider call for nobody.
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
	}

	return result, fmt.Errorf("%w: %s", ErrExhausted, (&app.UpstreamError{Attempts: failures}).Error())
}

// continuation describes how a fallback resumes.
type continuation string

const (
	modeFresh    continuation = "fresh"
	modeContinue continuation = "continue"
	modeRestart  continuation = "restart"
)

// continuationMode decides how this provider can resume from a partial answer.
func (r *Runner) continuationMode(provider, prefix string) continuation {
	if prefix == "" {
		return modeFresh
	}
	spec, ok := r.registry.Spec(provider)
	if !ok {
		return modeRestart
	}
	switch spec.PrefixContinuation {
	case config.PrefixNative, config.PrefixPrefill:
		return modeContinue
	default:
		return modeRestart
	}
}

// buildRequest prepares the request for one attempt.
//
// For a continuation the accumulated text is appended as a trailing assistant message, which is
// the shape both Anthropic's Messages API and OpenAI-compatible prefill expect. The adapters
// translate it; nothing provider-specific leaks in here.
func (r *Runner) buildRequest(
	req domain.ChatRequest,
	decision domain.RouteDecision,
	prefix string,
	mode continuation,
) domain.ChatRequest {
	out := req
	out.RequestedModel = decision.Model.ID

	if mode != modeContinue || prefix == "" {
		return out
	}

	messages := make([]domain.Message, len(req.Messages), len(req.Messages)+1)
	copy(messages, req.Messages)
	messages = append(messages, domain.Message{Role: domain.RoleAssistant, Content: prefix})
	out.Messages = messages
	return out
}

// pump forwards one provider's stream to the sink, enforcing the stall timeout.
//
// It returns the usage the provider reported, the finish reason, and an error if the stream
// broke. Deltas already forwarded stay forwarded: the caller's accumulator has them, which is
// what makes continuation possible.
func (r *Runner) pump(
	ctx context.Context,
	provider app.Provider,
	req domain.ChatRequest,
	sink Sink,
	accumulated *strings.Builder,
) (domain.Usage, string, error) {
	chunks, err := provider.ChatStream(ctx, req)
	if err != nil {
		return domain.Usage{}, "", fmt.Errorf("opening stream: %w", err)
	}

	// The stall timer is reset on every chunk. A provider that has sent nothing for longer than
	// the timeout is treated as dead even though the TCP connection is still open -- which is
	// the most common way an LLM stream fails, and the one a plain read loop never notices.
	timer := time.NewTimer(r.stallTimeout)
	defer timer.Stop()

	var usage domain.Usage
	finish := ""

	for {
		select {
		case <-ctx.Done():
			return usage, finish, ctx.Err()

		case <-timer.C:
			return usage, finish, fmt.Errorf("%w after %s", ErrStalled, r.stallTimeout)

		case chunk, open := <-chunks:
			if !open {
				// A closed channel with no terminal chunk means the provider finished without
				// saying so. Treated as a clean end rather than an error: the tokens are real.
				if finish == "" {
					finish = "stop"
				}
				return usage, finish, nil
			}

			if !timer.Stop() {
				// The timer had already fired and drained; a fresh one is needed.
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(r.stallTimeout)

			if chunk.Err != nil {
				return usage, finish, chunk.Err
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			if chunk.FinishReason != nil {
				finish = *chunk.FinishReason
			}
			if chunk.Delta == "" {
				continue
			}

			accumulated.WriteString(chunk.Delta)
			if err := sink.Delta(chunk.Delta); err != nil {
				// The client has gone. Stop pumping rather than draining a provider for nobody.
				return usage, finish, fmt.Errorf("writing to client: %w", err)
			}
		}
	}
}

// reasonOf summarises why the previous attempt failed, for the failover event.
func reasonOf(failures []app.AttemptFailure) string {
	if len(failures) == 0 {
		return "unknown"
	}
	err := failures[len(failures)-1].Err
	switch {
	case errors.Is(err, ErrStalled):
		return "stalled"
	case err == nil:
		return "unknown"
	case strings.Contains(err.Error(), "reset"):
		return "stream_reset"
	case strings.Contains(err.Error(), "opening stream"):
		return "connect_failed"
	default:
		return "upstream_error"
	}
}

// countTokens approximates a token count, matching the estimator used elsewhere.
func countTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}
