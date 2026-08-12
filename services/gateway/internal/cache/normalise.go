// Package cache implements the two-tier semantic cache.
//
// A cached answer is admitted for a new prompt in one of two ways:
//
//	exact     the prompts reduce to the same content-token set. Certain, and costs a hash
//	          lookup. This tier carries most of the value.
//	semantic  the prompts embed close enough together. Probabilistic, and the threshold is
//	          calibrated against a labelled pair set rather than guessed.
//
// The split is a measured decision, not a hedge. benchmarks/results/cache_calibration.json
// shows that a bi-encoder scores "increase the timeout" against "decrease the timeout" *higher*
// than it scores many genuine paraphrases, so no similarity threshold makes the semantic tier
// both useful and safe on its own. See docs/adr/0002-cache-threshold-calibration.md.
package cache

import (
	"sort"
	"strings"
	"unicode"
)

// filler is the set of tokens whose presence cannot change what is being asked: closed-class
// function words plus explicit politeness.
//
// It is deliberately short. Every addition raises the hit rate and risks conflating two
// genuinely different requests -- notably, instruction verbs are absent, because "explain X" and
// "write X" ask for different things and folding them together would buy hit rate with
// correctness. This set must match FILLER in benchmarks/common/normalise.py; the parity is
// enforced by TestNormaliseParity and its Python counterpart.
var filler = func() map[string]struct{} {
	words := strings.Fields(`
		a an the of for in on to and or is are am be being been was were with without this
		that these those it its as at by from into over under about than then so if but
		i we you they he she me us them my our your their mine ours yours theirs
		do does did done can could would should will shall may might must
		please thanks thank appreciate hi hello hey ok okay
		quick question thing stuck need help asking colleague sorry just really very
	`)
	set := make(map[string]struct{}, len(words))
	for _, w := range words {
		set[w] = struct{}{}
	}
	return set
}()

// numerals maps digit forms to their word form so "3 metrics" and "three metrics" agree.
var numerals = map[string]string{
	"1": "one", "2": "two", "3": "three", "4": "four", "5": "five",
	"6": "six", "7": "seven", "8": "eight", "9": "nine", "10": "ten",
	"20": "twenty", "30": "thirty", "40": "forty", "50": "fifty", "100": "hundred",
}

// suffixPair is one British/American spelling equivalence.
type suffixPair struct{ american, british string }

// suffixes are applied longest-first so that "izes" is not mangled by the "ize" rule.
var suffixes = []suffixPair{
	{"izing", "ising"},
	{"izes", "ises"},
	{"ized", "ised"},
	{"ize", "ise"},
	{"yzes", "yses"},
	{"yzed", "ysed"},
	{"yze", "yse"},
}

// CanonicalToken reduces one token to its canonical form.
func CanonicalToken(word string) string {
	if n, ok := numerals[word]; ok {
		return n
	}
	for _, s := range suffixes {
		if strings.HasSuffix(word, s.american) {
			return strings.TrimSuffix(word, s.american) + s.british
		}
	}
	return word
}

// tokenize splits text into lowercase tokens of letters, digits, apostrophes and percent signs,
// matching the [a-z0-9'%]+ pattern the Python implementation uses.
func tokenize(text string) []string {
	lower := strings.ToLower(text)
	return strings.FieldsFunc(lower, func(r rune) bool {
		if r == '\'' || r == '%' {
			return false
		}
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// ContentTokens returns the set of meaning-bearing tokens in text.
//
// A set rather than a sequence: "list three metrics for X" and "for X, list three metrics" ask
// the same thing. Losing order does mean two prompts with the same words in a different
// arrangement collide; that is accepted because reordering content words rarely changes an
// English question's meaning, and where it does the semantic tier is the backstop.
func ContentTokens(text string) map[string]struct{} {
	out := make(map[string]struct{}, 16)
	for _, token := range tokenize(text) {
		if _, skip := filler[token]; skip {
			continue
		}
		out[CanonicalToken(token)] = struct{}{}
	}
	return out
}

// ExactKey is a stable key for the exact tier: the sorted content tokens joined by spaces.
//
// Two prompts share an ExactKey exactly when SameQuestion reports true, so the exact tier is a
// map lookup rather than a scan.
func ExactKey(text string) string {
	tokens := ContentTokens(text)
	sorted := make([]string, 0, len(tokens))
	for token := range tokens {
		sorted = append(sorted, token)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, " ")
}

// SameQuestion reports whether the exact tier considers two prompts to be the same question.
func SameQuestion(left, right string) bool {
	return ExactKey(left) == ExactKey(right)
}
