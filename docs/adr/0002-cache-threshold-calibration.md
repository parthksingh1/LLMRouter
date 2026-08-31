# 2. Two-tier cache admission, with the threshold calibrated rather than chosen

**Status:** Accepted

## Context

A semantic cache serves a stored answer when a new prompt is "close enough" to an old one. It
has exactly one interesting knob — how close is close enough — and the two ways of getting it
wrong are not symmetric:

- **Too strict:** the cache never hits. It costs money and latency, and nobody notices.
- **Too loose:** the cache confidently answers a question that was not asked. Nobody notices
  that either, until someone does, and then the cache gets switched off permanently.

The obvious design is: embed the prompt, find the nearest neighbour, serve it if the cosine
similarity clears a threshold. We built that, then measured it.

## What the measurement showed

`benchmarks/cache/calibrate.py` sweeps the threshold against 500 paraphrase pairs that should
hit and 500 hard negatives that must not. Per-kind results with `BAAI/bge-small-en-v1.5`:

| Negative kind | n | mean similarity | max |
|---|---|---|---|
| `different_question` (other question, same system) | 175 | 0.552 | 0.827 |
| `subject_swap` (same question, other system) | 125 | 0.828 | 0.934 |
| **`minimal_pair`** (one word changed, meaning inverted) | 200 | **0.934** | **0.998** |
| `paraphrase` (should hit) | 500 | 0.958 | 0.997 |

The first two separate cleanly. The third does not, and it is the one that matters.

**A bi-encoder scores "should I increase the timeout" against "should I decrease the timeout"
higher than it scores many genuine paraphrases.** That is not a defect in this particular model;
it is what sentence embeddings do. They encode topic and structure, and a single antonym barely
moves the vector.

The consequence is arithmetic: at a 0.5% false-hit budget, similarity alone admitted **0.2%** of
paraphrases. A cache that hits one time in five hundred is not a cache.

## Decision

Admit in two tiers.

**Tier 1 — exact.** Both prompts reduce to the same content-token set after conservative
normalisation (`benchmarks/common/normalise.py`, mirrored in `internal/cache/normalise.go`).
Only closed-class function words and politeness are discarded, plus `-ise`/`-ize` spellings and
small numerals. Certain, costs one hash lookup, and carries most of the value: **39.6%** of
paraphrases at **0.00%** false hits.

Instruction verbs are deliberately *not* filler. "Explain X" and "write X" ask for different
things, and folding them together would buy hit rate with correctness. A test asserts that
nobody adds them later.

**Tier 2 — semantic.** Cosine similarity above the calibrated threshold, *and* the exact-tier
rule as a guard. The guard is what makes it safe: a minimal pair can score 0.998 and still be
refused, because its content tokens differ.

The threshold is chosen by maximising hit rate subject to a false-hit budget, not by maximising
F1 or Youden's J. A symmetric criterion assumes the two error types cost the same. They do not.

## Consequences

The committed calibration reports the semantic tier as **not worthwhile on this pair set**:
`+0.2%` hits for `+0.4%` false hits. The tool says so in `semantic_tier_verdict` rather than
quietly recommending a threshold that loses money.

That verdict is against a deliberately adversarial set — 40% minimal pairs, far more than real
near-miss traffic contains. On the seeded workload the cache reaches **39.6%** with **zero**
false hits. Both numbers are published, because quoting only the friendly one would be the
whole problem this ADR exists to avoid.

The exact tier does most of the work, which makes "semantic cache" a slightly grand name for
what earns its keep. That is the honest finding.

## Alternatives considered

**Lower the threshold and accept more false hits.** Rejected: a wrong answer delivered with
confidence is the failure that destroys trust in the whole feature.

**A cross-encoder re-rank over the top-k candidates.** This is the right answer and it is what a
production system should do. A cross-encoder sees both prompts at once and separates minimal
pairs easily. It costs a model, a GPU budget, and 20–50 ms on the cache path — which would make
`make demo` depend on all three. Named in the results file as the recommended upgrade.

**Fine-tune an embedding model on the negatives.** Better still, and needs training data,
training infrastructure and a retraining story. Out of scope for a gateway.

## Revisit when

A cross-encoder is available, or the false-hit budget changes. Re-run
`make bench-cache`; the threshold in `.env.example` is read from the result, not written by
hand.
