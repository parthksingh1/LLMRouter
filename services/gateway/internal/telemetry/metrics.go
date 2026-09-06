// Package telemetry wires structured logging, Prometheus metrics and OpenTelemetry tracing.
//
// Metrics follow the RED method for the HTTP surface (rate, errors, duration) plus the
// domain-specific counters the dashboards and the resume claims depend on. Every collector is
// held on a struct rather than in package-level globals, so tests can build an isolated
// registry and assert on values without cross-test contamination.
package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the gateway's collector set.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inFlight        prometheus.Gauge

	upstreamDuration *prometheus.HistogramVec
	upstreamTotal    *prometheus.CounterVec

	cacheLookups    *prometheus.CounterVec
	cacheHitRatio   prometheus.Gauge
	cacheSimilarity prometheus.Histogram

	guardrailBlocks   *prometheus.CounterVec
	guardrailDuration prometheus.Histogram
	guardrailFailOpen prometheus.Counter

	failoverTotal *prometheus.CounterVec
	budgetBlocks  *prometheus.CounterVec

	tokensTotal *prometheus.CounterVec
	costTotal   *prometheus.CounterVec

	breakerState *prometheus.GaugeVec
	providerUp   *prometheus.GaugeVec

	eventsDropped prometheus.Counter
}

// latencyBuckets span a cached hit (single-digit milliseconds) to a long frontier-model
// generation (tens of seconds). The default Prometheus buckets top out at 10s, which would
// collapse the whole interesting tail of LLM traffic into +Inf.
var latencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 0.75,
	1, 1.5, 2, 3, 5, 8, 12, 20, 30, 60,
}

// guardrailBuckets are sub-millisecond-resolution because the service's whole claim is a p99
// under 12 ms; default buckets could not tell 3 ms from 11 ms.
var guardrailBuckets = []float64{
	0.0005, 0.001, 0.002, 0.003, 0.004, 0.006, 0.008, 0.010, 0.012, 0.015, 0.02, 0.05, 0.1,
}

// NewMetrics builds and registers every collector on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	m.requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_requests_total",
		Help: "Total HTTP requests by route, method and status class.",
	}, []string{"route", "method", "status"})

	m.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_request_duration_seconds",
		Help:    "End-to-end request duration, including upstream generation time.",
		Buckets: latencyBuckets,
	}, []string{"route", "cached"})

	m.inFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "llmrouter_requests_in_flight",
		Help: "Requests currently being served.",
	})

	m.upstreamDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmrouter_upstream_duration_seconds",
		Help:    "Time spent in a single upstream provider attempt.",
		Buckets: latencyBuckets,
	}, []string{"provider", "model", "outcome"})

	m.upstreamTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_upstream_attempts_total",
		Help: "Upstream provider attempts by outcome.",
	}, []string{"provider", "model", "outcome"})

	m.cacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_cache_lookups_total",
		Help: "Semantic cache lookups by result (hit, miss, error, disabled).",
	}, []string{"result"})

	m.cacheHitRatio = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "llmrouter_cache_hit_ratio",
		Help: "Rolling semantic cache hit ratio over the process lifetime.",
	})

	m.cacheSimilarity = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmrouter_cache_similarity",
		Help:    "Cosine similarity of the nearest neighbour on every cache lookup.",
		Buckets: prometheus.LinearBuckets(0.5, 0.025, 21),
	})

	m.guardrailBlocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_guardrail_block_total",
		Help: "Requests blocked by guardrails, by OWASP LLM category.",
	}, []string{"owasp", "detector"})

	m.guardrailDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmrouter_guardrail_duration_seconds",
		Help:    "Latency of the guardrails input screening call as observed by the gateway.",
		Buckets: guardrailBuckets,
	})

	m.guardrailFailOpen = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llmrouter_guardrail_fail_open_total",
		Help: "Requests allowed through because the guardrails sidecar was unreachable.",
	})

	m.failoverTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_failover_total",
		Help: "Provider failovers, by reason and whether the stream had already started.",
	}, []string{"from", "to", "reason", "midstream"})

	m.budgetBlocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_budget_block_total",
		Help: "Requests rejected or warned by the per-tenant budget.",
	}, []string{"tenant", "action"})

	m.tokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_tokens_total",
		Help: "Tokens consumed, by tenant, model and direction.",
	}, []string{"tenant", "model", "direction"})

	m.costTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmrouter_cost_usd_total",
		Help: "Attributed spend in USD, by tenant, provider and model.",
	}, []string{"tenant", "provider", "model"})

	m.breakerState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmrouter_circuit_breaker_state",
		Help: "Circuit breaker state per provider (0 closed, 1 half-open, 2 open).",
	}, []string{"provider"})

	m.providerUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmrouter_provider_up",
		Help: "Provider health as of the most recent health check (1 healthy, 0 unhealthy).",
	}, []string{"provider"})

	m.eventsDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llmrouter_events_dropped_total",
		Help: "Analytics events dropped because the sink buffer was full.",
	})

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requestsTotal, m.requestDuration, m.inFlight,
		m.upstreamDuration, m.upstreamTotal,
		m.cacheLookups, m.cacheHitRatio, m.cacheSimilarity,
		m.guardrailBlocks, m.guardrailDuration, m.guardrailFailOpen,
		m.failoverTotal, m.budgetBlocks,
		m.tokensTotal, m.costTotal,
		m.breakerState, m.providerUp, m.eventsDropped,
	)
	return m
}

// Handler serves the Prometheus scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          m.registry,
	})
}

// Registry exposes the underlying registry so tests can gather values.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// --- recording helpers -------------------------------------------------------
//
// Handlers call these rather than touching collectors directly, which keeps label cardinality
// decisions in one auditable place. Note that tenant ids are bounded by the tenant registry, so
// using them as a label is safe here; a public multi-tenant deployment would need to drop them.

// ObserveRequest records one completed HTTP request.
func (m *Metrics) ObserveRequest(route, method string, status int, cached bool, d time.Duration) {
	m.requestsTotal.WithLabelValues(route, method, statusClass(status)).Inc()
	m.requestDuration.WithLabelValues(route, strconv.FormatBool(cached)).Observe(d.Seconds())
}

// IncInFlight and DecInFlight bracket a request.
func (m *Metrics) IncInFlight() { m.inFlight.Inc() }

// DecInFlight decrements the in-flight gauge.
func (m *Metrics) DecInFlight() { m.inFlight.Dec() }

// ObserveUpstream records one provider attempt.
func (m *Metrics) ObserveUpstream(provider, model, outcome string, d time.Duration) {
	m.upstreamDuration.WithLabelValues(provider, model, outcome).Observe(d.Seconds())
	m.upstreamTotal.WithLabelValues(provider, model, outcome).Inc()
}

// ObserveCache records a lookup result and, when a neighbour was found, its similarity.
func (m *Metrics) ObserveCache(result string, similarity, hitRatio float64) {
	m.cacheLookups.WithLabelValues(result).Inc()
	if similarity > 0 {
		m.cacheSimilarity.Observe(similarity)
	}
	m.cacheHitRatio.Set(hitRatio)
}

// ObserveGuardrail records screening latency and any block.
func (m *Metrics) ObserveGuardrail(d time.Duration, blocked bool, owasp, detector string) {
	m.guardrailDuration.Observe(d.Seconds())
	if blocked {
		m.guardrailBlocks.WithLabelValues(owasp, detector).Inc()
	}
}

// GuardrailFailedOpen records that screening was skipped because the sidecar was unreachable.
func (m *Metrics) GuardrailFailedOpen() { m.guardrailFailOpen.Inc() }

// ObserveFailover records a provider failover.
func (m *Metrics) ObserveFailover(from, to, reason string, midstream bool) {
	m.failoverTotal.WithLabelValues(from, to, reason, strconv.FormatBool(midstream)).Inc()
}

// ObserveBudget records a budget warning or block.
func (m *Metrics) ObserveBudget(tenant, action string) {
	m.budgetBlocks.WithLabelValues(tenant, action).Inc()
}

// ObserveUsage records tokens and attributed cost for a completed request.
func (m *Metrics) ObserveUsage(tenant, provider, model string, promptTokens, completionTokens int, costUSD float64) {
	m.tokensTotal.WithLabelValues(tenant, model, "prompt").Add(float64(promptTokens))
	m.tokensTotal.WithLabelValues(tenant, model, "completion").Add(float64(completionTokens))
	m.costTotal.WithLabelValues(tenant, provider, model).Add(costUSD)
}

// SetProviderHealth publishes health and breaker state for one provider.
func (m *Metrics) SetProviderHealth(provider string, healthy bool, breaker string) {
	up := 0.0
	if healthy {
		up = 1
	}
	m.providerUp.WithLabelValues(provider).Set(up)

	state := 0.0
	switch breaker {
	case "half-open":
		state = 1
	case "open":
		state = 2
	}
	m.breakerState.WithLabelValues(provider).Set(state)
}

// EventDropped records an analytics event lost to backpressure.
func (m *Metrics) EventDropped() { m.eventsDropped.Inc() }

func statusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
