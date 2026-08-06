package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
)

// repoConfig points at the real configuration shipped with the repository. Loading it in tests
// is deliberate: it turns "did someone break config/providers.yaml" into a unit-test failure
// rather than a runtime one discovered during a demo.
func repoConfig(name string) string {
	return filepath.Join("..", "..", "..", "..", "config", name)
}

func TestLoadProvidersFromRepoCatalogue(t *testing.T) {
	t.Parallel()

	f, err := config.LoadProviders(repoConfig("providers.yaml"))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}

	if len(f.Providers) != 5 {
		t.Errorf("expected 5 providers, got %d", len(f.Providers))
	}

	wantProviders := []string{"openai", "anthropic", "google", "mistral", "together"}
	for _, want := range wantProviders {
		if _, ok := f.Provider(want); !ok {
			t.Errorf("provider %q missing from the catalogue", want)
		}
	}

	// Every chat model needs pricing and a quality score, or cost_optimized and quality_tiered
	// routing silently degenerate to picking the first candidate.
	for _, d := range f.Descriptors() {
		if d.PriceInPerM <= 0 {
			t.Errorf("model %s has no input price", d.ID)
		}
		if d.Quality <= 0 || d.Quality > 1 {
			t.Errorf("model %s has an out-of-range quality score %v", d.ID, d.Quality)
		}
		if d.ContextWindow <= 0 {
			t.Errorf("model %s has no context window", d.ID)
		}
	}

	if _, ok := f.FindModel("gpt-4o"); !ok {
		t.Error("FindModel could not locate gpt-4o")
	}
	if _, ok := f.FindModel("does-not-exist"); ok {
		t.Error("FindModel returned a model that is not in the catalogue")
	}
}

func TestProviderDefaultsAreUsable(t *testing.T) {
	t.Parallel()

	f, err := config.LoadProviders(repoConfig("providers.yaml"))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	d := f.Defaults

	if d.StallTimeout() <= 0 {
		t.Error("stream_stall_timeout_ms must be positive or mid-stream failover can never trigger")
	}
	if d.RequestTimeout() <= d.StallTimeout() {
		t.Error("the request timeout must exceed the stall timeout, or a stall can never be observed")
	}
	if d.CircuitBreaker.ConsecutiveFailures == 0 {
		t.Error("circuit breaker threshold must be set")
	}
	if d.HealthInterval() <= 0 {
		t.Error("health check interval must be positive")
	}
}

func TestLoadPoliciesFromRepoCatalogue(t *testing.T) {
	t.Parallel()

	f, err := config.LoadPolicies(repoConfig("policies.yaml"))
	if err != nil {
		t.Fatalf("LoadPolicies: %v", err)
	}

	wantStrategies := []string{
		"cost_optimized", "latency_optimized", "quality_tiered",
		"weighted_round_robin", "canary",
	}
	for _, name := range wantStrategies {
		p, ok := f.ByName(name)
		if !ok {
			t.Fatalf("policy %q is missing", name)
		}
		if p.Strategy != name {
			t.Errorf("policy %q declares strategy %q", name, p.Strategy)
		}
	}

	if _, ok := f.ByName(f.DefaultPolicy); !ok {
		t.Errorf("default_policy %q is not defined", f.DefaultPolicy)
	}

	// Every virtual alias must resolve, or "auto" would 404 at runtime.
	for alias, policy := range f.VirtualModels {
		if _, ok := f.ByName(policy); !ok {
			t.Errorf("virtual model %q maps to unknown policy %q", alias, policy)
		}
	}
	if got, ok := f.ResolveVirtual("auto"); !ok || got == "" {
		t.Error(`"auto" must resolve to a policy`)
	}

	tiered, _ := f.ByName("quality_tiered")
	if len(tiered.Params.Buckets) != 3 {
		t.Errorf("quality_tiered expects 3 difficulty buckets, got %d", len(tiered.Params.Buckets))
	}
	if len(tiered.Params.Classifier.HardKeywords) == 0 {
		t.Error("quality_tiered classifier has no hard keywords")
	}
	th := tiered.Params.Classifier.Thresholds
	if th.EasyBelow >= th.HardAbove {
		t.Errorf("classifier thresholds overlap: easy_below=%v hard_above=%v", th.EasyBelow, th.HardAbove)
	}
}

func TestLoadTenantsAppliesDefaults(t *testing.T) {
	t.Parallel()

	f, err := config.LoadTenants(repoConfig("tenants.yaml"))
	if err != nil {
		t.Fatalf("LoadTenants: %v", err)
	}
	if len(f.Tenants) == 0 {
		t.Fatal("no tenants loaded")
	}

	for _, tn := range f.Tenants {
		if tn.DefaultPolicy == "" {
			t.Errorf("tenant %s has no default policy after defaults were applied", tn.ID)
		}
		if tn.DailyTokenBudget <= 0 {
			t.Errorf("tenant %s has no daily token budget", tn.ID)
		}
		if tn.WarnThreshold <= 0 || tn.WarnThreshold > 1 {
			t.Errorf("tenant %s has an out-of-range warn threshold %v", tn.ID, tn.WarnThreshold)
		}

		// The raw key must never survive conversion into the domain entity.
		d := tn.Domain()
		if strings.Contains(d.APIKeyHash, tn.APIKey) {
			t.Errorf("tenant %s: the API key leaked into the domain entity", tn.ID)
		}
		if len(d.APIKeyHash) != 64 {
			t.Errorf("tenant %s: expected a sha256 hex digest, got %q", tn.ID, d.APIKeyHash)
		}
	}
}

func TestCatalogueLoadersRejectBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		body    string
		load    func(string) error
		wantErr string
	}{
		{
			name: "providers: duplicate provider name",
			file: "providers.yaml",
			body: `
version: 1
providers:
  - name: openai
    prefix_continuation: prefill
    models: [{id: a, price_in_per_m: 1, price_out_per_m: 1}]
  - name: openai
    prefix_continuation: prefill
    models: [{id: b, price_in_per_m: 1, price_out_per_m: 1}]
`,
			load:    func(p string) error { _, err := config.LoadProviders(p); return err },
			wantErr: "duplicate provider",
		},
		{
			name: "providers: unknown prefix continuation mode",
			file: "providers.yaml",
			body: `
version: 1
providers:
  - name: openai
    prefix_continuation: telepathy
    models: [{id: a, price_in_per_m: 1, price_out_per_m: 1}]
`,
			load:    func(p string) error { _, err := config.LoadProviders(p); return err },
			wantErr: "invalid prefix_continuation",
		},
		{
			name: "providers: a provider with no models",
			file: "providers.yaml",
			body: `
version: 1
providers:
  - name: openai
    prefix_continuation: none
    models: []
`,
			load:    func(p string) error { _, err := config.LoadProviders(p); return err },
			wantErr: "offers no models",
		},
		{
			name:    "policies: empty file",
			file:    "policies.yaml",
			body:    "version: 1\n",
			load:    func(p string) error { _, err := config.LoadPolicies(p); return err },
			wantErr: "no policies defined",
		},
		{
			name: "policies: virtual model pointing at nothing",
			file: "policies.yaml",
			body: `
version: 1
default_policy: a
virtual_models: {auto: ghost}
policies:
  - {name: a, strategy: cost_optimized}
`,
			load:    func(p string) error { _, err := config.LoadPolicies(p); return err },
			wantErr: "unknown policy",
		},
		{
			name: "tenants: duplicate api key",
			file: "tenants.yaml",
			body: `
version: 1
tenants:
  - {id: a, api_key: same}
  - {id: b, api_key: same}
`,
			load:    func(p string) error { _, err := config.LoadTenants(p); return err },
			wantErr: "duplicate api_key",
		},
		{
			name: "tenants: missing api key",
			file: "tenants.yaml",
			body: `
version: 1
tenants:
  - {id: a}
`,
			load:    func(p string) error { _, err := config.LoadTenants(p); return err },
			wantErr: "needs both",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTemp(t, tc.file, tc.body)
			err := tc.load(path)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadMissingFileIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := config.LoadProviders(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("expected an error for a missing providers file")
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	base := func() *config.Config {
		providers, err := config.LoadProviders(repoConfig("providers.yaml"))
		if err != nil {
			t.Fatalf("LoadProviders: %v", err)
		}
		policies, err := config.LoadPolicies(repoConfig("policies.yaml"))
		if err != nil {
			t.Fatalf("LoadPolicies: %v", err)
		}
		tenants, err := config.LoadTenants(repoConfig("tenants.yaml"))
		if err != nil {
			t.Fatalf("LoadTenants: %v", err)
		}
		return &config.Config{
			Log:       config.LogConfig{Level: "info", Format: "json"},
			Provider:  config.ProviderRuntime{Mode: "mock", APIKeys: map[string]string{}},
			Cache:     config.CacheConfig{SimilarityThreshold: 0.9},
			Guard:     config.GuardrailsConfig{FailMode: "open"},
			Providers: providers,
			Policies:  policies,
			Tenants:   tenants,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{name: "the shipped configuration is valid", mutate: func(*config.Config) {}},
		{
			name:    "unknown log level",
			mutate:  func(c *config.Config) { c.Log.Level = "verbose" },
			wantErr: "LOG_LEVEL",
		},
		{
			name:    "unknown provider mode",
			mutate:  func(c *config.Config) { c.Provider.Mode = "hybrid" },
			wantErr: "PROVIDER_MODE",
		},
		{
			name:    "unknown guardrail fail mode",
			mutate:  func(c *config.Config) { c.Guard.FailMode = "maybe" },
			wantErr: "GUARDRAILS_FAIL_MODE",
		},
		{
			name:    "similarity threshold out of range",
			mutate:  func(c *config.Config) { c.Cache.SimilarityThreshold = 1.5 },
			wantErr: "CACHE_SIMILARITY_THRESHOLD",
		},
		{
			name:    "live mode without api keys is refused at startup",
			mutate:  func(c *config.Config) { c.Provider.Mode = "live" },
			wantErr: "PROVIDER_MODE=live",
		},
		{
			name:    "default policy must exist",
			mutate:  func(c *config.Config) { c.Policies.DefaultPolicy = "ghost" },
			wantErr: "default_policy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base()
			tc.mutate(c)
			err := c.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	// Not parallel: it mutates the process environment.
	t.Setenv("PROVIDERS_CONFIG", repoConfig("providers.yaml"))
	t.Setenv("POLICIES_CONFIG", repoConfig("policies.yaml"))
	t.Setenv("TENANTS_CONFIG", repoConfig("tenants.yaml"))
	t.Setenv("GATEWAY_ADDR", ":9999")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("CACHE_SIMILARITY_THRESHOLD", "0.87")
	t.Setenv("GUARDRAILS_TIMEOUT_MS", "25")
	t.Setenv("MOCK_OPENAI_FAIL_AFTER", "3")
	t.Setenv("MOCK_ANTHROPIC_FAIL_MIDSTREAM", "true")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != ":9999" {
		t.Errorf("Server.Addr = %q, want :9999", cfg.Server.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want debug", cfg.Log.Level)
	}
	if cfg.Cache.SimilarityThreshold != 0.87 {
		t.Errorf("SimilarityThreshold = %v, want 0.87", cfg.Cache.SimilarityThreshold)
	}
	if cfg.Guard.Timeout.Milliseconds() != 25 {
		t.Errorf("Guard.Timeout = %v, want 25ms", cfg.Guard.Timeout)
	}
	if cfg.Provider.MockFailAfter["openai"] != 3 {
		t.Errorf("MockFailAfter[openai] = %d, want 3", cfg.Provider.MockFailAfter["openai"])
	}
	if !cfg.Provider.MockFailMidstream["anthropic"] {
		t.Error("MockFailMidstream[anthropic] should be true")
	}
	if !cfg.Provider.IsMock() {
		t.Error("the default provider mode should be mock, so the demo needs no keys")
	}
}

func TestLoadRejectsMalformedEnvValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "non-numeric timeout", key: "GATEWAY_READ_TIMEOUT_SEC", value: "thirty"},
		{name: "negative timeout", key: "GATEWAY_READ_TIMEOUT_SEC", value: "-1"},
		{name: "non-numeric threshold", key: "CACHE_SIMILARITY_THRESHOLD", value: "high"},
		{name: "non-boolean flag", key: "CACHE_ENABLED", value: "yes-please"},
		{name: "non-numeric fail-after", key: "MOCK_OPENAI_FAIL_AFTER", value: "soon"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROVIDERS_CONFIG", repoConfig("providers.yaml"))
			t.Setenv("POLICIES_CONFIG", repoConfig("policies.yaml"))
			t.Setenv("TENANTS_CONFIG", repoConfig("tenants.yaml"))
			t.Setenv(tc.key, tc.value)

			if _, err := config.Load(); err == nil {
				t.Fatalf("expected Load to reject %s=%q", tc.key, tc.value)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q should name the offending variable %s", err, tc.key)
			}
		})
	}
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}
