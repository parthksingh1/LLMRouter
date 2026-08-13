// Package router implements the policy engine: it turns a request plus a policy into a concrete
// provider and model, and produces the ordered fallbacks to try when that choice fails.
//
// The engine is deliberately data-driven. Strategies are registered by name and configured
// entirely from config/policies.yaml, so adding an A/B test or shifting a canary is a config
// change and a restart, not a deploy.
package router

import (
	"strings"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Classifier buckets a prompt into easy, medium or hard.
//
// It is a transparent linear model over four features rather than a learned classifier, for
// three reasons: it runs in microseconds on the hot path, its decisions can be explained in a
// log line, and it has no model artefact to ship or drift. A production deployment would replace
// it with a small trained model behind the same interface -- see docs/adr/0002.
//
// The important property is not accuracy in the abstract but *asymmetry*: mistaking a hard
// prompt for an easy one costs quality, while the reverse only costs money. The quality_tiered
// strategy therefore adds a safety margin on top of whatever this returns.
type Classifier struct {
	weights    config.ClassifierWeights
	thresholds config.ClassifierThresholds
	hard       []string
	easy       []string
}

// referenceChars is the prompt length that saturates the length feature. Prompts longer than
// this are all treated as "long"; the signal stops being informative well before the context
// window is reached.
const referenceChars = 1200.0

// NewClassifier builds a classifier from policy configuration, applying defaults so that a
// policy which omits the classifier block still behaves sensibly.
func NewClassifier(spec config.ClassifierSpec) *Classifier {
	c := &Classifier{
		weights:    spec.Weights,
		thresholds: spec.Thresholds,
		hard:       lowerAll(spec.HardKeywords),
		easy:       lowerAll(spec.EasyKeywords),
	}
	if c.weights.Length == 0 && c.weights.Keywords == 0 && c.weights.Code == 0 && c.weights.MultiTurn == 0 {
		c.weights = config.ClassifierWeights{Length: 0.30, Keywords: 0.42, Code: 0.16, MultiTurn: 0.12}
	}
	if c.thresholds.EasyBelow == 0 && c.thresholds.HardAbove == 0 {
		c.thresholds = config.ClassifierThresholds{EasyBelow: 0.34, HardAbove: 0.62}
	}
	return c
}

// Features are the inputs to the difficulty score, exposed so the decision can be logged and
// so the offline policy simulator can reproduce it exactly.
type Features struct {
	Length    float64 // 0..1, prompt length relative to referenceChars
	Keywords  float64 // -1..1, negative for easy markers, positive for hard ones
	Code      float64 // 0 or 1
	MultiTurn float64 // 0..1, saturating at six turns
	Score     float64 // -1..1, the weighted combination; the sign separates easy from medium
}

// Classify returns the difficulty bucket and the features behind it.
func (c *Classifier) Classify(req domain.ChatRequest) (domain.Difficulty, Features) {
	prompt := req.UserPrompt()
	lower := strings.ToLower(prompt)

	f := Features{
		Length:    clamp01(float64(len(prompt)) / referenceChars),
		Keywords:  c.keywordSignal(lower),
		Code:      boolFeature(looksLikeCode(prompt)),
		MultiTurn: clamp01(float64(req.TurnCount()-1) / 5.0),
	}

	// The score is clamped to -1..1, NOT to 0..1.
	//
	// That sign matters and was a real bug: with a 0..1 clamp, every prompt carrying an easy
	// marker and every prompt carrying none collapsed to exactly 0, so "easy" and "medium"
	// became indistinguishable and 100% of medium traffic was routed as easy. Letting the score
	// go negative turns "more easy markers than hard ones" into the easy signal in its own
	// right, which separates the three buckets robustly instead of relying on a knife-edge
	// length threshold.
	f.Score = clampSigned(
		c.weights.Length*f.Length +
			c.weights.Keywords*f.Keywords +
			c.weights.Code*f.Code +
			c.weights.MultiTurn*f.MultiTurn,
	)

	return c.bucket(f.Score), f
}

// bucket maps a score onto a difficulty, applying the confidence band.
//
// The band is the asymmetry made concrete. Mistaking a hard prompt for an easy one costs a bad
// answer; mistaking an easy prompt for a hard one costs a fraction of a cent. So a score that
// sits close to a bucket boundary is resolved *upwards*: the router only routes down when the
// classifier is clearly on the cheap side of the line.
//
// It also gives the cost/quality trade-off a continuous dial. Quality floors alone move in
// steps -- one notch of safety margin jumps a whole bucket to the next model tier -- whereas
// widening the band shifts a controllable slice of the borderline traffic upwards. The
// committed value is the operating point measured by benchmarks/eval/run_eval.py.
func (c *Classifier) bucket(score float64) domain.Difficulty {
	band := c.thresholds.ConfidenceBand

	if score >= c.thresholds.HardAbove {
		return domain.DifficultyHard
	}
	if score < c.thresholds.EasyBelow {
		// Close to the easy/medium line: promote rather than risk under-serving.
		if c.thresholds.EasyBelow-score < band {
			return domain.DifficultyMedium
		}
		return domain.DifficultyEasy
	}
	// Medium. Close to the medium/hard line: promote.
	if c.thresholds.HardAbove-score < band {
		return domain.DifficultyHard
	}
	return domain.DifficultyMedium
}

// keywordSignal returns a value in -1..1. Hard markers push up, easy markers pull down, and the
// count is saturated so that a prompt repeating one word does not dominate the score.
func (c *Classifier) keywordSignal(lowerPrompt string) float64 {
	hard := countMatches(lowerPrompt, c.hard)
	easy := countMatches(lowerPrompt, c.easy)

	const saturateAt = 3.0
	h := clamp01(float64(hard) / saturateAt)
	e := clamp01(float64(easy) / saturateAt)
	return h - e
}

func countMatches(haystack string, needles []string) int {
	n := 0
	for _, kw := range needles {
		if strings.Contains(haystack, kw) {
			n++
		}
	}
	return n
}

// looksLikeCode detects the markers that reliably indicate a programming question: fenced
// blocks, an obvious indent block, or a dense run of punctuation characteristic of source text.
func looksLikeCode(s string) bool {
	if strings.Contains(s, "```") {
		return true
	}
	for _, marker := range []string{"def ", "func ", "class ", "SELECT ", "import ", "#include", "=> ", "};"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	// Four-space or tab indentation on any line after the first.
	for _, line := range strings.Split(s, "\n")[1:] {
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			return true
		}
	}
	return false
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(s))
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clampSigned bounds a value to -1..1, preserving the sign that separates easy from medium.
func clampSigned(v float64) float64 {
	if v < -1 {
		return -1
	}
	if v > 1 {
		return 1
	}
	return v
}

func boolFeature(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
