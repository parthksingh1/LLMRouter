package router

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Availability is the slice of the provider registry the engine needs.
//
// Declared here rather than imported so the engine can be tested without constructing a registry
// and its background health-check goroutines.
type Availability interface {
	Available(name string) bool
	ObservedTTFBMillis(name string) float64
}

// estimateShape is the token shape used to compare model prices before a request has run.
//
// Prices are split between input and output, so comparing models needs an assumed ratio. These
// values are the medians of the seeded traffic set; the comparison is a ranking, so what matters
// is that the ratio is representative, not that the absolute figure is right.
const (
	estimatePromptTokens     = 350
	estimateCompletionTokens = 280
)

// Engine is the policy engine. It satisfies app.Router.
type Engine struct {
	catalogue    config.ProvidersFile
	policies     config.PoliciesFile
	constraints  config.ConstraintSpec
	availability Availability

	strategies map[string]Strategy
	// fallbacks maps a policy name to its configured provider preference order.
	fallbacks map[string][]string
	// classifiers holds the classifier of any quality_tiered policy, so the engine can label a
	// decision with the difficulty that produced it.
	classifiers map[string]*Classifier
}

var _ app.Router = (*Engine)(nil)

// New builds the engine from configuration, constructing every strategy up front so that a
// misconfigured policy fails at start-up rather than on the first request that uses it.
func New(catalogue config.ProvidersFile, policies config.PoliciesFile, availability Availability) (*Engine, error) {
	e := &Engine{
		catalogue:    catalogue,
		policies:     policies,
		constraints:  policies.Constraints,
		availability: availability,
		strategies:   make(map[string]Strategy, len(policies.Policies)),
		fallbacks:    make(map[string][]string, len(policies.Policies)),
		classifiers:  map[string]*Classifier{},
	}

	for _, p := range policies.Policies {
		s, err := NewStrategy(p)
		if err != nil {
			return nil, err
		}
		e.strategies[p.Name] = s
		e.fallbacks[p.Name] = p.Fallbacks

		if qt, ok := s.(*qualityTiered); ok {
			e.classifiers[p.Name] = qt.Classifier()
		}
	}

	if _, ok := e.strategies[policies.DefaultPolicy]; !ok {
		return nil, fmt.Errorf("default policy %q has no strategy", policies.DefaultPolicy)
	}
	return e, nil
}

// Route chooses a provider and model for a request.
//
// Resolution order for the model field:
//  1. a concrete model id pins the request, bypassing routing entirely;
//  2. a virtual model ("auto", "auto:cheap") selects a policy;
//  3. anything else is an error, because silently routing an unknown model would hide typos.
func (e *Engine) Route(_ context.Context, req domain.ChatRequest, t domain.Tenant) (domain.RouteDecision, error) {
	// 1. Pinned model.
	if model, ok := e.catalogue.FindModel(req.RequestedModel); ok {
		if e.constraints.EnforceTenantAllowlist && !t.ModelAllowed(model.ID) {
			return domain.RouteDecision{}, fmt.Errorf("%w: tenant %s may not use %s",
				app.ErrModelNotAllowed, t.ID, model.ID)
		}
		if e.constraints.RequireHealthy && !e.availability.Available(model.Provider) {
			// A pinned model whose provider is down is not fatal: the fallback chain will
			// carry it. Report the pin as the decision and let the dispatcher fail over.
			return domain.RouteDecision{
				Provider: model.Provider,
				Model:    model,
				Policy:   "pinned",
				Reason:   fmt.Sprintf("caller pinned %s; its provider is currently unavailable", model.ID),
			}, nil
		}
		return domain.RouteDecision{
			Provider: model.Provider,
			Model:    model,
			Policy:   "pinned",
			Reason:   fmt.Sprintf("caller pinned %s", model.ID),
		}, nil
	}

	// 2. Virtual model or explicit policy header.
	policyName, err := e.resolvePolicy(req, t)
	if err != nil {
		return domain.RouteDecision{}, err
	}
	strategy := e.strategies[policyName]

	difficulty := domain.DifficultyMedium
	if c, ok := e.classifiers[policyName]; ok {
		difficulty, _ = c.Classify(req)
	}

	candidates := e.candidates(t, true)
	if len(candidates) == 0 {
		// Nothing healthy. Retry ignoring health so the caller gets a real upstream error from
		// an attempt rather than an immediate 503 that hides which provider is broken.
		candidates = e.candidates(t, false)
		if len(candidates) == 0 {
			return domain.RouteDecision{}, fmt.Errorf("%w: no model in the catalogue is permitted for tenant %s",
				app.ErrNoProviderAvailable, t.ID)
		}
	}

	pick, reason, err := strategy.Pick(req, difficulty, candidates)
	if err != nil {
		return domain.RouteDecision{}, fmt.Errorf("%w: %s", app.ErrNoProviderAvailable, err)
	}

	return domain.RouteDecision{
		Provider:   pick.Model.Provider,
		Model:      pick.Model,
		Policy:     policyName,
		Difficulty: difficulty,
		Reason:     reason,
		Score:      pick.EstCostUSD,
	}, nil
}

// Fallbacks returns the ordered alternatives to try after primary has failed.
//
// Ordering follows three rules, in priority order:
//
//  1. never the provider that just failed;
//  2. providers named in the policy's `fallbacks:` list first, in that order;
//  3. within a provider, the model closest in quality to the one that failed, so a failover does
//     not quietly downgrade the answer.
//
// The list is capped by constraints.max_attempts so a bad afternoon cannot turn one client
// request into a stampede across every vendor.
func (e *Engine) Fallbacks(
	_ context.Context,
	req domain.ChatRequest,
	t domain.Tenant,
	primary domain.RouteDecision,
) []domain.RouteDecision {
	maxAttempts := e.constraints.MaxAttempts
	if maxAttempts <= 1 {
		return nil
	}

	preference := e.fallbacks[primary.Policy]
	rank := make(map[string]int, len(preference))
	for i, name := range preference {
		rank[name] = i
	}

	candidates := filter(e.candidates(t, true), func(c Candidate) bool {
		return c.Model.Provider != primary.Provider
	})
	if len(candidates) == 0 {
		return nil
	}

	targetQuality := primary.Model.Quality
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, iok := rank[candidates[i].Model.Provider]
		rj, jok := rank[candidates[j].Model.Provider]
		if iok != jok {
			return iok // a listed provider outranks an unlisted one
		}
		if iok && jok && ri != rj {
			return ri < rj
		}
		// Closest quality to what we were going to serve.
		di := abs(candidates[i].Model.Quality - targetQuality)
		dj := abs(candidates[j].Model.Quality - targetQuality)
		if di != dj {
			return di < dj
		}
		if candidates[i].EstCostUSD != candidates[j].EstCostUSD {
			return candidates[i].EstCostUSD < candidates[j].EstCostUSD
		}
		return candidates[i].Model.ID < candidates[j].Model.ID
	})

	// At most one model per provider: retrying a second model on a provider that just failed
	// wastes an attempt, because the failure is usually the provider, not the model.
	seen := map[string]bool{}
	out := make([]domain.RouteDecision, 0, maxAttempts-1)
	for _, c := range candidates {
		if seen[c.Model.Provider] {
			continue
		}
		seen[c.Model.Provider] = true

		out = append(out, domain.RouteDecision{
			Provider:   c.Model.Provider,
			Model:      c.Model,
			Policy:     primary.Policy,
			Difficulty: primary.Difficulty,
			Attempt:    len(out) + 1,
			Reason: fmt.Sprintf("failover %d: %s (quality %.3f vs %.3f on the failed model)",
				len(out)+1, c.Model.ID, c.Model.Quality, targetQuality),
			Score: c.EstCostUSD,
		})
		if len(out) >= maxAttempts-1 {
			break
		}
	}
	_ = req // reserved: a future strategy may re-classify for the fallback
	return out
}

// resolvePolicy determines which policy governs a request.
func (e *Engine) resolvePolicy(req domain.ChatRequest, t domain.Tenant) (string, error) {
	// An explicit header wins, and an unknown one is an error rather than a silent default:
	// silently ignoring a routing instruction is worse than refusing it.
	if req.Policy != "" {
		if _, ok := e.strategies[req.Policy]; !ok {
			return "", fmt.Errorf("%w: %q", app.ErrPolicyNotFound, req.Policy)
		}
		return req.Policy, nil
	}

	if policy, ok := e.policies.ResolveVirtual(req.RequestedModel); ok {
		return policy, nil
	}

	// A model that is neither concrete nor virtual is a typo.
	if req.RequestedModel != "" {
		return "", fmt.Errorf("%w: %q is neither a model in the catalogue nor a virtual model (%s)",
			app.ErrModelNotFound, req.RequestedModel, strings.Join(e.virtualNames(), ", "))
	}

	if t.DefaultPolicy != "" {
		if _, ok := e.strategies[t.DefaultPolicy]; ok {
			return t.DefaultPolicy, nil
		}
	}
	return e.policies.DefaultPolicy, nil
}

// candidates assembles the eligible set for a tenant.
func (e *Engine) candidates(t domain.Tenant, requireHealthy bool) []Candidate {
	descriptors := e.catalogue.Descriptors()
	out := make([]Candidate, 0, len(descriptors))

	for _, m := range descriptors {
		if e.constraints.EnforceTenantAllowlist && !t.ModelAllowed(m.ID) {
			continue
		}
		if requireHealthy && e.constraints.RequireHealthy && !e.availability.Available(m.Provider) {
			continue
		}

		spec, _ := e.catalogue.Provider(m.Provider)
		ttfb := e.availability.ObservedTTFBMillis(m.Provider)
		if ttfb <= 0 {
			ttfb = float64(spec.Latency.TTFBP50MS)
		}

		out = append(out, Candidate{
			Model:         m,
			TTFBMillis:    ttfb,
			TTFBP99Millis: float64(spec.Latency.TTFBP99MS),
			EstCostUSD:    m.BlendedCostPerCall(estimatePromptTokens, estimateCompletionTokens),
		})
	}

	// Catalogue order is map-free and therefore already deterministic, but sorting by id makes
	// the guarantee explicit and independent of YAML ordering.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Model.ID < out[j].Model.ID })
	return out
}

// PolicyNames lists the configured policies, for diagnostics.
func (e *Engine) PolicyNames() []string {
	out := make([]string, 0, len(e.strategies))
	for name := range e.strategies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) virtualNames() []string {
	out := make([]string, 0, len(e.policies.VirtualModels))
	for name := range e.policies.VirtualModels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
