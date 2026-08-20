package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Span attribute keys.
//
// Prefixed `llmrouter.` rather than reusing the OpenTelemetry gen-ai semantic conventions,
// which were still changing when this was written. Using a stable private namespace and
// documenting the mapping is better than tracking a moving spec and silently breaking every
// saved Jaeger query on upgrade.
const (
	AttrTenant       = attribute.Key("llmrouter.tenant_id")
	AttrPolicy       = attribute.Key("llmrouter.policy")
	AttrProvider     = attribute.Key("llmrouter.provider")
	AttrModel        = attribute.Key("llmrouter.model")
	AttrDifficulty   = attribute.Key("llmrouter.difficulty")
	AttrCached       = attribute.Key("llmrouter.cached")
	AttrCacheScore   = attribute.Key("llmrouter.cache_similarity")
	AttrPromptTokens = attribute.Key("llmrouter.prompt_tokens")
	AttrOutputTokens = attribute.Key("llmrouter.completion_tokens")
	AttrCostUSD      = attribute.Key("llmrouter.cost_usd")
	AttrFailover     = attribute.Key("llmrouter.failover")
	AttrFailoverFrom = attribute.Key("llmrouter.failover_from")
	AttrFailoverTo   = attribute.Key("llmrouter.failover_to")
	AttrAttempt      = attribute.Key("llmrouter.attempt")
	AttrGuardBlocked = attribute.Key("llmrouter.guardrail_blocked")
	AttrGuardOWASP   = attribute.Key("llmrouter.guardrail_owasp")
	AttrFailedOpen   = attribute.Key("llmrouter.guardrail_failed_open")
)

// TracerName is the instrumentation scope.
const TracerName = "github.com/parthkumarsingh/llmrouter/gateway"

// Tracing holds the tracer provider so it can be shut down cleanly.
type Tracing struct {
	provider *sdktrace.TracerProvider
	log      *slog.Logger
	enabled  bool
}

// TracingOptions configures tracing.
type TracingOptions struct {
	// Endpoint is the OTLP gRPC collector address. Empty disables tracing entirely.
	Endpoint    string
	ServiceName string
	Version     string
	Environment string
	SampleRatio float64
	Log         *slog.Logger
}

// NewTracing wires OpenTelemetry.
//
// An unreachable collector is not fatal and does not retry aggressively: the exporter buffers
// and drops. Tracing is diagnostic, and a gateway that will not start because its trace
// collector is down has turned an observability dependency into an availability dependency.
func NewTracing(ctx context.Context, opts TracingOptions) (*Tracing, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Endpoint == "" {
		opts.Log.Info("tracing disabled: no OTLP endpoint configured")
		return &Tracing{log: opts.Log}, nil
	}
	if opts.SampleRatio <= 0 {
		opts.SampleRatio = 1.0
	}

	// A short timeout: start-up must not stall on a collector that is not there yet.
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	exporter, err := otlptracegrpc.New(dialCtx,
		otlptracegrpc.WithEndpointURL(normaliseEndpoint(opts.Endpoint)),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(opts.ServiceName),
			semconv.ServiceVersion(opts.Version),
			attribute.String("deployment.environment", opts.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("building resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(4096),
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
		// ParentBased means an upstream service's sampling decision is honoured, so a trace is
		// never half-sampled: either the whole request is recorded or none of it is.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.SampleRatio))),
	)

	otel.SetTracerProvider(provider)
	// W3C tracecontext plus baggage, so a trace started by the caller continues through the
	// gateway into the guardrails sidecar rather than starting again.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	opts.Log.Info("tracing enabled",
		"endpoint", opts.Endpoint, "service", opts.ServiceName, "sample_ratio", opts.SampleRatio)

	return &Tracing{provider: provider, log: opts.Log, enabled: true}, nil
}

// normaliseEndpoint accepts either a bare host:port or a full URL, because the OTEL_ env var
// convention allows both and getting it wrong produces a silent no-op.
func normaliseEndpoint(endpoint string) string {
	if len(endpoint) > 7 && (endpoint[:7] == "http://" || endpoint[:8] == "https://") {
		return endpoint
	}
	return "http://" + endpoint
}

// Enabled reports whether traces are being exported.
func (t *Tracing) Enabled() bool { return t != nil && t.enabled }

// Shutdown flushes pending spans.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	// Bounded: a slow collector must not extend the shutdown grace period that in-flight SSE
	// streams are relying on.
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := t.provider.Shutdown(flushCtx); err != nil {
		return fmt.Errorf("shutting down tracing: %w", err)
	}
	return nil
}

// Tracer returns the gateway's tracer.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// StartRequest opens the root span for a chat request.
func StartRequest(ctx context.Context, name string, req domain.ChatRequest, tenant domain.Tenant) (context.Context, trace.Span) {
	ctx, span := Tracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			AttrTenant.String(tenant.ID),
			AttrModel.String(req.RequestedModel),
			AttrPromptTokens.Int(len(req.Messages)),
		),
	)
	return ctx, span
}

// RecordRouting annotates a span with the routing decision.
//
// These are the attributes that make a trace searchable in a way that answers real questions:
// "show me every request this tenant routed to the frontier model", "show me every failover
// last night". A span with only a duration answers none of them.
func RecordRouting(span trace.Span, d domain.RouteDecision) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(
		AttrPolicy.String(d.Policy),
		AttrProvider.String(d.Provider),
		AttrModel.String(d.Model.ID),
		AttrDifficulty.String(string(d.Difficulty)),
	)
	if d.Reason != "" {
		span.AddEvent("routed", trace.WithAttributes(attribute.String("reason", d.Reason)))
	}
}

// RecordCache annotates a span with the cache outcome.
func RecordCache(span trace.Span, hit bool, similarity float64) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(AttrCached.Bool(hit))
	if similarity > 0 {
		span.SetAttributes(AttrCacheScore.Float64(similarity))
	}
}

// RecordUsage annotates a span with tokens and cost.
func RecordUsage(span trace.Span, usage domain.Usage, costUSD float64) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(
		AttrPromptTokens.Int(usage.PromptTokens),
		AttrOutputTokens.Int(usage.CompletionTokens),
		AttrCostUSD.Float64(costUSD),
	)
}

// RecordFailover marks a span as having failed over.
//
// Set as an attribute as well as an event, because an attribute is filterable in Jaeger's UI
// and an event is not: `llmrouter.failover=true` is the query an operator actually types.
func RecordFailover(span trace.Span, from, to, reason string, attempt int) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(AttrFailover.Bool(true))
	span.AddEvent("failover", trace.WithAttributes(
		AttrFailoverFrom.String(from),
		AttrFailoverTo.String(to),
		AttrAttempt.Int(attempt),
		attribute.String("reason", reason),
	))
}

// RecordGuardrail annotates a span with the screening outcome.
func RecordGuardrail(span trace.Span, blocked bool, owasp string, failedOpen bool) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(AttrGuardBlocked.Bool(blocked))
	if owasp != "" {
		span.SetAttributes(AttrGuardOWASP.String(owasp))
	}
	if failedOpen {
		// A request that skipped screening is a security-relevant event, and it must be
		// findable in a trace search rather than only in a log line.
		span.SetAttributes(AttrFailedOpen.Bool(true))
		span.AddEvent("guardrails_failed_open")
	}
}

// RecordError marks a span as failed.
func RecordError(span trace.Span, err error) {
	if span == nil || !span.IsRecording() || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// TraceIDFrom returns the current trace id, for correlating a log line with a trace.
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
