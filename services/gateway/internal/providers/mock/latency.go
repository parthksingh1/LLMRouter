package mock

import (
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
)

// LatencyModel samples time-to-first-byte from a distribution fitted to the p50/p95/p99 given in
// config/providers.yaml.
//
// The fit is a piecewise-linear inverse CDF through the three configured quantiles, with an
// exponential tail beyond p99. That is cruder than fitting a log-normal, but it has a property
// that matters more here: the sampled p50, p95 and p99 come back equal to the configured values,
// so what an operator writes in the YAML is what the benchmark measures.
type LatencyModel struct {
	p50, p95, p99 float64 // milliseconds
	tokensPerSec  float64

	// speed divides every simulated duration. Tests set it high to run instantly; the demo
	// leaves it at 1 so measured latencies are meaningful.
	speed float64

	mu  sync.Mutex
	rng *rand.Rand
}

// NewLatencyModel builds a latency model for one provider.
func NewLatencyModel(spec config.LatencySpec, seed int64, speed float64) *LatencyModel {
	if speed <= 0 {
		speed = 1
	}
	m := &LatencyModel{
		p50:          float64(spec.TTFBP50MS),
		p95:          float64(spec.TTFBP95MS),
		p99:          float64(spec.TTFBP99MS),
		tokensPerSec: spec.TokensPerSec,
		speed:        speed,
		rng:          rand.New(rand.NewSource(seed)), //nolint:gosec // simulation, not crypto
	}
	if m.p50 <= 0 {
		m.p50 = 300
	}
	if m.p95 < m.p50 {
		m.p95 = m.p50 * 2
	}
	if m.p99 < m.p95 {
		m.p99 = m.p95 * 1.6
	}
	if m.tokensPerSec <= 0 {
		m.tokensPerSec = 60
	}
	return m
}

// TTFB samples one time-to-first-byte.
func (m *LatencyModel) TTFB() time.Duration {
	m.mu.Lock()
	u := m.rng.Float64()
	m.mu.Unlock()
	return m.scale(m.quantile(u))
}

// quantile is the inverse CDF described in the type comment.
func (m *LatencyModel) quantile(u float64) float64 {
	switch {
	case u <= 0:
		return m.p50 * 0.35
	case u < 0.5:
		// Below the median, interpolate from a floor at u=0 up to p50.
		lo := m.p50 * 0.35
		return lo + (m.p50-lo)*(u/0.5)
	case u < 0.95:
		return m.p50 + (m.p95-m.p50)*((u-0.5)/0.45)
	case u < 0.99:
		return m.p95 + (m.p99-m.p95)*((u-0.95)/0.04)
	default:
		// Exponential tail so the worst case is heavy but bounded in practice.
		excess := (u - 0.99) / 0.01
		return m.p99 * (1 + 0.9*math.Pow(excess, 1.5))
	}
}

// InterTokenDelay is how long to wait between streamed tokens.
func (m *LatencyModel) InterTokenDelay() time.Duration {
	return m.scale(1000.0 / m.tokensPerSec)
}

// GenerationTime is the simulated wall time to produce n completion tokens, excluding TTFB.
func (m *LatencyModel) GenerationTime(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return m.scale(float64(n) * 1000.0 / m.tokensPerSec)
}

// Float returns a uniform random number, used for failure injection so that every source of
// randomness in the mock shares one seeded stream.
func (m *LatencyModel) Float() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rng.Float64()
}

func (m *LatencyModel) scale(ms float64) time.Duration {
	return time.Duration(ms / m.speed * float64(time.Millisecond))
}

// sleepCtx sleeps for d unless ctx is cancelled first. It returns false if the context ended.
func sleepCtx(done <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return false
	case <-t.C:
		return true
	}
}
