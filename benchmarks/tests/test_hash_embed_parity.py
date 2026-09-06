"""Cross-language parity gate for the hash embedder.

The semantic cache threshold is calibrated in Python and enforced in Go. If the two hash
embedding implementations ever diverge, the calibrated number silently stops describing the
running cache: the hit rate falls, nothing errors, and the cause is a long way from the symptom.

The golden vectors are produced by the Go implementation:

    cd services/gateway && go run ./cmd/hashembed \\
        < ../../benchmarks/tests/testdata/hash_embed_inputs.txt \\
        > ../../benchmarks/tests/testdata/hash_embed_golden.json

Regenerate them only when the construction is changed on purpose -- and re-run the cache
calibration in the same commit, because the similarity distribution will have moved.
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / "services" / "embedder"))

from app.main import cosine, hash_embed  # noqa: E402

GOLDEN = Path(__file__).parent / "testdata" / "hash_embed_golden.json"

#: Go serialises float32; Python computes in float64. Agreement to within a float32 epsilon is
#: the most that can be asked, and is far tighter than anything that would affect a threshold
#: quoted to four decimal places.
TOLERANCE = 1e-6


def load_golden() -> list[dict[str, object]]:
    if not GOLDEN.exists():  # pragma: no cover - only in a broken checkout
        pytest.skip(f"golden file missing: {GOLDEN}")
    return json.loads(GOLDEN.read_text(encoding="utf-8"))


@pytest.mark.parametrize("record", load_golden(), ids=lambda r: r["text"][:40])
def test_python_matches_go(record: dict[str, object]) -> None:
    """Every component of every golden vector must match the Python implementation."""
    text = record["text"]
    expected = record["vector"]
    assert isinstance(text, str) and isinstance(expected, list)

    actual = hash_embed(text, len(expected))

    assert len(actual) == len(expected), "dimension mismatch between Go and Python"
    for i, (got, want) in enumerate(zip(actual, expected, strict=True)):
        assert (
            abs(got - want) < TOLERANCE
        ), f"component {i} differs for {text!r}: python={got!r} go={want!r}"


def test_vectors_are_unit_norm() -> None:
    """The cache compares with cosine similarity, which assumes unit-norm vectors."""
    for record in load_golden():
        vec = hash_embed(str(record["text"]))
        magnitude = math.sqrt(sum(x * x for x in vec))
        assert magnitude == pytest.approx(1.0, abs=1e-9), record["text"]


def test_normalisation_makes_case_and_whitespace_irrelevant() -> None:
    """Two spellings of the same question must produce the identical vector.

    This is the exact-match half of the cache: if normalisation did not collapse these, the
    cache would rely on the similarity threshold to catch a case it can answer with certainty.
    """
    a = hash_embed("what is the capital of Peru")
    b = hash_embed("  What   Is The  CAPITAL of   PERU  ")
    assert cosine(a, b) == pytest.approx(1.0, abs=1e-12)


def test_paraphrases_score_higher_than_unrelated_text() -> None:
    """The whole premise of the cache: meaning-preserving rewrites land close together."""
    base = hash_embed("how do I reset my password")
    paraphrase = hash_embed("how can I reset my password")
    unrelated = hash_embed("what is the capital of Peru")

    assert cosine(base, paraphrase) > cosine(base, unrelated)


def test_a_one_word_change_is_measurably_different() -> None:
    """The hard-negative case the calibration exists to price.

    "password" and "router" differ by one word, so the vectors stay close -- closer than to an
    unrelated sentence. Serving one answer for the other would be a false hit, which is why the
    threshold is calibrated rather than guessed.
    """
    base = hash_embed("how do I reset my password")
    confusable = hash_embed("how do I reset my router")

    similarity = cosine(base, confusable)
    assert similarity < 1.0, "a different question must not be identical"
    assert (
        similarity > 0.3
    ), "a near-miss should still be lexically close, or the test is wrong"


def test_short_inputs_do_not_crash() -> None:
    """Inputs shorter than one trigram are padded rather than producing a zero vector."""
    for text in ("", "a", "ab", "abc"):
        vec = hash_embed(text)
        assert len(vec) == 384
        assert math.sqrt(sum(x * x for x in vec)) == pytest.approx(1.0, abs=1e-9)
