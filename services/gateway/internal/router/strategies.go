package router

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync/atomic"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Candidate is one model the engine may choose, together with everything a strategy needs to
// rank it. Assembling this once per request keeps strategies free of lookups.
type Candidate struct {
	Model domain.ModelDescriptor
	// TTFBMillis is the observed moving average where available, otherwise the configured p50.
	TTFBMillis float64
	// TTFBP99Millis is the configured tail latency, used as a hard ceiling.
	TTFBP99Millis float64
	// EstCostUSD is the modelled cost of this call, from the request's token shape.
	EstCostUSD float64
}

// Strategy picks one candidate from the eligible set.
//
// Candidates are pre-filtered by the engine: unhealthy providers, breaker-open providers and
// models the tenant may not use are already gone. A strategy that finds an empty slice returns
// an error rather than a zero value, so a misconfiguration surfaces immediately.
type Strategy interface {
	Name() string
	Pick(req domain.ChatRequest, difficulty domain.Difficulty, candidates []Candidate) (Candidate, string, error)
}

// errNoCandidate is returned when a strategy has nothing eligible to choose from. The engine
// translates it into app.ErrNoProviderAvailable.
var errNoCandidate = fmt.Errorf("no eligible candidate")

// NewStrategy builds the strategy named by a policy.
func NewStrategy(p config.PolicySpec) (Strategy, error) {
	switch p.Strategy {
	case "cost_optimized":
		return newCostOptimized(p.Params), nil
	case "latency_optimized":
		return newLatencyOptimized(p.Params), nil
	case "quality_tiered":
		return newQualityTiered(p.Params)
	case "weighted_round_robin":
		return newWeightedRoundRobin(p.Params)
	case "canary":
		return newCanary(p.Params)
	default:
		return nil, fmt.Errorf("unknown routing strategy %q in policy %q", p.Strategy, p.Name)
	}
}

// --- cost_optimized ----------------------------------------------------------

type costOptimized struct {
	qualityFloor float64
	maxTTFBP99   float64
}

func newCostOptimized(p config.PolicyArgs) *costOptimized {
	return &costOptimized{
		qualityFloor: p.QualityFloor,
		maxTTFBP99:   float64(p.MaxTTFBP99MS),
	}
}

func (s *costOptimized) Name() string { return "cost_optimized" }

// Pick returns the cheapest model clearing the quality floor and the latency ceiling.
//
// Ties are broken by quality, then by model id. Deterministic tie-breaking matters more than it
// looks: without it, two gateway replicas given identical config would route identically-priced
// models differently, and the eval benchmark would not reproduce.
func (s *costOptimized) Pick(_ domain.ChatRequest, _ domain.Difficulty, candidates []Candidate) (Candidate, string, error) {
	eligible := filter(candidates, func(c Candidate) bool {
		if c.Model.Quality < s.qualityFloor {
			return false
		}
		if s.maxTTFBP99 > 0 && c.TTFBP99Millis > s.maxTTFBP99 {
			return false
		}
		return true
	})
	if len(eligible) == 0 {
		// Rather than fail the request outright, fall back to the highest-quality candidate.
		// Refusing to answer because nothing was cheap enough is the wrong trade.
		if len(candidates) == 0 {
			return Candidate{}, "", errNoCandidate
		}
		best := maxBy(candidates, func(c Candidate) float64 { return c.Model.Quality })
		return best, fmt.Sprintf("no model cleared the quality floor %.2f; fell back to the best available (%s)",
			s.qualityFloor, best.Model.ID), nil
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].EstCostUSD != eligible[j].EstCostUSD {
			return eligible[i].EstCostUSD < eligible[j].EstCostUSD
		}
		if eligible[i].Model.Quality != eligible[j].Model.Quality {
			return eligible[i].Model.Quality > eligible[j].Model.Quality
		}
		return eligible[i].Model.ID < eligible[j].Model.ID
	})

	best := eligible[0]
	return best, fmt.Sprintf("cheapest of %d models above quality %.2f (est $%.6f)",
		len(eligible), s.qualityFloor, best.EstCostUSD), nil
}

// --- latency_optimized -------------------------------------------------------

type latencyOptimized struct {
	qualityFloor float64
}

func newLatencyOptimized(p config.PolicyArgs) *latencyOptimized {
	return &latencyOptimized{qualityFloor: p.QualityFloor}
}

func (s *latencyOptimized) Name() string { return "latency_optimized" }

// Pick returns the fastest candidate by observed TTFB, subject to the quality floor.
//
// It ranks on *observed* latency rather than configured latency, so a provider having a bad
// afternoon is routed around automatically well before its circuit breaker trips.
func (s *latencyOptimized) Pick(_ domain.ChatRequest, _ domain.Difficulty, candidates []Candidate) (Candidate, string, error) {
	eligible := filter(candidates, func(c Candidate) bool { return c.Model.Quality >= s.qualityFloor })
	if len(eligible) == 0 {
		if len(candidates) == 0 {
			return Candidate{}, "", errNoCandidate
		}
		eligible = candidates
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].TTFBMillis != eligible[j].TTFBMillis {
			return eligible[i].TTFBMillis < eligible[j].TTFBMillis
		}
		if eligible[i].EstCostUSD != eligible[j].EstCostUSD {
			return eligible[i].EstCostUSD < eligible[j].EstCostUSD
		}
		return eligible[i].Model.ID < eligible[j].Model.ID
	})

	best := eligible[0]
	return best, fmt.Sprintf("lowest observed TTFB (%.0f ms) above quality %.2f",
		best.TTFBMillis, s.qualityFloor), nil
}

// --- quality_tiered ----------------------------------------------------------

type qualityTiered struct {
	classifier   *Classifier
	safetyMargin float64
	floors       map[domain.Difficulty]float64
}

func newQualityTiered(p config.PolicyArgs) (*qualityTiered, error) {
	if len(p.Buckets) == 0 {
		return nil, fmt.Errorf("quality_tiered requires at least one difficulty bucket")
	}
	floors := make(map[domain.Difficulty]float64, len(p.Buckets))
	for _, b := range p.Buckets {
		switch domain.Difficulty(b.Name) {
		case domain.DifficultyEasy, domain.DifficultyMedium, domain.DifficultyHard:
			floors[domain.Difficulty(b.Name)] = b.QualityFloor
		default:
			return nil, fmt.Errorf("quality_tiered: unknown difficulty bucket %q", b.Name)
		}
	}
	return &qualityTiered{
		classifier:   NewClassifier(p.Classifier),
		safetyMargin: p.SafetyMargin,
		floors:       floors,
	}, nil
}

func (s *qualityTiered) Name() string { return "quality_tiered" }

// Classifier exposes the difficulty classifier so the engine can label the decision without
// running it twice.
func (s *qualityTiered) Classifier() *Classifier { return s.classifier }

// Pick returns the cheapest model clearing the floor for this prompt's difficulty bucket.
//
// The safety margin is the whole cost/quality dial. It is added to the bucket floor, so raising
// it keeps more traffic on frontier models: it buys quality with money. The committed value is
// the operating point measured by benchmarks/eval/run_eval.py.
func (s *qualityTiered) Pick(_ domain.ChatRequest, difficulty domain.Difficulty, candidates []Candidate) (Candidate, string, error) {
	if len(candidates) == 0 {
		return Candidate{}, "", errNoCandidate
	}

	floor, ok := s.floors[difficulty]
	if !ok {
		// An unclassified prompt is treated as hard. Failing safe here is the asymmetry the
		// classifier comment describes: over-spending is recoverable, a bad answer is not.
		floor = s.floors[domain.DifficultyHard]
	}
	effective := floor + s.safetyMargin

	eligible := filter(candidates, func(c Candidate) bool { return c.Model.Quality >= effective })
	if len(eligible) == 0 {
		best := maxBy(candidates, func(c Candidate) float64 { return c.Model.Quality })
		return best, fmt.Sprintf("%s prompt: nothing cleared quality %.3f, using the best available %s (%.3f)",
			difficulty, effective, best.Model.ID, best.Model.Quality), nil
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].EstCostUSD != eligible[j].EstCostUSD {
			return eligible[i].EstCostUSD < eligible[j].EstCostUSD
		}
		if eligible[i].Model.Quality != eligible[j].Model.Quality {
			return eligible[i].Model.Quality > eligible[j].Model.Quality
		}
		return eligible[i].Model.ID < eligible[j].Model.ID
	})

	best := eligible[0]
	return best, fmt.Sprintf("%s prompt: cheapest model above quality %.3f (floor %.2f + margin %.2f), est $%.6f",
		difficulty, effective, floor, s.safetyMargin, best.EstCostUSD), nil
}

// --- weighted_round_robin ----------------------------------------------------

type weightedRoundRobin struct {
	targets []config.WeightedTarget
	total   int
	counter atomic.Uint64
}

func newWeightedRoundRobin(p config.PolicyArgs) (*weightedRoundRobin, error) {
	if len(p.Targets) == 0 {
		return nil, fmt.Errorf("weighted_round_robin requires at least one target")
	}
	total := 0
	for _, t := range p.Targets {
		if t.Weight <= 0 {
			return nil, fmt.Errorf("weighted_round_robin: target %q has a non-positive weight", t.Model)
		}
		total += t.Weight
	}
	return &weightedRoundRobin{targets: p.Targets, total: total}, nil
}

func (s *weightedRoundRobin) Name() string { return "weighted_round_robin" }

// Pick walks a deterministic weighted sequence rather than sampling randomly.
//
// Deterministic interleaving gives exact ratios over any window, which is what an A/B test
// needs; random sampling only converges eventually and makes short experiments noisy.
func (s *weightedRoundRobin) Pick(_ domain.ChatRequest, _ domain.Difficulty, candidates []Candidate) (Candidate, string, error) {
	if len(candidates) == 0 {
		return Candidate{}, "", errNoCandidate
	}

	// Try each arm in weighted order until one is actually available. An arm whose provider is
	// down must not silently absorb its share of the experiment.
	n := s.counter.Add(1) - 1
	slot := int(n % uint64(s.total))

	ordered := make([]config.WeightedTarget, 0, len(s.targets))
	acc := 0
	startIdx := 0
	for i, t := range s.targets {
		acc += t.Weight
		if slot < acc {
			startIdx = i
			break
		}
	}
	for i := 0; i < len(s.targets); i++ {
		ordered = append(ordered, s.targets[(startIdx+i)%len(s.targets)])
	}

	for _, t := range ordered {
		if c, ok := findModel(candidates, t.Model); ok {
			pct := float64(t.Weight) / float64(s.total) * 100
			return c, fmt.Sprintf("A/B arm %s (%.0f%% of traffic)", t.Model, pct), nil
		}
	}
	return candidates[0], "every A/B arm was unavailable; fell back to the first candidate", nil
}

// --- canary ------------------------------------------------------------------

type canary struct {
	baseline string
	canary   string
	percent  int
	sticky   bool
	counter  atomic.Uint64
}

func newCanary(p config.PolicyArgs) (*canary, error) {
	if p.BaselineModel == "" || p.CanaryModel == "" {
		return nil, fmt.Errorf("canary requires both baseline_model and canary_model")
	}
	if p.CanaryPercent < 0 || p.CanaryPercent > 100 {
		return nil, fmt.Errorf("canary_percent must be between 0 and 100, got %d", p.CanaryPercent)
	}
	return &canary{
		baseline: p.BaselineModel,
		canary:   p.CanaryModel,
		percent:  p.CanaryPercent,
		sticky:   p.Sticky,
	}, nil
}

func (s *canary) Name() string { return "canary" }

// Pick sends a percentage of traffic to the canary model.
//
// With sticky enabled, the bucket is derived from (tenant, prompt) rather than a counter, so the
// same caller asking the same question always lands on the same arm. That is what makes a canary
// comparison meaningful: differences come from the model, not from which arm a retry happened to
// hit.
func (s *canary) Pick(req domain.ChatRequest, _ domain.Difficulty, candidates []Candidate) (Candidate, string, error) {
	if len(candidates) == 0 {
		return Candidate{}, "", errNoCandidate
	}

	var bucket int
	if s.sticky {
		h := fnv.New32a()
		_, _ = h.Write([]byte(req.TenantID))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(domain.NormalizePrompt(req.UserPrompt())))
		bucket = int(h.Sum32() % 100)
	} else {
		bucket = int((s.counter.Add(1) - 1) % 100)
	}

	want, arm := s.baseline, "baseline"
	if bucket < s.percent {
		want, arm = s.canary, "canary"
	}

	if c, ok := findModel(candidates, want); ok {
		return c, fmt.Sprintf("%s arm %s (%d%% canary, sticky=%v)", arm, want, s.percent, s.sticky), nil
	}
	// The chosen arm is unavailable. Prefer the other arm over an arbitrary model, so a canary
	// outage degrades into the baseline rather than into something untested.
	other := s.baseline
	if want == s.baseline {
		other = s.canary
	}
	if c, ok := findModel(candidates, other); ok {
		return c, fmt.Sprintf("%s arm %s is unavailable; served the other arm %s", arm, want, other), nil
	}
	return candidates[0], fmt.Sprintf("neither canary arm is available; fell back to %s", candidates[0].Model.ID), nil
}

// --- helpers -----------------------------------------------------------------

func filter(in []Candidate, keep func(Candidate) bool) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func findModel(in []Candidate, id string) (Candidate, bool) {
	for _, c := range in {
		if c.Model.ID == id {
			return c, true
		}
	}
	return Candidate{}, false
}

func maxBy(in []Candidate, score func(Candidate) float64) Candidate {
	best := in[0]
	for _, c := range in[1:] {
		if score(c) > score(best) || (score(c) == score(best) && c.Model.ID < best.Model.ID) {
			best = c
		}
	}
	return best
}
