package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// --- providers.yaml ----------------------------------------------------------

// ProvidersFile mirrors config/providers.yaml.
type ProvidersFile struct {
	Version   int              `yaml:"version"`
	Defaults  ProviderDefaults `yaml:"defaults"`
	Providers []ProviderSpec   `yaml:"providers"`
}

// ProviderDefaults are settings shared by every provider.
type ProviderDefaults struct {
	StreamStallTimeoutMS  int                `yaml:"stream_stall_timeout_ms"`
	RequestTimeoutMS      int                `yaml:"request_timeout_ms"`
	CircuitBreaker        CircuitBreakerSpec `yaml:"circuit_breaker"`
	HealthCheckIntervalMS int                `yaml:"health_check_interval_ms"`
}

// StallTimeout is the configured stream stall timeout as a duration.
func (d ProviderDefaults) StallTimeout() time.Duration {
	return time.Duration(d.StreamStallTimeoutMS) * time.Millisecond
}

// RequestTimeout is the configured per-request timeout as a duration.
func (d ProviderDefaults) RequestTimeout() time.Duration {
	return time.Duration(d.RequestTimeoutMS) * time.Millisecond
}

// HealthInterval is the health-check period as a duration.
func (d ProviderDefaults) HealthInterval() time.Duration {
	return time.Duration(d.HealthCheckIntervalMS) * time.Millisecond
}

// CircuitBreakerSpec configures the per-provider breaker.
type CircuitBreakerSpec struct {
	ConsecutiveFailures uint32 `yaml:"consecutive_failures"`
	OpenDurationMS      int    `yaml:"open_duration_ms"`
	HalfOpenProbes      uint32 `yaml:"half_open_probes"`
}

// OpenDuration is how long the breaker stays open.
func (c CircuitBreakerSpec) OpenDuration() time.Duration {
	return time.Duration(c.OpenDurationMS) * time.Millisecond
}

// PrefixMode describes how a provider can resume from a partial assistant turn.
type PrefixMode string

// Prefix continuation capabilities. See docs/adr/0003-failover-semantics.md.
const (
	// PrefixNative means the API accepts a trailing assistant message and continues it.
	PrefixNative PrefixMode = "native"
	// PrefixPrefill means continuation works via an assistant-prefill trick, with caveats.
	PrefixPrefill PrefixMode = "prefill"
	// PrefixNone means the provider cannot continue; failover must restart the generation.
	PrefixNone PrefixMode = "none"
)

// ProviderSpec is one entry under `providers:`.
type ProviderSpec struct {
	Name               string      `yaml:"name"`
	BaseURL            string      `yaml:"base_url"`
	APIKeyEnv          string      `yaml:"api_key_env"`
	PrefixContinuation PrefixMode  `yaml:"prefix_continuation"`
	Latency            LatencySpec `yaml:"latency"`
	Models             []ModelSpec `yaml:"models"`
}

// LatencySpec is the synthetic latency distribution used by the mock harness.
type LatencySpec struct {
	TTFBP50MS    int     `yaml:"ttfb_p50_ms"`
	TTFBP95MS    int     `yaml:"ttfb_p95_ms"`
	TTFBP99MS    int     `yaml:"ttfb_p99_ms"`
	TokensPerSec float64 `yaml:"tokens_per_sec"`
}

// ModelSpec is one model offered by a provider.
type ModelSpec struct {
	ID           string  `yaml:"id"`
	Family       string  `yaml:"family"`
	Tier         string  `yaml:"tier"`
	Quality      float64 `yaml:"quality"`
	Context      int     `yaml:"context"`
	PriceInPerM  float64 `yaml:"price_in_per_m"`
	PriceOutPerM float64 `yaml:"price_out_per_m"`
}

// Descriptor converts a ModelSpec into the domain type.
func (m ModelSpec) Descriptor(provider string) domain.ModelDescriptor {
	return domain.ModelDescriptor{
		ID:            m.ID,
		Provider:      provider,
		Family:        m.Family,
		Tier:          m.Tier,
		Quality:       m.Quality,
		ContextWindow: m.Context,
		PriceInPerM:   m.PriceInPerM,
		PriceOutPerM:  m.PriceOutPerM,
	}
}

// LoadProviders parses config/providers.yaml.
func LoadProviders(path string) (ProvidersFile, error) {
	var f ProvidersFile
	if err := readYAML(path, &f); err != nil {
		return f, err
	}
	if len(f.Providers) == 0 {
		return f, fmt.Errorf("%s: no providers defined", path)
	}
	seen := map[string]bool{}
	for _, p := range f.Providers {
		if p.Name == "" {
			return f, fmt.Errorf("%s: a provider is missing `name`", path)
		}
		if seen[p.Name] {
			return f, fmt.Errorf("%s: duplicate provider %q", path, p.Name)
		}
		seen[p.Name] = true
		switch p.PrefixContinuation {
		case PrefixNative, PrefixPrefill, PrefixNone:
		default:
			return f, fmt.Errorf("%s: provider %q has invalid prefix_continuation %q",
				path, p.Name, p.PrefixContinuation)
		}
		if len(p.Models) == 0 {
			return f, fmt.Errorf("%s: provider %q offers no models", path, p.Name)
		}
		for _, m := range p.Models {
			if m.ID == "" {
				return f, fmt.Errorf("%s: provider %q has a model with no id", path, p.Name)
			}
			if m.PriceInPerM < 0 || m.PriceOutPerM < 0 {
				return f, fmt.Errorf("%s: model %q has negative pricing", path, m.ID)
			}
		}
	}
	return f, nil
}

// Descriptors flattens every chat-capable model in the catalogue.
func (f ProvidersFile) Descriptors() []domain.ModelDescriptor {
	out := make([]domain.ModelDescriptor, 0, len(f.Providers)*3)
	for _, p := range f.Providers {
		for _, m := range p.Models {
			if m.Tier == "embedding" {
				continue
			}
			out = append(out, m.Descriptor(p.Name))
		}
	}
	return out
}

// Provider looks up one provider spec by name.
func (f ProvidersFile) Provider(name string) (ProviderSpec, bool) {
	for _, p := range f.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return ProviderSpec{}, false
}

// FindModel locates a concrete model id anywhere in the catalogue.
func (f ProvidersFile) FindModel(id string) (domain.ModelDescriptor, bool) {
	for _, p := range f.Providers {
		for _, m := range p.Models {
			if m.ID == id {
				return m.Descriptor(p.Name), true
			}
		}
	}
	return domain.ModelDescriptor{}, false
}

// --- policies.yaml -----------------------------------------------------------

// PoliciesFile mirrors config/policies.yaml.
type PoliciesFile struct {
	Version       int               `yaml:"version"`
	DefaultPolicy string            `yaml:"default_policy"`
	VirtualModels map[string]string `yaml:"virtual_models"`
	Policies      []PolicySpec      `yaml:"policies"`
	Constraints   ConstraintSpec    `yaml:"constraints"`
}

// PolicySpec is one routing policy.
type PolicySpec struct {
	Name      string     `yaml:"name"`
	Strategy  string     `yaml:"strategy"`
	Params    PolicyArgs `yaml:"params"`
	Fallbacks []string   `yaml:"fallbacks"`
}

// PolicyArgs is the union of every strategy's parameters. Only the fields relevant to a given
// strategy are populated; the strategy constructor validates what it needs.
type PolicyArgs struct {
	QualityFloor  float64          `yaml:"quality_floor"`
	MaxTTFBP99MS  int              `yaml:"max_ttfb_p99_ms"`
	EWMAAlpha     float64          `yaml:"ewma_alpha"`
	SafetyMargin  float64          `yaml:"safety_margin"`
	Buckets       []BucketSpec     `yaml:"buckets"`
	Classifier    ClassifierSpec   `yaml:"classifier"`
	Targets       []WeightedTarget `yaml:"targets"`
	BaselineModel string           `yaml:"baseline_model"`
	CanaryModel   string           `yaml:"canary_model"`
	CanaryPercent int              `yaml:"canary_percent"`
	Sticky        bool             `yaml:"sticky"`
}

// BucketSpec maps a difficulty bucket to a quality floor.
type BucketSpec struct {
	Name         string  `yaml:"name"`
	QualityFloor float64 `yaml:"quality_floor"`
}

// ClassifierSpec configures the heuristic prompt-difficulty classifier.
type ClassifierSpec struct {
	Weights      ClassifierWeights    `yaml:"weights"`
	Thresholds   ClassifierThresholds `yaml:"thresholds"`
	HardKeywords []string             `yaml:"hard_keywords"`
	EasyKeywords []string             `yaml:"easy_keywords"`
}

// ClassifierWeights are the feature weights of the difficulty score.
type ClassifierWeights struct {
	Length    float64 `yaml:"length"`
	Keywords  float64 `yaml:"keywords"`
	Code      float64 `yaml:"code"`
	MultiTurn float64 `yaml:"multi_turn"`
}

// ClassifierThresholds cut the score into buckets.
type ClassifierThresholds struct {
	EasyBelow float64 `yaml:"easy_below"`
	HardAbove float64 `yaml:"hard_above"`
	// ConfidenceBand widens each boundary upwards: a score within this distance below a
	// threshold is promoted to the harder bucket. See Classifier.bucket.
	ConfidenceBand float64 `yaml:"confidence_band"`
}

// WeightedTarget is one arm of a weighted round robin.
type WeightedTarget struct {
	Model  string `yaml:"model"`
	Weight int    `yaml:"weight"`
}

// ConstraintSpec is applied after a strategy has chosen a model.
type ConstraintSpec struct {
	RequireHealthy         bool `yaml:"require_healthy"`
	EnforceTenantAllowlist bool `yaml:"enforce_tenant_allowlist"`
	MaxAttempts            int  `yaml:"max_attempts"`
}

// LoadPolicies parses config/policies.yaml.
func LoadPolicies(path string) (PoliciesFile, error) {
	var f PoliciesFile
	if err := readYAML(path, &f); err != nil {
		return f, err
	}
	if len(f.Policies) == 0 {
		return f, fmt.Errorf("%s: no policies defined", path)
	}
	seen := map[string]bool{}
	for _, p := range f.Policies {
		if p.Name == "" || p.Strategy == "" {
			return f, fmt.Errorf("%s: policy needs both `name` and `strategy`", path)
		}
		if seen[p.Name] {
			return f, fmt.Errorf("%s: duplicate policy %q", path, p.Name)
		}
		seen[p.Name] = true
	}
	for alias, policy := range f.VirtualModels {
		if !seen[policy] {
			return f, fmt.Errorf("%s: virtual model %q maps to unknown policy %q", path, alias, policy)
		}
	}
	if f.Constraints.MaxAttempts <= 0 {
		f.Constraints.MaxAttempts = 3
	}
	return f, nil
}

// ByName looks up a policy.
func (f PoliciesFile) ByName(name string) (PolicySpec, bool) {
	for _, p := range f.Policies {
		if p.Name == name {
			return p, true
		}
	}
	return PolicySpec{}, false
}

// ResolveVirtual maps a virtual model id such as "auto" to a policy name.
func (f PoliciesFile) ResolveVirtual(model string) (string, bool) {
	p, ok := f.VirtualModels[model]
	return p, ok
}

// --- tenants.yaml ------------------------------------------------------------

// TenantsFile mirrors config/tenants.yaml.
type TenantsFile struct {
	Version  int            `yaml:"version"`
	Defaults TenantDefaults `yaml:"defaults"`
	Tenants  []TenantSpec   `yaml:"tenants"`
}

// TenantDefaults fill in unset per-tenant fields.
type TenantDefaults struct {
	DailyTokenBudget int64    `yaml:"daily_token_budget"`
	MonthlyUSDBudget float64  `yaml:"monthly_usd_budget"`
	DefaultPolicy    string   `yaml:"default_policy"`
	AllowedModels    []string `yaml:"allowed_models"`
	WarnThreshold    float64  `yaml:"warn_threshold"`
}

// TenantSpec is one tenant record.
type TenantSpec struct {
	ID               string   `yaml:"id"`
	Name             string   `yaml:"name"`
	APIKey           string   `yaml:"api_key"`
	DefaultPolicy    string   `yaml:"default_policy"`
	DailyTokenBudget int64    `yaml:"daily_token_budget"`
	MonthlyUSDBudget float64  `yaml:"monthly_usd_budget"`
	AllowedModels    []string `yaml:"allowed_models"`
	WarnThreshold    float64  `yaml:"warn_threshold"`

	// SeedProfile is consumed only by seed/generate_traffic.py. The gateway ignores it.
	SeedProfile map[string]any `yaml:"seed_profile"`
}

// LoadTenants parses config/tenants.yaml and applies defaults.
func LoadTenants(path string) (TenantsFile, error) {
	var f TenantsFile
	if err := readYAML(path, &f); err != nil {
		return f, err
	}
	seenID := map[string]bool{}
	seenKey := map[string]bool{}
	for i := range f.Tenants {
		t := &f.Tenants[i]
		if t.ID == "" || t.APIKey == "" {
			return f, fmt.Errorf("%s: tenant %d needs both `id` and `api_key`", path, i)
		}
		if seenID[t.ID] {
			return f, fmt.Errorf("%s: duplicate tenant id %q", path, t.ID)
		}
		if seenKey[t.APIKey] {
			return f, fmt.Errorf("%s: duplicate api_key for tenant %q", path, t.ID)
		}
		seenID[t.ID], seenKey[t.APIKey] = true, true
	}
	f.ApplyDefaults()
	return f, nil
}

// ApplyDefaults fills unset per-tenant fields from the `defaults:` block.
//
// It is exported and idempotent because a TenantsFile can also be constructed in code -- by
// tests, or by a future control-plane API -- and a tenant with a zero budget would otherwise be
// silently rejected by the budget enforcer on its first request.
func (f *TenantsFile) ApplyDefaults() {
	for i := range f.Tenants {
		t := &f.Tenants[i]
		if t.DefaultPolicy == "" {
			t.DefaultPolicy = f.Defaults.DefaultPolicy
		}
		if t.DailyTokenBudget == 0 {
			t.DailyTokenBudget = f.Defaults.DailyTokenBudget
		}
		if t.MonthlyUSDBudget == 0 {
			t.MonthlyUSDBudget = f.Defaults.MonthlyUSDBudget
		}
		if len(t.AllowedModels) == 0 {
			t.AllowedModels = f.Defaults.AllowedModels
		}
		if t.WarnThreshold == 0 {
			t.WarnThreshold = f.Defaults.WarnThreshold
		}
	}
}

// Domain converts a TenantSpec into the domain entity. The raw API key never crosses this
// boundary: only its hash does, so a Tenant can be logged safely.
func (t TenantSpec) Domain() domain.Tenant {
	return domain.Tenant{
		ID:               t.ID,
		Name:             t.Name,
		APIKeyHash:       domain.HashText(t.APIKey),
		DefaultPolicy:    t.DefaultPolicy,
		DailyTokenBudget: t.DailyTokenBudget,
		MonthlyUSDBudget: t.MonthlyUSDBudget,
		AllowedModels:    t.AllowedModels,
		WarnThreshold:    t.WarnThreshold,
	}
}

func readYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.Unmarshal
	if err := dec(b, out); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}
