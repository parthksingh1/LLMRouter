package http

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// sseSink writes a streamed answer as OpenAI-compatible SSE frames.
//
// It satisfies app.StreamSink. The important property is that every `data:` frame is a valid
// chat.completion.chunk: a vanilla OpenAI SDK sees an ordinary stream, and the failover
// metadata rides on a *named* event that those SDKs skip. That is what lets the gateway be
// honest about a provider switch without breaking any client that does not care.
type sseSink struct {
	writer    *SSEWriter
	id        string
	requestID string
	model     string
	created   int64

	roleSent     bool
	includeUsage bool
}

func newSSESink(w *SSEWriter, id, requestID, model string, includeUsage bool) *sseSink {
	return &sseSink{
		writer:       w,
		id:           id,
		requestID:    requestID,
		model:        model,
		created:      time.Now().Unix(),
		includeUsage: includeUsage,
	}
}

func (s *sseSink) base() openaiapi.ChatCompletionChunk {
	return openaiapi.ChatCompletionChunk{
		ID:      s.id,
		Object:  openaiapi.ObjectChatCompletionChunk,
		Created: s.created,
		Model:   s.model,
	}
}

// Delta forwards one token to the client.
func (s *sseSink) Delta(text string) error {
	// OpenAI sends the assistant role on the first delta and only there; clients that build a
	// message from the stream rely on it arriving exactly once.
	if !s.roleSent {
		chunk := s.base()
		chunk.Choices = []openaiapi.Choice{{
			Index: 0,
			Delta: &openaiapi.Message{Role: openaiapi.RoleAssistant},
		}}
		if err := s.writer.WriteChunk(chunk); err != nil {
			return err
		}
		s.roleSent = true
	}

	chunk := s.base()
	chunk.Choices = []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Content: text}}}
	return s.writer.WriteChunk(chunk)
}

// Failover publishes the provider switch.
func (s *sseSink) Failover(event app.StreamFailover) error {
	// The model on subsequent chunks must reflect who is actually generating now, or a client
	// logging per-model usage from the stream would attribute the tail to the wrong provider.
	s.model = event.Model
	return s.writer.WriteEvent("failover", event)
}

// Done writes the terminating chunk and the [DONE] marker.
//
// The terminator carries the llmrouter routing block, which is the streamed equivalent of what
// the non-streaming response returns. A caller that wants to know which provider answered, what
// it cost, and whether it failed over gets the same information either way.
func (s *sseSink) Done(summary app.StreamSummary) error {
	finishReason := summary.FinishReason
	if finishReason == "" {
		finishReason = openaiapi.FinishStop
	}

	chunk := s.base()
	if summary.Model != "" {
		chunk.Model = summary.Model
	}
	chunk.Choices = []openaiapi.Choice{{
		Index:        0,
		Delta:        &openaiapi.Message{},
		FinishReason: &finishReason,
	}}
	if s.includeUsage {
		chunk.Usage = &openaiapi.Usage{
			PromptTokens:     summary.Usage.PromptTokens,
			CompletionTokens: summary.Usage.CompletionTokens,
			TotalTokens:      summary.Usage.TotalTokens(),
		}
	}
	chunk.LLMRouter = &openaiapi.RoutingMeta{
		RequestID:     s.requestID,
		Policy:        summary.Policy,
		Provider:      summary.Provider,
		ResolvedModel: summary.Model,
		Difficulty:    summary.Difficulty,
		Cached:        summary.Cached,
		CacheScore:    summary.CacheScore,
		Failovers:     summary.Failovers,
		FailoverPath:  summary.Path,
		CostUSD:       summary.CostUSD,
		GuardrailHits: summary.Findings,
	}
	if err := s.writer.WriteChunk(chunk); err != nil {
		return err
	}
	return s.writer.Done()
}

// StreamCompleter is the streaming use case this handler drives.
type StreamCompleter interface {
	CompleteStream(ctx context.Context, req domain.ChatRequest, tenant domain.Tenant, sink app.StreamSink) (app.Result, error)
}

// completionsStream serves a streamed chat completion.
//
// Error handling changes partway through, and that is inherent to SSE rather than a shortcut:
// before the first byte a real HTTP status is still possible and is far more useful to a client;
// after it, the status is already 200 and a failure has to be delivered inside the stream.
func (h *ChatHandler) completionsStream(
	w http.ResponseWriter,
	r *http.Request,
	req domain.ChatRequest,
	tenant domain.Tenant,
) {
	streamer, ok := h.svc.(StreamCompleter)
	if !ok {
		WriteError(w, r, http.StatusNotImplemented, openaiapi.NewError(
			"streaming is not configured on this gateway",
			openaiapi.ErrTypeAPI, "streaming_unavailable", ""))
		return
	}

	// Everything that can fail before the stream opens is allowed to fail properly. The use
	// case screens, checks the cache and reserves budget before it writes anything, so a 400 or
	// a 429 still reaches the client as a status code rather than as an in-band error frame.
	sse := NewSSEWriter(w)
	deferred := &deferredOpen{writer: sse}

	sink := newSSESink(sse, "chatcmpl-"+req.RequestID, req.RequestID, req.RequestedModel, req.IncludeUsage)
	deferred.sink = sink

	_, err := streamer.CompleteStream(r.Context(), req, tenant, deferred)

	switch {
	case err == nil:
		return

	case !deferred.opened:
		// Nothing has been written: report it properly.
		WriteAppError(w, r, err)
		return

	case errors.Is(err, context.Canceled):
		// The client hung up. Nothing to report, and nobody to report it to.
		LoggerFrom(r.Context()).Debug("client disconnected mid-stream", "request_id", req.RequestID)
		return

	default:
		// Mid-stream failure with the chain exhausted. The client has a partial answer; telling
		// it so in-band is the only option left, and it is what OpenAI's own API does.
		LoggerFrom(r.Context()).Error("stream failed after the response had started",
			"request_id", req.RequestID, "error", err)
		_ = sse.WriteError("the upstream provider failed and no fallback could continue the response",
			openaiapi.ErrTypeAPI, "upstream_error")
		_ = sse.Done()
		return
	}
}

// deferredOpen opens the SSE response lazily, on the first write.
//
// This is what preserves proper HTTP status codes for pre-stream failures. Opening eagerly would
// commit a 200 before the use case had decided whether the request is even allowed, turning
// every guardrail block and budget rejection into a 200 with an error frame.
type deferredOpen struct {
	writer *SSEWriter
	sink   *sseSink
	opened bool
	err    error
}

func (d *deferredOpen) open() error {
	if d.opened {
		return d.err
	}
	d.opened = true
	d.err = d.writer.Open()
	return d.err
}

func (d *deferredOpen) Delta(text string) error {
	if err := d.open(); err != nil {
		return err
	}
	return d.sink.Delta(text)
}

func (d *deferredOpen) Failover(event app.StreamFailover) error {
	if err := d.open(); err != nil {
		return err
	}
	return d.sink.Failover(event)
}

func (d *deferredOpen) Done(summary app.StreamSummary) error {
	if err := d.open(); err != nil {
		return err
	}
	return d.sink.Done(summary)
}
