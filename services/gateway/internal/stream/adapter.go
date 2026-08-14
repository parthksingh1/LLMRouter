package stream

import (
	"context"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Stream adapts Runner to the app.StreamRunner port.
//
// The two types are structurally identical, and translating between them is the price of
// keeping internal/app free of any dependency on internal/stream: the use case describes what
// it needs, this package supplies it, and neither imports the other's concrete types.
func (r *Runner) Stream(
	ctx context.Context,
	req domain.ChatRequest,
	primary domain.RouteDecision,
	fallbacks []domain.RouteDecision,
	sink app.StreamSink,
) (app.StreamResult, error) {
	result, err := r.Run(ctx, req, primary, fallbacks, sinkAdapter{sink})

	out := app.StreamResult{
		Content:      result.Content,
		FinishReason: result.FinishReason,
		Usage:        result.Usage,
		Decision:     result.Decision,
		Path:         result.Path,
		Restarted:    result.Restarted,
	}
	for _, f := range result.Failovers {
		out.Failovers = append(out.Failovers, app.StreamFailover(f))
	}
	return out, err
}

// sinkAdapter bridges app.StreamSink to the local Sink interface.
type sinkAdapter struct{ inner app.StreamSink }

func (a sinkAdapter) Delta(text string) error { return a.inner.Delta(text) }

func (a sinkAdapter) Failover(event FailoverEvent) error {
	return a.inner.Failover(app.StreamFailover(event))
}

func (a sinkAdapter) Done(summary Summary) error {
	return a.inner.Done(app.StreamSummary{
		FinishReason: summary.FinishReason,
		Usage:        summary.Usage,
		Provider:     summary.Provider,
		Model:        summary.Model,
		Path:         summary.Path,
		Failovers:    summary.Failovers,
	})
}

// Compile-time assertion that Runner satisfies the port its consumer declared.
var _ app.StreamRunner = (*Runner)(nil)
