package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/router"
)

// availability is a controllable stand-in for the provider registry.
type availability struct {
	down map[string]bool
	ttfb map[string]float64
}

func newAvailability() *availability {
	return &availability{down: map[string]bool{}, ttfb: map[string]float64{}}
}

func (a *availability) Available(name string) bool { return !a.down[name] }

func (a *availability) ObservedTTFBMillis(name string) float64 { return a.ttfb[name] }

// catalogue mirrors the shape of the real config: five providers, a frontier and an efficient
// model each, with prices and quality scores spread the way real ones are.
func catalogue() config.ProvidersFile {
	mk := func(name string, ttfb int, models ...config.ModelSpec) config.ProviderSpec {
		return config.ProviderSpec{
			Name:               name,
			PrefixContinuation: config.PrefixPrefill,
			Latency:            config.LatencySpec{TTFBP50MS: ttfb, TTFBP95MS: ttfb * 2, TTFBP99MS: ttfb * 4, TokensPerSec: 60},
			Models:             models,
		}
	}
	return config.ProvidersFile{
		Defaults: config.ProviderDefaults{HealthCheckIntervalMS: 1000},
		Providers: []config.ProviderSpec{
			mk("openai", 420,
				config.ModelSpec{ID: "gpt-4o", Family: "gpt-4", Tier: "frontier", Quality: 0.951, PriceInPerM: 2.50, PriceOutPerM: 10.00},
				config.ModelSpec{ID: "gpt-4o-mini", Family: "gpt-4", Tier: "efficient", Quality: 0.863, PriceInPerM: 0.15, PriceOutPerM: 0.60},
			),
			mk("anthropic", 380,
				config.ModelSpec{ID: "claude-3-5-sonnet", Family: "claude-3", Tier: "frontier", Quality: 0.948, PriceInPerM: 3.00, PriceOutPerM: 15.00},
				config.ModelSpec{ID: "claude-3-5-haiku", Family: "claude-3", Tier: "efficient", Quality: 0.871, PriceInPerM: 0.80, PriceOutPerM: 4.00},
			),
			mk("google", 350,
				config.ModelSpec{ID: "gemini-1.5-pro", Family: "gemini", Tier: "frontier", Quality: 0.932, PriceInPerM: 1.25, PriceOutPerM: 5.00},
				config.ModelSpec{ID: "gemini-1.5-flash", Family: "gemini", Tier: "efficient", Quality: 0.842, PriceInPerM: 0.075, PriceOutPerM: 0.30},
			),
			mk("mistral", 300,
				config.ModelSpec{ID: "mistral-large-latest", Family: "mistral", Tier: "frontier", Quality: 0.903, PriceInPerM: 2.00, PriceOutPerM: 6.00},
			),
			mk("together", 260,
				config.ModelSpec{ID: "llama-3.1-70b-instruct", Family: "llama-3", Tier: "frontier", Quality: 0.884, PriceInPerM: 0.88, PriceOutPerM: 0.88},
				config.ModelSpec{ID: "llama-3.1-8b-instruct", Family: "llama-3", Tier: "efficient", Quality: 0.762, PriceInPerM: 0.18, PriceOutPerM: 0.18},
			),
		},
	}
}

func policies() config.PoliciesFile {
	return config.PoliciesFile{
		DefaultPolicy: "quality_tiered",
		VirtualModels: map[string]string{
			"auto":       "quality_tiered",
			"auto:cheap": "cost_optimized",
			"auto:fast":  "latency_optimized",
		},
		Constraints: config.ConstraintSpec{
			RequireHealthy: true, EnforceTenantAllowlist: true, MaxAttempts: 3,
		},
		Policies: []config.PolicySpec{
			{
				Name: "cost_optimized", Strategy: "cost_optimized",
				Params:    config.PolicyArgs{QualityFloor: 0.84, MaxTTFBP99MS: 2500},
				Fallbacks: []string{"anthropic", "openai"},
			},
			{
				Name: "latency_optimized", Strategy: "latency_optimized",
				Params:    config.PolicyArgs{QualityFloor: 0.80},
				Fallbacks: []string{"together", "mistral"},
			},
			{
				Name: "quality_tiered", Strategy: "quality_tiered",
				Params: config.PolicyArgs{
					SafetyMargin: 0.02,
					Buckets: []config.BucketSpec{
						{Name: "easy", QualityFloor: 0.84},
						{Name: "medium", QualityFloor: 0.86},
						{Name: "hard", QualityFloor: 0.93},
					},
					Classifier: config.ClassifierSpec{
						Weights:      config.ClassifierWeights{Length: 0.28, Keywords: 0.46, Code: 0.14, MultiTurn: 0.12},
						Thresholds:   config.ClassifierThresholds{EasyBelow: 0.0, HardAbove: 0.30},
						HardKeywords: []string{"prove", "derive", "complexity", "trade-off", "race condition", "distributed"},
						EasyKeywords: []string{"what is", "capital of", "summar", "translate"},
					},
				},
				Fallbacks: []string{"anthropic", "google", "openai"},
			},
			{
				Name: "weighted_round_robin", Strategy: "weighted_round_robin",
				Params: config.PolicyArgs{Targets: []config.WeightedTarget{
					{Model: "gpt-4o-mini", Weight: 40},
					{Model: "claude-3-5-haiku", Weight: 35},
					{Model: "llama-3.1-70b-instruct", Weight: 25},
				}},
			},
			{
				Name: "canary", Strategy: "canary",
				Params: config.PolicyArgs{
					BaselineModel: "gpt-4o-mini", CanaryModel: "gemini-1.5-flash",
					CanaryPercent: 10, Sticky: true,
				},
			},
		},
	}
}

func tenant() domain.Tenant {
	return domain.Tenant{ID: "tenant-a", DefaultPolicy: "quality_tiered", AllowedModels: []string{"*"}}
}

func newEngine(t *testing.T, avail router.Availability) *router.Engine {
	t.Helper()
	e, err := router.New(catalogue(), policies(), avail)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return e
}

func request(model, policy, prompt string) domain.ChatRequest {
	return domain.ChatRequest{
		RequestID:      "req-1",
		TenantID:       "tenant-a",
		RequestedModel: model,
		Policy:         policy,
		Messages:       []domain.Message{{Role: domain.RoleUser, Content: prompt}},
	}
}

func TestRouteSelectsByPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		model      string
		policy     string
		prompt     string
		wantModel  string
		wantPolicy string
	}{
		{
			name:  "a pinned model bypasses routing entirely",
			model: "claude-3-5-sonnet", prompt: "anything",
			wantModel: "claude-3-5-sonnet", wantPolicy: "pinned",
		},
		{
			name:  "cost_optimized takes the cheapest model above its floor",
			model: "auto:cheap", prompt: "hello",
			// gemini-1.5-flash at 0.842 clears the 0.84 floor and is the cheapest overall.
			wantModel: "gemini-1.5-flash", wantPolicy: "cost_optimized",
		},
		{
			name:  "latency_optimized takes the fastest provider above its floor",
			model: "auto:fast", prompt: "hello",
			// together has the lowest configured p50, and llama-70b clears the 0.80 floor.
			wantModel: "llama-3.1-70b-instruct", wantPolicy: "latency_optimized",
		},
		{
			name:  "quality_tiered sends an easy prompt to a cheap model",
			model: "auto", prompt: "what is the capital of France",
			// The easy floor is 0.84, but the 0.02 safety margin lifts the effective floor to
			// 0.86, which excludes gemini-1.5-flash (0.842) and llama-3.1-8b (0.762). The
			// margin is the dial that buys quality with money; this is it doing its job.
			wantModel: "gpt-4o-mini", wantPolicy: "quality_tiered",
		},
		{
			name:  "quality_tiered keeps a hard prompt on a frontier model",
			model: "auto", prompt: "derive the complexity and analyse the trade-off in a distributed system",
			// hard floor 0.93 + margin 0.02 = 0.95, which only gpt-4o clears.
			wantModel: "gpt-4o", wantPolicy: "quality_tiered",
		},
		{
			name: "an explicit policy header overrides the virtual model",
			// cost_optimized has no safety margin, so gemini-1.5-flash (0.842) clears its
			// 0.84 floor -- and the hard prompt is routed cheaply anyway, which is exactly what
			// the caller asked for by overriding the policy.
			model: "auto", policy: "cost_optimized", prompt: "derive the complexity of this",
			wantModel: "gemini-1.5-flash", wantPolicy: "cost_optimized",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEngine(t, newAvailability())

			d, err := e.Route(context.Background(), request(tc.model, tc.policy, tc.prompt), tenant())
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if d.Model.ID != tc.wantModel {
				t.Errorf("model = %q, want %q (reason: %s)", d.Model.ID, tc.wantModel, d.Reason)
			}
			if d.Policy != tc.wantPolicy {
				t.Errorf("policy = %q, want %q", d.Policy, tc.wantPolicy)
			}
			if d.Reason == "" {
				t.Error("every decision must carry a human-readable reason")
			}
		})
	}
}

func TestRouteIsDeterministic(t *testing.T) {
	t.Parallel()

	e := newEngine(t, newAvailability())
	req := request("auto:cheap", "", "hello there")

	first, err := e.Route(context.Background(), req, tenant())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	// Identical inputs must give identical decisions, or two replicas would route the same
	// traffic differently and the eval benchmark would not reproduce.
	for i := 0; i < 25; i++ {
		d, err := e.Route(context.Background(), req, tenant())
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if d.Model.ID != first.Model.ID {
			t.Fatalf("routing is non-deterministic: got %q then %q", first.Model.ID, d.Model.ID)
		}
	}
}

func TestRouteRespectsHealth(t *testing.T) {
	t.Parallel()

	avail := newAvailability()
	// Knock out the two cheapest providers; cost_optimized must move up the price ladder.
	avail.down["google"] = true
	avail.down["together"] = true

	e := newEngine(t, avail)
	d, err := e.Route(context.Background(), request("auto:cheap", "", "hello"), tenant())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Provider == "google" || d.Provider == "together" {
		t.Errorf("routed to the unhealthy provider %q", d.Provider)
	}
	if d.Model.ID != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini as the cheapest healthy option", d.Model.ID)
	}
}

func TestRouteRespectsTenantAllowlist(t *testing.T) {
	t.Parallel()

	e := newEngine(t, newAvailability())
	restricted := domain.Tenant{
		ID: "tenant-b", DefaultPolicy: "quality_tiered",
		AllowedModels: []string{"claude-3-5-haiku", "gpt-4o-mini"},
	}

	t.Run("routed requests stay inside the allowlist", func(t *testing.T) {
		d, err := e.Route(context.Background(), request("auto:cheap", "", "hello"), restricted)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if !restricted.ModelAllowed(d.Model.ID) {
			t.Errorf("routed to %q, which is outside the tenant allowlist", d.Model.ID)
		}
	})

	t.Run("a pinned model outside the allowlist is refused", func(t *testing.T) {
		_, err := e.Route(context.Background(), request("gpt-4o", "", "hello"), restricted)
		if err == nil {
			t.Fatal("expected the pinned model to be refused")
		}
		if !strings.Contains(err.Error(), "may not use") {
			t.Errorf("error = %v, want a permission message", err)
		}
	})
}

func TestRouteRejectsUnknownModelsAndPolicies(t *testing.T) {
	t.Parallel()

	e := newEngine(t, newAvailability())

	t.Run("unknown model", func(t *testing.T) {
		_, err := e.Route(context.Background(), request("gpt-9", "", "hi"), tenant())
		if err == nil {
			t.Fatal("expected an error for an unknown model")
		}
		// The message must list the virtual models, because the usual cause is a caller
		// guessing at the alias.
		if !strings.Contains(err.Error(), "auto") {
			t.Errorf("error %v should name the available virtual models", err)
		}
	})

	t.Run("unknown policy is refused rather than silently ignored", func(t *testing.T) {
		_, err := e.Route(context.Background(), request("auto", "made-up-policy", "hi"), tenant())
		if err == nil {
			t.Fatal("expected an error for an unknown policy")
		}
	})
}

func TestFallbacksNeverRepeatTheFailedProvider(t *testing.T) {
	t.Parallel()

	e := newEngine(t, newAvailability())
	req := request("auto", "", "derive the complexity of this distributed algorithm")

	primary, err := e.Route(context.Background(), req, tenant())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	fallbacks := e.Fallbacks(context.Background(), req, tenant(), primary)
	if len(fallbacks) == 0 {
		t.Fatal("expected at least one fallback")
	}
	// max_attempts is 3, so the primary plus two fallbacks.
	if len(fallbacks) > 2 {
		t.Errorf("got %d fallbacks, want at most 2 for max_attempts=3", len(fallbacks))
	}

	seen := map[string]bool{primary.Provider: true}
	for _, f := range fallbacks {
		if seen[f.Provider] {
			t.Errorf("provider %q appears twice in the failover chain", f.Provider)
		}
		seen[f.Provider] = true
		if f.Attempt == 0 {
			t.Error("fallback decisions must carry their attempt number")
		}
	}

	// The first fallback should be the policy's preferred provider, which for quality_tiered
	// is anthropic.
	if fallbacks[0].Provider != "anthropic" {
		t.Errorf("first fallback provider = %q, want anthropic from the policy preference list",
			fallbacks[0].Provider)
	}
}

func TestFallbacksPreferSimilarQuality(t *testing.T) {
	t.Parallel()

	avail := newAvailability()
	// Remove the preferred fallback providers so ranking falls through to the quality rule.
	avail.down["anthropic"] = true
	avail.down["google"] = true

	e := newEngine(t, avail)
	req := request("gpt-4o", "", "anything")

	primary, err := e.Route(context.Background(), req, tenant())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	fallbacks := e.Fallbacks(context.Background(), req, tenant(), primary)
	if len(fallbacks) == 0 {
		t.Fatal("expected a fallback")
	}
	// gpt-4o is 0.951. Of what is left (mistral 0.903, together 0.884/0.762), mistral-large is
	// closest, so a failover must not silently drop to a much weaker model.
	if fallbacks[0].Model.ID != "mistral-large-latest" {
		t.Errorf("first fallback = %q, want mistral-large-latest as the closest in quality",
			fallbacks[0].Model.ID)
	}
}

func TestFallbacksEmptyWhenNothingElseIsUp(t *testing.T) {
	t.Parallel()

	avail := newAvailability()
	for _, p := range []string{"anthropic", "google", "mistral", "together"} {
		avail.down[p] = true
	}

	e := newEngine(t, avail)
	req := request("gpt-4o", "", "anything")
	primary, err := e.Route(context.Background(), req, tenant())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got := e.Fallbacks(context.Background(), req, tenant(), primary); len(got) != 0 {
		t.Errorf("got %d fallbacks, want none when every other provider is down", len(got))
	}
}

func TestWeightedRoundRobinHitsItsRatios(t *testing.T) {
	t.Parallel()

	e := newEngine(t, newAvailability())
	req := request("auto", "weighted_round_robin", "hello")

	counts := map[string]int{}
	const n = 1000
	for i := 0; i < n; i++ {
		d, err := e.Route(context.Background(), req, tenant())
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		counts[d.Model.ID]++
	}

	want := map[string]float64{
		"gpt-4o-mini":            0.40,
		"claude-3-5-haiku":       0.35,
		"llama-3.1-70b-instruct": 0.25,
	}
	for model, share := range want {
		got := float64(counts[model]) / n
		// Deterministic interleaving should be exact to well within a percent.
		if got < share-0.01 || got > share+0.01 {
			t.Errorf("%s got %.3f of traffic, want %.2f (counts: %v)", model, got, share, counts)
		}
	}
}

func TestCanaryIsStickyPerPrompt(t *testing.T) {
	t.Parallel()

	e := newEngine(t, newAvailability())

	t.Run("the same prompt always lands on the same arm", func(t *testing.T) {
		req := request("auto", "canary", "how do I reset my password")
		first, err := e.Route(context.Background(), req, tenant())
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		for i := 0; i < 20; i++ {
			d, _ := e.Route(context.Background(), req, tenant())
			if d.Model.ID != first.Model.ID {
				t.Fatalf("sticky canary flipped arms: %q then %q", first.Model.ID, d.Model.ID)
			}
		}
	})

	t.Run("roughly the configured share reaches the canary", func(t *testing.T) {
		canary := 0
		const n = 600
		for i := 0; i < n; i++ {
			req := request("auto", "canary", "distinct prompt number "+strings.Repeat("x", i%50)+string(rune('a'+i%26)))
			d, err := e.Route(context.Background(), req, tenant())
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if d.Model.ID == "gemini-1.5-flash" {
				canary++
			}
		}
		share := float64(canary) / n
		// Hashing gives an approximate split, so the tolerance is wide; the point is that it is
		// neither zero nor everything.
		if share < 0.03 || share > 0.20 {
			t.Errorf("canary share = %.3f, want roughly 0.10", share)
		}
	})
}

func TestNewRejectsInvalidPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  config.PolicySpec
		wantErr string
	}{
		{
			name:    "unknown strategy",
			policy:  config.PolicySpec{Name: "p", Strategy: "vibes"},
			wantErr: "unknown routing strategy",
		},
		{
			name:    "quality_tiered without buckets",
			policy:  config.PolicySpec{Name: "p", Strategy: "quality_tiered"},
			wantErr: "at least one difficulty bucket",
		},
		{
			name: "quality_tiered with an unknown bucket name",
			policy: config.PolicySpec{Name: "p", Strategy: "quality_tiered", Params: config.PolicyArgs{
				Buckets: []config.BucketSpec{{Name: "impossible", QualityFloor: 0.9}},
			}},
			wantErr: "unknown difficulty bucket",
		},
		{
			name:    "weighted_round_robin without targets",
			policy:  config.PolicySpec{Name: "p", Strategy: "weighted_round_robin"},
			wantErr: "at least one target",
		},
		{
			name: "weighted_round_robin with a zero weight",
			policy: config.PolicySpec{Name: "p", Strategy: "weighted_round_robin", Params: config.PolicyArgs{
				Targets: []config.WeightedTarget{{Model: "a", Weight: 0}},
			}},
			wantErr: "non-positive weight",
		},
		{
			name:    "canary without arms",
			policy:  config.PolicySpec{Name: "p", Strategy: "canary"},
			wantErr: "baseline_model and canary_model",
		},
		{
			name: "canary with an out-of-range percentage",
			policy: config.PolicySpec{Name: "p", Strategy: "canary", Params: config.PolicyArgs{
				BaselineModel: "a", CanaryModel: "b", CanaryPercent: 150,
			}},
			wantErr: "between 0 and 100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A misconfigured policy must fail at construction, not on the first request that
			// happens to use it.
			pf := config.PoliciesFile{DefaultPolicy: "p", Policies: []config.PolicySpec{tc.policy}}
			_, err := router.New(catalogue(), pf, newAvailability())
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestEngineSatisfiesTheRouterPort(t *testing.T) {
	t.Parallel()
	var _ app.Router = newEngine(t, newAvailability())
}

func TestClassifier(t *testing.T) {
	t.Parallel()

	spec := config.ClassifierSpec{
		Weights:      config.ClassifierWeights{Length: 0.28, Keywords: 0.46, Code: 0.14, MultiTurn: 0.12},
		Thresholds:   config.ClassifierThresholds{EasyBelow: 0.0, HardAbove: 0.30},
		HardKeywords: []string{"prove", "derive", "complexity", "trade-off", "race condition", "distributed"},
		EasyKeywords: []string{"what is", "capital of", "summar", "translate"},
	}
	c := router.NewClassifier(spec)

	tests := []struct {
		name     string
		messages []domain.Message
		want     domain.Difficulty
	}{
		{
			name:     "a short factual question is easy",
			messages: []domain.Message{{Role: domain.RoleUser, Content: "what is the capital of Peru"}},
			want:     domain.DifficultyEasy,
		},
		{
			name:     "a translation request is easy",
			messages: []domain.Message{{Role: domain.RoleUser, Content: "translate this sentence to German"}},
			want:     domain.DifficultyEasy,
		},
		{
			name: "three hard markers reach the hard bucket on their own",
			messages: []domain.Message{{
				Role:    domain.RoleUser,
				Content: "derive the complexity and the trade-off here",
			}},
			want: domain.DifficultyHard,
		},
		{
			// With no markers at all the score is a small positive number, which is the medium
			// bucket by construction: "medium" is the absence of evidence either way.
			name: "a prompt with no markers is medium",
			messages: []domain.Message{{
				Role:    domain.RoleUser,
				Content: strings.Repeat("some ordinary prose about a topic. ", 20),
			}},
			want: domain.DifficultyMedium,
		},
		{
			// A single easy marker contributes -0.153 to the score, which a 900-character
			// prompt outweighs. That is the intended ordering: length is weak evidence, but it
			// is not nothing, and one keyword should not override a wall of text.
			name: "one easy marker does not outweigh a very long prompt",
			messages: []domain.Message{{
				Role:    domain.RoleUser,
				Content: "summar" + strings.Repeat("ise this text please. ", 40),
			}},
			want: domain.DifficultyMedium,
		},
		{
			name: "several easy markers win regardless of length",
			messages: []domain.Message{{
				Role:    domain.RoleUser,
				Content: "translate and summarise: what is " + strings.Repeat("this text about a topic. ", 20),
			}},
			want: domain.DifficultyEasy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, features := c.Classify(domain.ChatRequest{Messages: tc.messages})
			if got != tc.want {
				t.Errorf("Classify() = %s, want %s (score %.3f, features %+v)",
					got, tc.want, features.Score, features)
			}
			// The score is signed: negative means the easy markers outweighed the hard ones,
			// which is what separates easy from medium.
			if features.Score < -1 || features.Score > 1 {
				t.Errorf("score %.3f is outside -1..1", features.Score)
			}
		})
	}
}

func TestClassifierDetectsCode(t *testing.T) {
	t.Parallel()

	c := router.NewClassifier(config.ClassifierSpec{})

	withCode := domain.ChatRequest{Messages: []domain.Message{{
		Role: domain.RoleUser, Content: "fix this:\n```go\nfunc main() {}\n```",
	}}}
	withoutCode := domain.ChatRequest{Messages: []domain.Message{{
		Role: domain.RoleUser, Content: "fix this thing for me",
	}}}

	_, codeFeatures := c.Classify(withCode)
	_, plainFeatures := c.Classify(withoutCode)

	if codeFeatures.Code != 1 {
		t.Error("a fenced code block should set the code feature")
	}
	if plainFeatures.Code != 0 {
		t.Error("prose should not set the code feature")
	}
	if codeFeatures.Score <= plainFeatures.Score {
		t.Error("code should raise the difficulty score")
	}
}

func TestClassifierMultiTurn(t *testing.T) {
	t.Parallel()

	c := router.NewClassifier(config.ClassifierSpec{})

	single := domain.ChatRequest{Messages: []domain.Message{
		{Role: domain.RoleUser, Content: "carry on"},
	}}
	long := domain.ChatRequest{Messages: []domain.Message{
		{Role: domain.RoleUser, Content: "one"},
		{Role: domain.RoleAssistant, Content: "two"},
		{Role: domain.RoleUser, Content: "three"},
		{Role: domain.RoleAssistant, Content: "four"},
		{Role: domain.RoleUser, Content: "carry on"},
	}}

	_, singleF := c.Classify(single)
	_, longF := c.Classify(long)

	if singleF.MultiTurn != 0 {
		t.Errorf("a single turn should score 0 on the multi-turn feature, got %v", singleF.MultiTurn)
	}
	if longF.MultiTurn <= singleF.MultiTurn {
		t.Error("a longer conversation should raise the multi-turn feature")
	}
}
