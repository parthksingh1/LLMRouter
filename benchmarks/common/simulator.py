"""An offline reimplementation of the gateway's routing decision.

This module exists so `make bench` reproduces every number on a machine with nothing but Python
-- no Docker, no Go, no running stack. It is the one place in the repository where logic is
deliberately written twice, and that duplication is a real risk: a simulator that drifts from the
router turns an honest benchmark into a flattering one.

Three things keep it honest:

1. It reads the *same* `config/policies.yaml` and `config/providers.yaml` the gateway reads.
   Weights, thresholds, quality floors, the safety margin and prices are never duplicated -- only
   the ~60 lines of arithmetic that combine them.
2. `benchmarks/compare_modes.py` runs the eval in both modes and fails if the cost saving differs
   by more than a small tolerance. CI's integration job runs it against the live stack.
3. Every results file records which mode produced it, so a reader always knows what they are
   looking at.

If you change the router, change this too, and let compare_modes.py tell you whether you got it
right. See docs/adr/0007-offline-benchmark-simulator.md.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .catalogue import Catalogue, Model, load_catalogue, load_policies, policy

#: Mirrors referenceChars in services/gateway/internal/router/classifier.go.
REFERENCE_CHARS = 1200.0

#: Mirrors the keyword saturation constant in the same file.
KEYWORD_SATURATION = 3.0

#: Markers the Go classifier treats as evidence of code.
CODE_MARKERS = ("def ", "func ", "class ", "SELECT ", "import ", "#include", "=> ", "};")


@dataclass(frozen=True)
class Features:
    """The classifier's inputs and its score, mirroring router.Features in Go."""

    length: float
    keywords: float
    code: float
    multi_turn: float
    score: float


@dataclass(frozen=True)
class Decision:
    """One routing decision."""

    model: Model
    difficulty: str
    reason: str


def _clamp01(v: float) -> float:
    return max(0.0, min(1.0, v))


def _clamp_signed(v: float) -> float:
    return max(-1.0, min(1.0, v))


def classify(prompt: str, turns: int, classifier_cfg: dict[str, Any]) -> tuple[str, Features]:
    """Reproduce the Go difficulty classifier exactly.

    Any change here without the same change in classifier.go will show up as a compare_modes
    failure rather than as a quietly wrong benchmark.
    """
    weights = classifier_cfg.get("weights", {})
    thresholds = classifier_cfg.get("thresholds", {})
    hard_keywords = [k.lower() for k in classifier_cfg.get("hard_keywords", [])]
    easy_keywords = [k.lower() for k in classifier_cfg.get("easy_keywords", [])]

    w_len = float(weights.get("length", 0.28))
    w_kw = float(weights.get("keywords", 0.46))
    w_code = float(weights.get("code", 0.14))
    w_multi = float(weights.get("multi_turn", 0.12))
    easy_below = float(thresholds.get("easy_below", 0.22))
    hard_above = float(thresholds.get("hard_above", 0.44))
    band = float(thresholds.get("confidence_band", 0.0))

    lower = prompt.lower()

    length = _clamp01(len(prompt) / REFERENCE_CHARS)

    hard_hits = sum(1 for k in hard_keywords if k in lower)
    easy_hits = sum(1 for k in easy_keywords if k in lower)
    keywords = _clamp01(hard_hits / KEYWORD_SATURATION) - _clamp01(easy_hits / KEYWORD_SATURATION)

    code = 1.0 if _looks_like_code(prompt) else 0.0
    multi_turn = _clamp01((turns - 1) / 5.0)

    # Clamped to -1..1, not 0..1: the sign is what separates "easy" (more easy markers than
    # hard ones) from "medium" (neither). See the matching comment in classifier.go.
    score = _clamp_signed(w_len * length + w_kw * keywords + w_code * code + w_multi * multi_turn)

    return _bucket(score, easy_below, hard_above, band), Features(length, keywords, code, multi_turn, score)


def _bucket(score: float, easy_below: float, hard_above: float, band: float) -> str:
    """Mirror Classifier.bucket in Go, including the confidence band.

    A score within `band` below a boundary is promoted to the harder bucket, because
    under-serving a hard prompt costs an answer while over-serving an easy one costs a fraction
    of a cent.
    """
    if score >= hard_above:
        return "hard"
    if score < easy_below:
        return "medium" if easy_below - score < band else "easy"
    return "hard" if hard_above - score < band else "medium"


def _looks_like_code(text: str) -> bool:
    """Mirror looksLikeCode in classifier.go."""
    if "```" in text:
        return True
    if any(marker in text for marker in CODE_MARKERS):
        return True
    return any(line.startswith(("    ", "\t")) for line in text.split("\n")[1:])


class Router:
    """The offline routing engine."""

    def __init__(self, catalogue: Catalogue | None = None) -> None:
        self.catalogue = catalogue or load_catalogue()
        self.policies = load_policies()

    def route(self, policy_name: str, prompt: str, turns: int = 1) -> Decision:
        """Return the model the gateway would choose under `policy_name`."""
        spec = policy(policy_name)
        strategy = spec["strategy"]
        params = spec.get("params", {})

        if strategy == "quality_tiered":
            return self._quality_tiered(prompt, turns, params)
        if strategy == "cost_optimized":
            return self._cost_optimized(params)
        if strategy == "latency_optimized":
            return self._latency_optimized(params)
        raise NotImplementedError(
            f"the offline simulator does not model the {strategy!r} strategy. "
            f"weighted_round_robin and canary are stateful A/B mechanisms; benchmark them "
            f"against the live gateway with MODE=gateway."
        )

    # -- strategies ---------------------------------------------------------------------

    def _quality_tiered(self, prompt: str, turns: int, params: dict[str, Any]) -> Decision:
        difficulty, _ = classify(prompt, turns, params.get("classifier", {}))

        floors = {b["name"]: float(b["quality_floor"]) for b in params.get("buckets", [])}
        margin = float(params.get("safety_margin", 0.0))
        floor = floors.get(difficulty, floors.get("hard", 0.9))
        effective = floor + margin

        eligible = [m for m in self.catalogue.models if m.quality >= effective]
        if not eligible:
            best = max(self.catalogue.models, key=lambda m: (m.quality, -m.estimated_cost))
            return Decision(best, difficulty, f"nothing cleared quality {effective:.3f}; used the best available")

        chosen = min(eligible, key=lambda m: (m.estimated_cost, -m.quality, m.id))
        return Decision(
            chosen,
            difficulty,
            f"{difficulty} prompt: cheapest model above quality {effective:.3f}",
        )

    def _cost_optimized(self, params: dict[str, Any]) -> Decision:
        floor = float(params.get("quality_floor", 0.0))
        max_ttfb = float(params.get("max_ttfb_p99_ms", 0) or 0)

        eligible = []
        for m in self.catalogue.models:
            if m.quality < floor:
                continue
            if max_ttfb and self.catalogue.provider(m.provider).ttfb_p99_ms > max_ttfb:
                continue
            eligible.append(m)

        if not eligible:
            best = max(self.catalogue.models, key=lambda m: (m.quality, -m.estimated_cost))
            return Decision(best, "n/a", f"nothing cleared quality {floor:.2f}; used the best available")

        chosen = min(eligible, key=lambda m: (m.estimated_cost, -m.quality, m.id))
        return Decision(chosen, "n/a", f"cheapest model above quality {floor:.2f}")

    def _latency_optimized(self, params: dict[str, Any]) -> Decision:
        floor = float(params.get("quality_floor", 0.0))
        eligible = [m for m in self.catalogue.models if m.quality >= floor] or list(self.catalogue.models)

        chosen = min(
            eligible,
            key=lambda m: (self.catalogue.provider(m.provider).ttfb_p50_ms, m.estimated_cost, m.id),
        )
        return Decision(chosen, "n/a", f"lowest configured TTFB above quality {floor:.2f}")


def answered_correctly(model: Model, min_quality: float) -> bool:
    """Whether a model answers a case whose difficulty threshold is `min_quality`.

    This is the scoring rule the eval set is built around, and it is a simplification worth
    stating out loud: real quality is graded, not a step function, and a real evaluation needs
    either human judgement or a model-based judge. What this rule does capture faithfully is the
    property the router actually controls -- that routing down costs answers on exactly those
    cases whose difficulty exceeds the cheaper model's measured capability.
    """
    return model.quality >= min_quality
