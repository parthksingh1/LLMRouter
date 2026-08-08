// Package providers contains the adapters for upstream LLM vendors plus the registry that
// tracks their health.
//
// The registry owns three concerns that would otherwise be duplicated in every adapter:
// circuit breaking, background health checking, and observed-latency tracking. Adapters stay
// thin: they translate requests, nothing more.
package providers

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// BreakerState is the state of a provider's circuit breaker.
type BreakerState string

// Circuit breaker states.
const (
	// BreakerClosed is the healthy state: requests flow through.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen means the provider is being shunned after repeated failures.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen means a limited number of probe requests are allowed through.
	BreakerHalfOpen BreakerState = "half-open"
)

// Breaker is a small consecutive-failure circuit breaker.
//
// It is deliberately hand-rolled rather than pulled from a library: the behaviour is a dozen
// lines, and owning it means the failover benchmark can drive it deterministically with an
// injected clock. See docs/adr/0004-circuit-breaking.md.
type Breaker struct {
	mu sync.Mutex

	consecutiveFailures uint32
	maxFailures         uint32
	openDuration        time.Duration
	halfOpenProbes      uint32

	state      BreakerState
	openedAt   time.Time
	probesLeft uint32
	now        func() time.Time
}

// NewBreaker builds a breaker from configuration. now may be nil, in which case time.Now is used.
func NewBreaker(spec config.CircuitBreakerSpec, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	maxFailures := spec.ConsecutiveFailures
	if maxFailures == 0 {
		maxFailures = 5
	}
	probes := spec.HalfOpenProbes
	if probes == 0 {
		probes = 1
	}
	openFor := spec.OpenDuration()
	if openFor == 0 {
		openFor = 15 * time.Second
	}
	return &Breaker{
		maxFailures:    maxFailures,
		openDuration:   openFor,
		halfOpenProbes: probes,
		state:          BreakerClosed,
		now:            now,
	}
}

// Allow reports whether a request may be attempted, transitioning open -> half-open when the
// cool-off has elapsed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if b.now().Sub(b.openedAt) >= b.openDuration {
			b.state = BreakerHalfOpen
			b.probesLeft = b.halfOpenProbes
			return b.takeProbeLocked()
		}
		return false
	case BreakerHalfOpen:
		return b.takeProbeLocked()
	default:
		return true
	}
}

func (b *Breaker) takeProbeLocked() bool {
	if b.probesLeft == 0 {
		return false
	}
	b.probesLeft--
	return true
}

// Success records a successful call, closing the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures = 0
	b.state = BreakerClosed
}

// Failure records a failed call, opening the breaker once the threshold is crossed.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutiveFailures++
	if b.state == BreakerHalfOpen || b.consecutiveFailures >= b.maxFailures {
		b.state = BreakerOpen
		b.openedAt = b.now()
	}
}

// State returns the current breaker state.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Report half-open once the cool-off has passed, so /readyz and metrics agree with Allow.
	if b.state == BreakerOpen && b.now().Sub(b.openedAt) >= b.openDuration {
		return BreakerHalfOpen
	}
	return b.state
}

// --- registry ----------------------------------------------------------------

// entry is the registry's per-provider bookkeeping.
type entry struct {
	provider app.Provider
	breaker  *Breaker
	spec     config.ProviderSpec

	mu        sync.RWMutex
	healthy   bool
	lastErr   error
	lastCheck time.Time
	// ewmaTTFB is the exponentially weighted moving average of observed time-to-first-byte,
	// which latency_optimized routing prefers over the configured p50.
	ewmaTTFB float64
}

// Registry holds every configured provider and tracks whether it can be dispatched to.
//
// It satisfies app.ProviderRegistry.
type Registry struct {
	entries map[string]*entry
	order   []string
	log     *slog.Logger

	healthInterval time.Duration
	ewmaAlpha      float64

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// Compile-time assertions that Registry satisfies the ports its consumers declare.
var (
	_ app.ProviderRegistry = (*Registry)(nil)
	_ app.BreakerProvider  = (*Registry)(nil)
)

// NewRegistry builds a registry over already-constructed adapters.
//
// Providers are keyed by their Name(); a provider present in the catalogue but absent from
// adapters is an error, because silently routing around a missing adapter would hide a
// deployment mistake.
func NewRegistry(
	cfg config.ProvidersFile,
	adapters []app.Provider,
	log *slog.Logger,
	now func() time.Time,
) (*Registry, error) {
	byName := make(map[string]app.Provider, len(adapters))
	for _, a := range adapters {
		byName[a.Name()] = a
	}

	r := &Registry{
		entries:        make(map[string]*entry, len(cfg.Providers)),
		order:          make([]string, 0, len(cfg.Providers)),
		log:            log,
		healthInterval: cfg.Defaults.HealthInterval(),
		ewmaAlpha:      0.3,
	}
	if r.healthInterval <= 0 {
		r.healthInterval = 5 * time.Second
	}

	for _, spec := range cfg.Providers {
		a, ok := byName[spec.Name]
		if !ok {
			return nil, fmt.Errorf("provider %q is configured but no adapter was supplied", spec.Name)
		}
		r.entries[spec.Name] = &entry{
			provider: a,
			breaker:  NewBreaker(cfg.Defaults.CircuitBreaker, now),
			spec:     spec,
			// Start optimistic: the first health check runs immediately on Start, and refusing
			// all traffic until then would make cold starts look like outages.
			healthy:  true,
			ewmaTTFB: float64(spec.Latency.TTFBP50MS),
		}
		r.order = append(r.order, spec.Name)
	}
	return r, nil
}

// Get returns the adapter for a provider name.
func (r *Registry) Get(name string) (app.Provider, bool) {
	e, ok := r.entries[name]
	if !ok {
		return nil, false
	}
	return e.provider, true
}

// Names returns provider names in catalogue order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Healthy reports the result of the most recent health check.
func (r *Registry) Healthy(name string) bool {
	e, ok := r.entries[name]
	if !ok {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.healthy
}

// Available reports whether a provider may be dispatched to right now.
func (r *Registry) Available(name string) bool {
	e, ok := r.entries[name]
	if !ok {
		return false
	}
	e.mu.RLock()
	healthy := e.healthy
	e.mu.RUnlock()
	return healthy && e.breaker.State() != BreakerOpen
}

// Breaker exposes a provider's breaker so the dispatcher can record outcomes.
//
// It returns the app.Breaker interface rather than the concrete type so that Registry satisfies
// app.BreakerProvider, which the chat use case discovers via a type assertion. Returning the
// concrete *Breaker would silently fail that assertion.
func (r *Registry) Breaker(name string) (app.Breaker, bool) {
	e, ok := r.entries[name]
	if !ok {
		return nil, false
	}
	return e.breaker, true
}

// Spec returns the catalogue entry for a provider, which the failover machinery consults for
// prefix-continuation capability.
func (r *Registry) Spec(name string) (config.ProviderSpec, bool) {
	e, ok := r.entries[name]
	if !ok {
		return config.ProviderSpec{}, false
	}
	return e.spec, true
}

// ObserveTTFB folds an observed time-to-first-byte into the provider's moving average.
func (r *Registry) ObserveTTFB(name string, d time.Duration) {
	e, ok := r.entries[name]
	if !ok {
		return
	}
	ms := float64(d.Milliseconds())
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ewmaTTFB = r.ewmaAlpha*ms + (1-r.ewmaAlpha)*e.ewmaTTFB
}

// ObservedTTFBMillis returns the current moving average, falling back to the configured p50
// before any request has been observed.
func (r *Registry) ObservedTTFBMillis(name string) float64 {
	e, ok := r.entries[name]
	if !ok {
		return 0
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ewmaTTFB
}

// HealthSnapshot is a point-in-time view of one provider, used by /readyz and the dashboard.
type HealthSnapshot struct {
	Name      string       `json:"name"`
	Healthy   bool         `json:"healthy"`
	Breaker   BreakerState `json:"breaker"`
	TTFBMS    float64      `json:"observed_ttfb_ms"`
	LastError string       `json:"last_error,omitempty"`
	LastCheck time.Time    `json:"last_check,omitempty"`
}

// Snapshot returns the health of every provider, in catalogue order.
func (r *Registry) Snapshot() []HealthSnapshot {
	out := make([]HealthSnapshot, 0, len(r.order))
	for _, name := range r.order {
		e := r.entries[name]
		e.mu.RLock()
		snap := HealthSnapshot{
			Name:      name,
			Healthy:   e.healthy,
			Breaker:   e.breaker.State(),
			TTFBMS:    e.ewmaTTFB,
			LastCheck: e.lastCheck,
		}
		if e.lastErr != nil {
			snap.LastError = e.lastErr.Error()
		}
		e.mu.RUnlock()
		out = append(out, snap)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AnyHealthy reports whether at least one provider can serve traffic. /readyz uses this: a
// gateway with every provider down is not ready, but one degraded provider is not an outage.
func (r *Registry) AnyHealthy() bool {
	for _, name := range r.order {
		if r.Available(name) {
			return true
		}
	}
	return false
}

// Start launches one health-check goroutine per provider. It returns immediately; call Stop to
// shut them down. The first check for every provider runs synchronously so that a gateway that
// has just started reports accurate health.
func (r *Registry) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	for _, name := range r.order {
		e := r.entries[name]
		r.checkOnce(ctx, name, e)

		r.wg.Add(1)
		go func(name string, e *entry) {
			defer r.wg.Done()
			ticker := time.NewTicker(r.healthInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					r.checkOnce(ctx, name, e)
				}
			}
		}(name, e)
	}
}

// Stop halts the health checkers and waits for them to exit.
func (r *Registry) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}

func (r *Registry) checkOnce(ctx context.Context, name string, e *entry) {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	err := e.provider.HealthCheck(checkCtx)

	e.mu.Lock()
	was := e.healthy
	e.healthy = err == nil
	e.lastErr = err
	e.lastCheck = time.Now()
	e.mu.Unlock()

	if was != (err == nil) && r.log != nil {
		if err != nil {
			r.log.Warn("provider became unhealthy", "provider", name, "error", err)
		} else {
			r.log.Info("provider recovered", "provider", name)
		}
	}
}

// Descriptors returns every model offered by every registered provider.
func (r *Registry) Descriptors() []domain.ModelDescriptor {
	out := make([]domain.ModelDescriptor, 0, len(r.order)*3)
	for _, name := range r.order {
		out = append(out, r.entries[name].provider.Models()...)
	}
	return out
}
