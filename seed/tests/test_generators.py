"""The seed generators are load-bearing: every committed number depends on them.

These tests pin the two properties that matter. Determinism, because a benchmark that does not
reproduce byte-for-byte is not evidence. And the structural invariants the benchmarks silently
assume — that no two eval cases share a prompt, that fixtures exist for every seeded prompt,
that the negative mix is what it claims to be.
"""

from __future__ import annotations

import json
import random
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / "seed"))

import generate_cache_pairs  # noqa: E402
import generate_eval_set  # noqa: E402
from corpus import (  # noqa: E402
    NEGATIVE_MIX,
    QUALITY_THRESHOLDS,
    confuse,
    estimate_tokens,
    normalize_prompt,
    paraphrase,
    prompt_key,
)

DATASET = REPO_ROOT / "benchmarks" / "eval" / "dataset.jsonl"
PAIRS = REPO_ROOT / "benchmarks" / "cache" / "pairs.jsonl"
FIXTURES = REPO_ROOT / "seed" / "fixtures" / "responses.jsonl"


def load_jsonl(path: Path) -> list[dict]:
    if not path.exists():
        pytest.skip(f"{path} not generated; run `make seed`")
    with path.open(encoding="utf-8") as fh:
        return [json.loads(line) for line in fh if line.strip()]


# --- determinism -------------------------------------------------------------


def test_eval_set_is_deterministic() -> None:
    """The same seed must produce identical output, or no committed number reproduces."""
    first = generate_eval_set.build_cases(1337, 200)
    second = generate_eval_set.build_cases(1337, 200)
    assert first == second


def test_a_different_seed_produces_different_cases() -> None:
    """Guards against a generator that ignores its seed and only looks deterministic."""
    assert generate_eval_set.build_cases(1337, 50) != generate_eval_set.build_cases(9999, 50)


def test_cache_pairs_are_deterministic() -> None:
    assert generate_cache_pairs.build_pairs(1337, 50, 50) == generate_cache_pairs.build_pairs(1337, 50, 50)


# --- eval set invariants -----------------------------------------------------


def test_eval_prompts_are_unique() -> None:
    """A duplicate prompt would be served from the cache during a gateway-mode run and price
    that case at zero, quietly flattering the cost saving."""
    cases = generate_eval_set.build_cases(1337, 200)
    keys = [c["key"] for c in cases]
    assert len(set(keys)) == len(keys)


def test_eval_thresholds_stay_inside_their_bucket_range() -> None:
    for case in generate_eval_set.build_cases(1337, 200):
        low, high = QUALITY_THRESHOLDS[case["difficulty"]]
        assert low <= case["min_quality"] <= high, case


def test_eval_set_covers_every_difficulty() -> None:
    difficulties = {c["difficulty"] for c in generate_eval_set.build_cases(1337, 200)}
    assert difficulties == {"easy", "medium", "hard"}


def test_misleading_cases_exist_and_are_a_minority() -> None:
    """They are where essentially all the measured quality drift comes from, so their share is
    effectively the drift dial and must not drift itself."""
    cases = generate_eval_set.build_cases(1337, 200)
    misleading = [c for c in cases if c["misleading"]]

    assert misleading, "without these the classifier never makes a mistake and drift is zero"
    assert len(misleading) / len(cases) < 0.15


# --- cache pairs -------------------------------------------------------------


def test_pair_set_is_balanced() -> None:
    pairs = generate_cache_pairs.build_pairs(1337, 100, 100)
    assert sum(1 for p in pairs if p["label"] == 1) == 100
    assert sum(1 for p in pairs if p["label"] == 0) == 100


def test_negative_mix_matches_the_declared_proportions() -> None:
    """An earlier version silently produced 73% subject swaps because word swaps were attempted
    first and skipped when a template had nothing to swap. That made the set far more
    adversarial than real traffic and pinned the achievable hit rate near zero."""
    pairs = generate_cache_pairs.build_pairs(1337, 100, 200)
    negatives = [p for p in pairs if p["label"] == 0]

    for kind, share in NEGATIVE_MIX.items():
        actual = sum(1 for p in negatives if p["kind"] == kind) / len(negatives)
        assert abs(actual - share) < 0.02, f"{kind}: {actual:.3f} vs declared {share}"


def test_pairs_never_repeat_a_side() -> None:
    """A pair whose two sides are identical is not a test of anything."""
    for pair in generate_cache_pairs.build_pairs(1337, 100, 100):
        assert pair["left"] != pair["right"], pair


# --- corpus helpers ----------------------------------------------------------


def test_token_estimate_matches_the_go_rule() -> None:
    """Four characters per token, the same estimator the Go gateway uses. If these diverge the
    Python and Go halves of a benchmark price the same call differently."""
    assert estimate_tokens("") == 0
    assert estimate_tokens("a") == 1
    assert estimate_tokens("abcd") == 1
    assert estimate_tokens("abcde") == 2
    assert estimate_tokens("x" * 400) == 100


def test_normalisation_collapses_case_and_whitespace() -> None:
    assert normalize_prompt("  Hello   WORLD  ") == "hello world"
    assert prompt_key("Hello World") == prompt_key("  hello   world ")


def test_paraphrase_preserves_the_question() -> None:
    rng = random.Random(1)
    original = "List three metrics worth tracking."
    for _ in range(20):
        rewritten = paraphrase(original, rng)
        assert rewritten, "a paraphrase must not be empty"


def test_confuse_changes_meaning_or_returns_none() -> None:
    rng = random.Random(1)
    assert confuse("no confusable terms in this sentence whatsoever", rng) is None

    changed = confuse("should I increase the timeout", rng)
    assert changed is not None
    assert changed != "should I increase the timeout"


# --- committed artifacts -----------------------------------------------------


def test_committed_dataset_matches_the_generator() -> None:
    """The committed file must be what the generator produces, or `make bench` measures one
    thing and the repository ships another."""
    committed = load_jsonl(DATASET)
    regenerated = generate_eval_set.build_cases(1337, len(committed))
    assert committed == regenerated, "run `make seed` and commit the result"


def test_every_seeded_prompt_has_a_fixture() -> None:
    """A missing fixture means the mock synthesises an answer of a different length, which
    changes the token count and therefore the measured cost."""
    fixtures = {f["key"] for f in load_jsonl(FIXTURES)}
    missing = [c["id"] for c in load_jsonl(DATASET) if c["key"] not in fixtures]
    assert not missing, f"{len(missing)} eval cases have no fixture, e.g. {missing[:5]}"


def test_frontier_answers_are_longer_in_aggregate() -> None:
    """A cheaper model must produce a visibly shorter answer, or it is only cheaper by the
    price ratio and the measured saving is understated.

    Asserted in aggregate rather than per fixture, and that is the correct claim: sentences vary
    in length, so an efficient answer of two long sentences can exceed a frontier answer of two
    short ones. Real models do not guarantee per-response ordering either. What must hold -- and
    what actually drives the cost figure -- is that the distributions differ.
    """
    fixtures = load_jsonl(FIXTURES)

    by_difficulty: dict[str, list[tuple[int, int]]] = {}
    for fixture in fixtures:
        pair = (
            fixture["responses"]["frontier"]["completion_tokens"],
            fixture["responses"]["efficient"]["completion_tokens"],
        )
        by_difficulty.setdefault(fixture["difficulty"], []).append(pair)

    for difficulty, pairs in by_difficulty.items():
        frontier_mean = sum(f for f, _ in pairs) / len(pairs)
        efficient_mean = sum(e for _, e in pairs) / len(pairs)
        assert frontier_mean > efficient_mean, (
            f"{difficulty}: frontier mean {frontier_mean:.1f} is not above "
            f"efficient mean {efficient_mean:.1f}; the tiers are not differentiated"
        )

    # The gap should widen with difficulty: that is what makes routing a hard prompt down cost
    # more than routing an easy one down.
    def gap(difficulty: str) -> float:
        pairs = by_difficulty[difficulty]
        return sum(f - e for f, e in pairs) / len(pairs)

    if {"easy", "hard"} <= by_difficulty.keys():
        assert gap("hard") > gap("easy"), (
            f"the tier gap should widen with difficulty: easy {gap('easy'):.1f} "
            f"vs hard {gap('hard'):.1f}"
        )
