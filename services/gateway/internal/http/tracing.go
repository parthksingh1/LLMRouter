package http

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/telemetry"
)

// Tracing starts a server span per request and continues any inbound trace.
//
// Continuing the caller's trace is the point: without extracting the W3C traceparent header the
// gateway would start a fresh trace for every request, and an application trying to understand
// why its user-facing request was slow would see the LLM call as an unexplained gap.
func Tracing(enabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !enabled {
			return next
		}
		propagator := otel.GetTextMapPropagator()

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			// The chi route pattern, not the raw path: a span name per request id would make
			// Jaeger's operation list useless and its aggregates meaningless.
			name := r.Method + " " + routePattern(r.WithContext(ctx))

			ctx, span := telemetry.Tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer))
			defer span.End()

			// The request id is on the span so a support ticket quoting one can be found, and
			// the trace id is on the response so a caller can quote it back.
			if id := RequestIDFrom(r.Context()); id != "" {
				span.SetAttributes(attribute.String("llmrouter.request_id", id))
				ctx = context.WithValue(ctx, ctxKeyRequestID, id)
			}
			if traceID := telemetry.TraceIDFrom(ctx); traceID != "" {
				w.Header().Set("X-Trace-Id", traceID)
			}

			rec, ok := w.(*statusRecorder)
			if !ok {
				rec = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
				w = rec
			}

			next.ServeHTTP(w, r.WithContext(ctx))

			span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}
