"""Cross-language parity gate for cache prompt normalisation.

The exact tier of the cache decides what counts as "the same question". Go enforces it at
request time; Python applies it when calibrating the threshold. If the two drift apart, the
committed hit rate stops describing the running cache -- and, worse, a prompt pair the
calibration counted as safe could become a false hit in production.

Regenerate the golden file when the rule changes on purpose:

    cd services/gateway && go run ./cmd/normalise \\
        < ../../benchmarks/tests/testdata/normalise_inputs.txt \\
        > ../../benchmarks/tests/testdata/normalise_golden.json

and re-run `make bench-cache` in the same commit, because the hit rate will have moved.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))

from benchmarks.common.normalise import exact_key, same_question  # noqa: E402

GOLDEN = Path(__file__).parent / "testdata" / "normalise_golden.json"


def load_golden() -> list[dict[str, str]]:
    if not GOLDEN.exists():  # pragma: no cover
        pytest.skip(f"golden file missing: {GOLDEN}")
    return json.loads(GOLDEN.read_text(encoding="utf-8"))


@pytest.mark.parametrize("record", load_golden(), ids=lambda r: r["text"][:44])
def test_python_matches_go(record: dict[str, str]) -> None:
    """The Python exact key must equal the Go one, token for token."""
    assert exact_key(record["text"]) == record["key"], (
        f"normalisation diverged for {record['text']!r}\n"
        f"  python: {exact_key(record['text'])!r}\n"
        f"  go:     {record['key']!r}"
    )


@pytest.mark.parametrize(
    ("left", "right", "same"),
    [
        # Meaning-preserving rewrites: these should share a key, and are where the exact tier
        # earns its keep.
        ("List three metrics worth tracking.", "list 3 metrics worth tracking", True),
        ("Summarise the incident report.", "Summarize the incident report.", True),
        ("Analyze the p99 latency spike.", "Analyse the p99 latency spike.", True),
        (
            "what is idempotency",
            "Hi, could you please help me with this - what is idempotency? Thanks!",
            True,
        ),
        ("for the queue, list the metrics", "list the metrics for the queue", True),
        # Meaning-changing edits: these must NOT share a key. Each one is a false hit the
        # embedding tier alone would have allowed, because a bi-encoder scores them ~0.99.
        ("Should I increase the timeout?", "Should I decrease the timeout?", False),
        ("What is the capital of Peru?", "What is the currency of Peru?", False),
        ("list three metrics", "list thirty metrics", False),
        ("Explain the design.", "Write the design.", False),
        ("reset my password", "reset my router", False),
    ],
)
def test_normalisation_semantics(left: str, right: str, same: bool) -> None:
    """The rule keeps politeness and spelling out, and keeps meaning in."""
    assert (
        same_question(left, right) is same
    ), f"{left!r} vs {right!r}\n  left:  {exact_key(left)!r}\n  right: {exact_key(right)!r}"


def test_instruction_verbs_are_not_filler() -> None:
    """A guard against the easiest way to inflate the hit rate.

    Adding instruction verbs to the filler list would make "explain X", "write X" and
    "summarise X" collide, which raises the measured hit rate while making the cache wrong. If
    someone does that, this fails.
    """
    from benchmarks.common.normalise import FILLER

    for verb in (
        "explain",
        "write",
        "draft",
        "summarise",
        "list",
        "compare",
        "describe",
        "review",
    ):
        assert verb not in FILLER, (
            f"{verb!r} is an instruction verb and must not be treated as filler: it changes what "
            f"is being asked, and folding it away buys hit rate with correctness"
        )
