// Package config loads the gateway's configuration.
//
// Two sources, both read exactly once at startup:
//
//   - the process environment, for deployment-varying knobs (12-factor);
//   - YAML files, for the provider catalogue, routing policies and tenant registry, which are
//     operator-authored data rather than deployment settings.
//
// Nothing outside this package reads os.Getenv. The loaded structs are injected downwards.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved gateway configuration.
type Config struct {
	Env       string
	Log       LogConfig
	Server    ServerConfig
	Provider  ProviderRuntime
	Cache     CacheConfig
	Guard     GuardrailsConfig
	Budget    BudgetConfig
	Events    EventsConfig
	Telemetry TelemetryConfig

	// Paths to the YAML catalogues. Parsed by Load into the fields below.
	ProvidersPath string
	PoliciesPath  string
	TenantsPath   string

	Providers ProvidersFile
	Policies  PoliciesFile
	Tenants   TenantsFile
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string // debug | info | warn | error
	Format string // json | console
}

// ServerConfig is the HTTP surface.
type ServerConfig struct {
	Addr          string
	MetricsAddr   string
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	ShutdownGrace time.Duration
}

// ProviderRuntime decides how upstream calls behave.
type ProviderRuntime struct {
	// Mode is "mock" (deterministic fixtures, simulated latency, zero egress) or "live".
	Mode         string
	FixturesPath string
	APIKeys      map[string]string

	// Mock failure injection. These power make failover-demo and the failover benchmark.
	MockSeed                 int64
	MockFailAfter            map[string]int
	MockFailMidstream        map[string]bool
	MockMidstreamFailRate    float64
	MockMidstreamFailAfterTk int
	MockStallProbability     float64
}

// IsMock reports whether the mock harness is in use.
func (p ProviderRuntime) IsMock() bool { return p.Mode != "live" }

// CacheConfig is the semantic cache.
type CacheConfig struct {
	Enabled             bool
	SimilarityThreshold float64
	TTL                 time.Duration
	MaxPromptChars      int
	QdrantURL           string
	QdrantCollection    string
	RedisURL            string
	EmbedderURL         string
	EmbeddingDim        int
}

// GuardrailsConfig is the sidecar client.
type GuardrailsConfig struct {
	Enabled      bool
	URL          string
	Timeout      time.Duration
	FailMode     string // open | closed
	ScreenOutput bool
}

// FailOpen reports whether an unreachable sidecar should let the request through.
func (g GuardrailsConfig) FailOpen() bool { return g.FailMode != "closed" }

// BudgetConfig is the per-tenant token bucket.
type BudgetConfig struct {
	Enabled       bool
	RedisURL      string
	WarnThreshold float64
}

// EventsConfig is the ClickHouse analytics sink.
type EventsConfig struct {
	ClickHouseURL string
	Database      string
	User          string
	Password      string
	BatchSize     int
	FlushInterval time.Duration
}

// TelemetryConfig is OpenTelemetry wiring.
type TelemetryConfig struct {
	OTLPEndpoint string
	ServiceName  string
	SampleRatio  float64
}

// Load reads the environment and the YAML catalogues.
//
// It never falls back silently: a malformed value is an error, and a missing value uses the
// documented default from .env.example.
func Load() (*Config, error) {
	c := &Config{
		Env: env("LLMROUTER_ENV", "demo"),
		Log: LogConfig{
			Level:  env("LOG_LEVEL", "info"),
			Format: env("LOG_FORMAT", "json"),
		},
		ProvidersPath: env("PROVIDERS_CONFIG", "config/providers.yaml"),
		PoliciesPath:  env("POLICIES_CONFIG", "config/policies.yaml"),
		TenantsPath:   env("TENANTS_CONFIG", "config/tenants.yaml"),
	}

	var err error
	if c.Server, err = loadServer(); err != nil {
		return nil, err
	}
	if c.Provider, err = loadProvider(); err != nil {
		return nil, err
	}
	if c.Cache, err = loadCache(); err != nil {
		return nil, err
	}
	if c.Guard, err = loadGuardrails(); err != nil {
		return nil, err
	}
	if c.Budget, err = loadBudget(); err != nil {
		return nil, err
	}
	if c.Events, err = loadEvents(); err != nil {
		return nil, err
	}
	if c.Telemetry, err = loadTelemetry(); err != nil {
		return nil, err
	}

	if c.Providers, err = LoadProviders(c.ProvidersPath); err != nil {
		return nil, fmt.Errorf("loading providers: %w", err)
	}
	if c.Policies, err = LoadPolicies(c.PoliciesPath); err != nil {
		return nil, fmt.Errorf("loading policies: %w", err)
	}
	if c.Tenants, err = LoadTenants(c.TenantsPath); err != nil {
		return nil, fmt.Errorf("loading tenants: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks cross-cutting invariants that a single loader cannot see.
func (c *Config) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Provider.Mode {
	case "mock", "live":
	default:
		return fmt.Errorf("PROVIDER_MODE must be mock|live, got %q", c.Provider.Mode)
	}
	switch c.Guard.FailMode {
	case "open", "closed":
	default:
		return fmt.Errorf("GUARDRAILS_FAIL_MODE must be open|closed, got %q", c.Guard.FailMode)
	}
	if c.Cache.SimilarityThreshold <= 0 || c.Cache.SimilarityThreshold > 1 {
		return fmt.Errorf("CACHE_SIMILARITY_THRESHOLD must be in (0,1], got %v", c.Cache.SimilarityThreshold)
	}
	if c.Provider.Mode == "live" {
		for _, p := range c.Providers.Providers {
			if c.Provider.APIKeys[p.Name] == "" {
				return fmt.Errorf("PROVIDER_MODE=live but %s is unset for provider %q", p.APIKeyEnv, p.Name)
			}
		}
	}
	if len(c.Tenants.Tenants) == 0 {
		return fmt.Errorf("tenant registry %s contains no tenants", c.TenantsPath)
	}
	if _, ok := c.Policies.ByName(c.Policies.DefaultPolicy); !ok {
		return fmt.Errorf("default_policy %q is not defined in %s", c.Policies.DefaultPolicy, c.PoliciesPath)
	}
	return nil
}

func loadServer() (ServerConfig, error) {
	read, err := durationSec("GATEWAY_READ_TIMEOUT_SEC", 30)
	if err != nil {
		return ServerConfig{}, err
	}
	write, err := durationSec("GATEWAY_WRITE_TIMEOUT_SEC", 120)
	if err != nil {
		return ServerConfig{}, err
	}
	grace, err := durationSec("GATEWAY_SHUTDOWN_GRACE_SEC", 15)
	if err != nil {
		return ServerConfig{}, err
	}
	return ServerConfig{
		Addr:          env("GATEWAY_ADDR", ":8080"),
		MetricsAddr:   env("METRICS_ADDR", ":9090"),
		ReadTimeout:   read,
		WriteTimeout:  write,
		IdleTimeout:   90 * time.Second,
		ShutdownGrace: grace,
	}, nil
}

func loadProvider() (ProviderRuntime, error) {
	seed, err := intEnv("MOCK_SEED", 1337)
	if err != nil {
		return ProviderRuntime{}, err
	}
	rate, err := floatEnv("MOCK_MIDSTREAM_FAIL_RATE", 0)
	if err != nil {
		return ProviderRuntime{}, err
	}
	stall, err := floatEnv("MOCK_STALL_PROBABILITY", 0)
	if err != nil {
		return ProviderRuntime{}, err
	}
	failAfterTokens, err := intEnv("MOCK_MIDSTREAM_FAIL_AFTER_TOKENS", 24)
	if err != nil {
		return ProviderRuntime{}, err
	}

	p := ProviderRuntime{
		Mode:                     env("PROVIDER_MODE", "mock"),
		FixturesPath:             env("MOCK_FIXTURES", "seed/fixtures/responses.jsonl"),
		APIKeys:                  map[string]string{},
		MockSeed:                 int64(seed),
		MockFailAfter:            map[string]int{},
		MockFailMidstream:        map[string]bool{},
		MockMidstreamFailRate:    rate,
		MockMidstreamFailAfterTk: failAfterTokens,
		MockStallProbability:     stall,
	}

	// Per-provider knobs follow a naming convention so adding a provider needs no code change.
	for _, name := range []string{"openai", "anthropic", "google", "mistral", "together"} {
		up := strings.ToUpper(name)
		p.APIKeys[name] = os.Getenv(up + "_API_KEY")
		if v := os.Getenv("MOCK_" + up + "_FAIL_AFTER"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return ProviderRuntime{}, fmt.Errorf("MOCK_%s_FAIL_AFTER: %w", up, err)
			}
			p.MockFailAfter[name] = n
		}
		if v := os.Getenv("MOCK_" + up + "_FAIL_MIDSTREAM"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return ProviderRuntime{}, fmt.Errorf("MOCK_%s_FAIL_MIDSTREAM: %w", up, err)
			}
			p.MockFailMidstream[name] = b
		}
	}
	return p, nil
}

func loadCache() (CacheConfig, error) {
	enabled, err := boolEnv("CACHE_ENABLED", true)
	if err != nil {
		return CacheConfig{}, err
	}
	// The operating point from benchmarks/results/cache_calibration.json. It was 0.9124, which
	// predated the calibration and would have admitted far more than the zero-false-hit budget
	// the cache claims: the whole finding in ADR-0002 is that a bi-encoder needs a threshold
	// this high, because it scores negation-flipped minimal pairs above many real paraphrases.
	thr, err := floatEnv("CACHE_SIMILARITY_THRESHOLD", 0.9962)
	if err != nil {
		return CacheConfig{}, err
	}
	ttl, err := durationSec("CACHE_TTL_SEC", 86400)
	if err != nil {
		return CacheConfig{}, err
	}
	maxChars, err := intEnv("CACHE_MAX_PROMPT_CHARS", 8000)
	if err != nil {
		return CacheConfig{}, err
	}
	dim, err := intEnv("EMBEDDING_DIM", 384)
	if err != nil {
		return CacheConfig{}, err
	}
	return CacheConfig{
		Enabled:             enabled,
		SimilarityThreshold: thr,
		TTL:                 ttl,
		MaxPromptChars:      maxChars,
		QdrantURL:           env("QDRANT_URL", "http://qdrant:6333"),
		QdrantCollection:    env("QDRANT_COLLECTION", "llmrouter_cache"),
		RedisURL:            env("REDIS_URL", "redis://redis:6379/0"),
		EmbedderURL:         env("EMBEDDER_URL", "http://embedder:8000"),
		EmbeddingDim:        dim,
	}, nil
}

func loadGuardrails() (GuardrailsConfig, error) {
	enabled, err := boolEnv("GUARDRAILS_ENABLED", true)
	if err != nil {
		return GuardrailsConfig{}, err
	}
	timeoutMS, err := intEnv("GUARDRAILS_TIMEOUT_MS", 50)
	if err != nil {
		return GuardrailsConfig{}, err
	}
	screenOut, err := boolEnv("GUARDRAILS_SCREEN_OUTPUT", false)
	if err != nil {
		return GuardrailsConfig{}, err
	}
	return GuardrailsConfig{
		Enabled:      enabled,
		URL:          env("GUARDRAILS_URL", "http://guardrails:8000"),
		Timeout:      time.Duration(timeoutMS) * time.Millisecond,
		FailMode:     env("GUARDRAILS_FAIL_MODE", "open"),
		ScreenOutput: screenOut,
	}, nil
}

func loadBudget() (BudgetConfig, error) {
	enabled, err := boolEnv("BUDGET_ENABLED", true)
	if err != nil {
		return BudgetConfig{}, err
	}
	warn, err := floatEnv("BUDGET_WARN_THRESHOLD", 0.80)
	if err != nil {
		return BudgetConfig{}, err
	}
	return BudgetConfig{
		Enabled:       enabled,
		RedisURL:      env("BUDGET_REDIS_URL", "redis://redis:6379/1"),
		WarnThreshold: warn,
	}, nil
}

func loadEvents() (EventsConfig, error) {
	batch, err := intEnv("EVENTS_BATCH_SIZE", 500)
	if err != nil {
		return EventsConfig{}, err
	}
	flushMS, err := intEnv("EVENTS_FLUSH_INTERVAL_MS", 2000)
	if err != nil {
		return EventsConfig{}, err
	}
	return EventsConfig{
		ClickHouseURL: env("CLICKHOUSE_URL", "http://clickhouse:8123"),
		Database:      env("CLICKHOUSE_DB", "llmrouter"),
		User:          env("CLICKHOUSE_USER", "default"),
		Password:      os.Getenv("CLICKHOUSE_PASSWORD"),
		BatchSize:     batch,
		FlushInterval: time.Duration(flushMS) * time.Millisecond,
	}, nil
}

func loadTelemetry() (TelemetryConfig, error) {
	ratio, err := floatEnv("OTEL_TRACES_SAMPLER_ARG", 1.0)
	if err != nil {
		return TelemetryConfig{}, err
	}
	return TelemetryConfig{
		OTLPEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:  env("OTEL_SERVICE_NAME", "llmrouter-gateway"),
		SampleRatio:  ratio,
	}, nil
}

// --- small typed env helpers -------------------------------------------------

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func floatEnv(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func boolEnv(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func durationSec(key string, def int) (time.Duration, error) {
	n, err := intEnv(key, def)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be non-negative, got %d", key, n)
	}
	return time.Duration(n) * time.Second, nil
}
