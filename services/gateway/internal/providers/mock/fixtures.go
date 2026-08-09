// Package mock implements a deterministic, offline stand-in for every upstream provider.
//
// It is what makes `make demo` work with no API keys and no network egress. Two properties
// matter and are tested:
//
//   - Determinism. The same prompt and model always yield the same text and token counts, so
//     benchmark numbers reproduce byte-for-byte on a fresh clone.
//   - Realism. Latency is sampled from the per-provider distribution in config/providers.yaml,
//     so end-to-end timings (notably cached vs uncached p50) are measured rather than asserted.
package mock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Tier names used to select a fixture variant. A frontier model answers more completely than an
// efficient one; that difference is what the eval benchmark measures as quality drift.
const (
	TierFrontier  = "frontier"
	TierEfficient = "efficient"
)

// FixtureVariant is one canned answer.
type FixtureVariant struct {
	Text             string `json:"text"`
	CompletionTokens int    `json:"completion_tokens"`
	// Correct records whether this variant actually answers the seeded question. The eval
	// harness reads it to compute quality; the gateway never looks at it.
	Correct bool `json:"correct"`
}

// Fixture is one seeded prompt and its per-tier answers.
type Fixture struct {
	Key          string                    `json:"key"`
	Prompt       string                    `json:"prompt"`
	Difficulty   string                    `json:"difficulty,omitempty"`
	PromptTokens int                       `json:"prompt_tokens"`
	Responses    map[string]FixtureVariant `json:"responses"`
}

// FixtureSet is an in-memory index of seeded responses.
//
// A prompt with no fixture is not an error: Lookup synthesises a deterministic answer from the
// prompt hash. That keeps the demo usable for arbitrary curl commands while keeping the seeded
// benchmark set exact.
type FixtureSet struct {
	byKey map[string]Fixture
}

// LoadFixtures reads a JSONL fixture file. A missing file yields an empty set rather than an
// error: the gateway is still fully functional on synthesised responses, and failing to boot
// because a demo fixture is absent would be the wrong trade.
func LoadFixtures(path string) (*FixtureSet, error) {
	fs := &FixtureSet{byKey: map[string]Fixture{}}
	if path == "" {
		return fs, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fs, nil
		}
		return nil, fmt.Errorf("opening fixtures %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "//") {
			continue
		}
		var fx Fixture
		if err := json.Unmarshal([]byte(raw), &fx); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if fx.Key == "" {
			fx.Key = domain.HashText(domain.NormalizePrompt(fx.Prompt))
		}
		fs.byKey[fx.Key] = fx
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading fixtures %s: %w", path, err)
	}
	return fs, nil
}

// Len is the number of seeded fixtures.
func (s *FixtureSet) Len() int { return len(s.byKey) }

// Lookup returns the answer for a prompt at a given tier, synthesising one when the prompt was
// not seeded. The second return value reports whether the answer came from a real fixture.
func (s *FixtureSet) Lookup(prompt, tier string) (FixtureVariant, Fixture, bool) {
	key := domain.HashText(domain.NormalizePrompt(prompt))
	if fx, ok := s.byKey[key]; ok {
		if v, ok := fx.Responses[tier]; ok {
			return v, fx, true
		}
		// A fixture without this tier falls back to the frontier answer, which is always present
		// in generated fixtures.
		if v, ok := fx.Responses[TierFrontier]; ok {
			return v, fx, true
		}
	}
	return synthesise(prompt, tier), Fixture{Key: key, Prompt: prompt}, false
}

// synthesise builds a deterministic, plausible-looking answer for an unseeded prompt.
//
// The text is derived from an FNV hash of (prompt, tier) so it is stable across runs and across
// machines, and the length differs by tier so that cost and latency still vary the way they
// would in production.
func synthesise(prompt, tier string) FixtureVariant {
	h := fnv.New64a()
	_, _ = h.Write([]byte(domain.NormalizePrompt(prompt)))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(tier))
	seed := h.Sum64()

	sentences := []string{
		"Here is a concise answer to your question.",
		"The key consideration is the trade-off between cost and latency.",
		"In practice you would measure this before committing to a design.",
		"A worked example follows, with the assumptions stated up front.",
		"The short version: it depends on the shape of your traffic.",
		"Three factors dominate the outcome, in decreasing order of impact.",
		"This is the behaviour the specification requires, not an implementation detail.",
		"The remaining edge cases are handled by the same mechanism.",
	}

	// Frontier answers are longer and draw on more of the sentence pool.
	n := 3
	if tier == TierFrontier {
		n = 6
	}
	var b strings.Builder
	b.WriteString("[mock:")
	b.WriteString(tier)
	b.WriteString("] ")
	for i := 0; i < n; i++ {
		idx := int((seed >> (uint(i) * 7)) % uint64(len(sentences)))
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(sentences[idx])
	}
	text := b.String()
	return FixtureVariant{
		Text:             text,
		CompletionTokens: EstimateTokens(text),
		Correct:          tier == TierFrontier,
	}
}

// EstimateTokens approximates a token count from text.
//
// Roughly four characters per token, which is close enough for cost attribution in a demo and,
// crucially, is the same estimator the seed generator and the Python benchmarks use, so the
// numbers agree across languages. Real deployments would use the provider's reported usage.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := (len(s) + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}

// TierFor maps a model tier from the catalogue onto a fixture tier.
func TierFor(modelTier string) string {
	if modelTier == TierFrontier {
		return TierFrontier
	}
	return TierEfficient
}
