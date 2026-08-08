package providers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers"
)

// fakeClock lets breaker tests advance time without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func breakerSpec() config.CircuitBreakerSpec {
	return config.CircuitBreakerSpec{
		ConsecutiveFailures: 3,
		OpenDurationMS:      1000,
		HalfOpenProbes:      1,
	}
}

func TestBreakerStateMachine(t *testing.T) {
	t.Parallel()

	type step struct {
		action    string // allow | success | failure | advance
		advance   time.Duration
		wantAllow bool
		wantState providers.BreakerState
	}

	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "stays closed while calls succeed",
			steps: []step{
				{action: "allow", wantAllow: true},
				{action: "success", wantState: providers.BreakerClosed},
				{action: "allow", wantAllow: true},
			},
		},
		{
			name: "isolated failures below the threshold do not open it",
			steps: []step{
				{action: "failure"},
				{action: "failure", wantState: providers.BreakerClosed},
				{action: "allow", wantAllow: true},
			},
		},
		{
			name: "a success resets the consecutive failure count",
			steps: []step{
				{action: "failure"},
				{action: "failure"},
				{action: "success"},
				{action: "failure"},
				{action: "failure", wantState: providers.BreakerClosed},
			},
		},
		{
			name: "opens on the third consecutive failure and refuses traffic",
			steps: []step{
				{action: "failure"},
				{action: "failure"},
				{action: "failure", wantState: providers.BreakerOpen},
				{action: "allow", wantAllow: false},
			},
		},
		{
			name: "moves to half-open after the cool-off and admits one probe",
			steps: []step{
				{action: "failure"},
				{action: "failure"},
				{action: "failure"},
				{action: "advance", advance: 1100 * time.Millisecond},
				{action: "allow", wantAllow: true},  // the probe
				{action: "allow", wantAllow: false}, // budget of one is spent
			},
		},
		{
			name: "a failed probe re-opens the breaker immediately",
			steps: []step{
				{action: "failure"},
				{action: "failure"},
				{action: "failure"},
				{action: "advance", advance: 1100 * time.Millisecond},
				{action: "allow", wantAllow: true},
				{action: "failure", wantState: providers.BreakerOpen},
				{action: "allow", wantAllow: false},
			},
		},
		{
			name: "a successful probe closes the breaker",
			steps: []step{
				{action: "failure"},
				{action: "failure"},
				{action: "failure"},
				{action: "advance", advance: 1100 * time.Millisecond},
				{action: "allow", wantAllow: true},
				{action: "success", wantState: providers.BreakerClosed},
				{action: "allow", wantAllow: true},
				{action: "allow", wantAllow: true},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newFakeClock()
			b := providers.NewBreaker(breakerSpec(), clock.Now)

			for i, s := range tc.steps {
				switch s.action {
				case "allow":
					if got := b.Allow(); got != s.wantAllow {
						t.Fatalf("step %d: Allow() = %v, want %v (state %s)", i, got, s.wantAllow, b.State())
					}
				case "success":
					b.Success()
				case "failure":
					b.Failure()
				case "advance":
					clock.Advance(s.advance)
				}
				if s.wantState != "" {
					if got := b.State(); got != s.wantState {
						t.Fatalf("step %d: State() = %s, want %s", i, got, s.wantState)
					}
				}
			}
		})
	}
}

func TestBreakerDefaultsWhenSpecIsEmpty(t *testing.T) {
	t.Parallel()

	b := providers.NewBreaker(config.CircuitBreakerSpec{}, nil)
	if !b.Allow() {
		t.Fatal("a fresh breaker must allow traffic")
	}
	for i := 0; i < 5; i++ {
		b.Failure()
	}
	if got := b.State(); got != providers.BreakerOpen {
		t.Errorf("State() = %s, want open after the default 5 failures", got)
	}
}

// stubProvider is a minimal app.Provider for registry tests.
type stubProvider struct {
	name string

	mu      sync.Mutex
	healthy bool
	checks  int
	models  []domain.ModelDescriptor
}

func newStub(name string, healthy bool) *stubProvider {
	return &stubProvider{
		name:    name,
		healthy: healthy,
		models:  []domain.ModelDescriptor{{ID: name + "-model", Provider: name}},
	}
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Models() []domain.ModelDescriptor { return s.models }

func (s *stubProvider) Chat(context.Context, domain.ChatRequest) (domain.ChatResponse, error) {
	return domain.ChatResponse{}, nil
}

func (s *stubProvider) ChatStream(context.Context, domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk)
	close(ch)
	return ch, nil
}

func (s *stubProvider) Embed(context.Context, domain.EmbedRequest) (domain.EmbedResponse, error) {
	return domain.EmbedResponse{}, nil
}

func (s *stubProvider) HealthCheck(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	if s.healthy {
		return nil
	}
	return errors.New("stub is unhealthy")
}

func (s *stubProvider) setHealthy(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy = v
}

func (s *stubProvider) checkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks
}

func testCatalogue(names ...string) config.ProvidersFile {
	f := config.ProvidersFile{
		Defaults: config.ProviderDefaults{
			CircuitBreaker:        breakerSpec(),
			HealthCheckIntervalMS: 10,
			StreamStallTimeoutMS:  1000,
			RequestTimeoutMS:      5000,
		},
	}
	for _, n := range names {
		f.Providers = append(f.Providers, config.ProviderSpec{
			Name:               n,
			PrefixContinuation: config.PrefixNone,
			Latency:            config.LatencySpec{TTFBP50MS: 100, TTFBP95MS: 200, TTFBP99MS: 400, TokensPerSec: 50},
			Models:             []config.ModelSpec{{ID: n + "-model", PriceInPerM: 1, PriceOutPerM: 1, Quality: 0.9}},
		})
	}
	return f
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewRegistryRequiresAnAdapterPerProvider(t *testing.T) {
	t.Parallel()

	_, err := providers.NewRegistry(
		testCatalogue("openai", "anthropic"),
		[]app.Provider{newStub("openai", true)},
		quietLogger(),
		time.Now,
	)
	if err == nil {
		t.Fatal("expected an error when a configured provider has no adapter")
	}
}

func TestRegistryTracksHealth(t *testing.T) {
	t.Parallel()

	good := newStub("openai", true)
	bad := newStub("anthropic", false)

	reg, err := providers.NewRegistry(
		testCatalogue("openai", "anthropic"),
		[]app.Provider{good, bad},
		quietLogger(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Start(ctx)
	defer reg.Stop()

	// Start runs the first check synchronously, so health is accurate immediately.
	if !reg.Healthy("openai") {
		t.Error("openai should be healthy")
	}
	if reg.Healthy("anthropic") {
		t.Error("anthropic should be unhealthy")
	}
	if !reg.AnyHealthy() {
		t.Error("AnyHealthy should be true while one provider is up")
	}

	if _, ok := reg.Get("openai"); !ok {
		t.Error("Get(openai) failed")
	}
	if _, ok := reg.Get("ghost"); ok {
		t.Error("Get returned a provider that was never registered")
	}
	if got := len(reg.Names()); got != 2 {
		t.Errorf("Names() returned %d entries, want 2", got)
	}
	if got := len(reg.Descriptors()); got != 2 {
		t.Errorf("Descriptors() returned %d models, want 2", got)
	}

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot() returned %d entries, want 2", len(snap))
	}
	for _, s := range snap {
		if s.Name == "anthropic" && s.LastError == "" {
			t.Error("an unhealthy provider should carry its last error in the snapshot")
		}
	}
}

func TestRegistryRecoversWhenAProviderComesBack(t *testing.T) {
	t.Parallel()

	p := newStub("openai", false)
	reg, err := providers.NewRegistry(testCatalogue("openai"), []app.Provider{p}, quietLogger(), time.Now)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Start(ctx)
	defer reg.Stop()

	if reg.Healthy("openai") {
		t.Fatal("provider should start unhealthy")
	}
	p.setHealthy(true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Healthy("openai") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("provider never recovered after %d health checks", p.checkCount())
}

func TestRegistryAvailabilityRespectsTheBreaker(t *testing.T) {
	t.Parallel()

	reg, err := providers.NewRegistry(
		testCatalogue("openai"),
		[]app.Provider{newStub("openai", true)},
		quietLogger(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Start(ctx)
	defer reg.Stop()

	if !reg.Available("openai") {
		t.Fatal("a healthy provider with a closed breaker must be available")
	}

	b, ok := reg.Breaker("openai")
	if !ok {
		t.Fatal("Breaker(openai) not found")
	}
	for i := 0; i < 3; i++ {
		b.Failure()
	}

	if reg.Available("openai") {
		t.Error("a provider with an open breaker must not be available")
	}
	if !reg.Healthy("openai") {
		t.Error("an open breaker must not be conflated with a failed health check")
	}
	if reg.AnyHealthy() {
		t.Error("AnyHealthy should be false when the only provider is breaker-open")
	}
}

func TestRegistryObservedTTFB(t *testing.T) {
	t.Parallel()

	reg, err := providers.NewRegistry(
		testCatalogue("openai"),
		[]app.Provider{newStub("openai", true)},
		quietLogger(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Seeded from the configured p50 so latency routing has a sane value before any traffic.
	if got := reg.ObservedTTFBMillis("openai"); got != 100 {
		t.Errorf("initial observed TTFB = %v, want the configured p50 of 100", got)
	}

	// The EWMA must move towards observations without jumping straight to them.
	reg.ObserveTTFB("openai", 500*time.Millisecond)
	got := reg.ObservedTTFBMillis("openai")
	if got <= 100 || got >= 500 {
		t.Errorf("observed TTFB = %v, want a value between the old average and the observation", got)
	}

	for i := 0; i < 50; i++ {
		reg.ObserveTTFB("openai", 500*time.Millisecond)
	}
	if got := reg.ObservedTTFBMillis("openai"); got < 495 {
		t.Errorf("observed TTFB = %v, want convergence towards 500", got)
	}
}

func TestRegistrySpecExposesFailoverCapability(t *testing.T) {
	t.Parallel()

	reg, err := providers.NewRegistry(
		testCatalogue("openai"),
		[]app.Provider{newStub("openai", true)},
		quietLogger(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	spec, ok := reg.Spec("openai")
	if !ok {
		t.Fatal("Spec(openai) not found")
	}
	if spec.PrefixContinuation != config.PrefixNone {
		t.Errorf("PrefixContinuation = %q, want %q", spec.PrefixContinuation, config.PrefixNone)
	}
	if _, ok := reg.Spec("ghost"); ok {
		t.Error("Spec returned an unregistered provider")
	}
}

func TestRegistryStopIsIdempotentWithoutStart(t *testing.T) {
	t.Parallel()

	reg, err := providers.NewRegistry(
		testCatalogue("openai"),
		[]app.Provider{newStub("openai", true)},
		quietLogger(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Stopping a registry that was never started must not panic; shutdown paths run this way
	// when start-up fails partway through.
	reg.Stop()
}
