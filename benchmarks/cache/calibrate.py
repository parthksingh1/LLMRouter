#!/usr/bin/env python3
"""Calibrate the semantic cache.

    python benchmarks/cache/calibrate.py --out benchmarks/results/cache_calibration.json \\
        --plot docs/diagrams/cache_roc.png

The cache admits a cached answer for a new prompt through two tiers:

    exact     the prompts reduce to the same content-token set (benchmarks/common/normalise.py).
              Certain, and costs nothing but a hash lookup.
    semantic  the prompts embed close enough together. Probabilistic, and this is the threshold
              being calibrated.

Both are measured against 500 paraphrase pairs that should hit and 500 hard negatives that must
not. The operating point is the similarity that maximises additional hits subject to a false-hit
budget, because the two error types are not interchangeable: a missed hit costs a few cents and
a second of latency, while a false hit is a confidently wrong answer.

The headline result of this calibration is a negative one, and it drove the design: a bi-encoder
cannot separate a minimal pair ("increase the timeout" against "decrease the timeout") from a
genuine paraphrase. Those negatives score *higher* than most positives, so no threshold makes
the semantic tier both useful and safe on its own. The exact tier does the safe work; the
semantic tier is calibrated to add only what it can add within budget.

Backends: `onnx` (fastembed, what the sidecar serves in EMBEDDER_MODE=onnx and what the
committed result uses), `hash` (the dependency-free trigram projection), `http` (a live sidecar).
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Callable

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / "services" / "embedder"))

from app.main import cosine, hash_embed  # noqa: E402
from benchmarks.common.normalise import same_question  # noqa: E402
from benchmarks.common.results import env_seed, write_result  # noqa: E402

DEFAULT_PAIRS = REPO_ROOT / "benchmarks" / "cache" / "pairs.jsonl"
DEFAULT_OUT = REPO_ROOT / "benchmarks" / "results" / "cache_calibration.json"

#: The false-hit budget: at most 0.5% of near-miss lookups may serve the wrong answer.
#:
#: This is a product decision written as a number, and it is the one worth arguing about. A
#: stricter budget costs hit rate, and therefore latency and money. The sweep below shows what
#: each choice would cost, so the number can be argued with rather than merely accepted.
FALSE_HIT_BUDGET = 0.005


def unit(vec: list[float]) -> list[float]:
    """Normalise so cosine similarity is a plain dot product."""
    magnitude = sum(x * x for x in vec) ** 0.5
    return vec if magnitude == 0 else [x / magnitude for x in vec]


def load_pairs(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        raise FileNotFoundError(f"{path} is missing; run `make seed-pairs` first")
    with path.open(encoding="utf-8") as fh:
        return [json.loads(line) for line in fh if line.strip()]


def build_embedder(
    kind: str, texts: list[str], model_name: str, dim: int, url: str
) -> Callable[[str], list[float]]:
    """Return a text -> vector function for the chosen backend."""
    if kind == "hash":
        return lambda text: hash_embed(text, dim)

    if kind == "onnx":
        try:
            from fastembed import TextEmbedding
        except ImportError as exc:
            raise SystemExit(
                "--embedder onnx needs fastembed (`pip install fastembed`). Use --embedder hash "
                "for a dependency-free run; the results file records which was used."
            ) from exc
        # One batched pass: embedding 2,000 texts individually takes minutes.
        model = TextEmbedding(model_name=model_name)
        unique = sorted(set(texts))
        table = dict(
            zip(
                unique,
                ([float(x) for x in v] for v in model.embed(unique)),
                strict=True,
            )
        )
        return lambda text: table[text]

    import urllib.error
    import urllib.request

    cache: dict[str, list[float]] = {}

    def embed(text: str) -> list[float]:
        if text not in cache:
            body = json.dumps({"texts": [text]}).encode("utf-8")
            req = urllib.request.Request(
                f"{url.rstrip('/')}/embed",
                data=body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            try:
                with urllib.request.urlopen(req, timeout=30) as resp:
                    cache[text] = json.loads(resp.read())["vectors"][0]
            except urllib.error.URLError as exc:
                raise SystemExit(
                    f"could not reach the embedder at {url}: {exc}"
                ) from exc
        return cache[text]

    return embed


def sweep(rows: list[dict[str, Any]], steps: int = 400) -> list[dict[str, float]]:
    """Sweep the semantic threshold over the pairs the exact tier did not already resolve.

    The sweep deliberately reports rates over ALL pairs, not over the residual population: a
    caller cares what fraction of their traffic is served wrongly, not what fraction of some
    intermediate subset is.
    """
    positives = [r for r in rows if r["label"] == 1]
    negatives = [r for r in rows if r["label"] == 0]
    if not positives or not negatives:
        raise ValueError("the pair set needs both positives and negatives")

    exact_hits = sum(1 for r in positives if r["exact"])
    exact_false = sum(1 for r in negatives if r["exact"])

    similarities = [r["similarity"] for r in rows]
    lo, hi = min(similarities), max(similarities)
    span = (hi - lo) or 1.0

    curve: list[dict[str, float]] = []
    for i in range(steps + 1):
        threshold = lo + span * i / steps
        semantic_hits = sum(
            1 for r in positives if not r["exact"] and r["similarity"] >= threshold
        )
        semantic_false = sum(
            1 for r in negatives if not r["exact"] and r["similarity"] >= threshold
        )

        curve.append(
            {
                "threshold": round(threshold, 6),
                "hit_rate": round((exact_hits + semantic_hits) / len(positives), 6),
                "false_hit_rate": round(
                    (exact_false + semantic_false) / len(negatives), 6
                ),
                "semantic_hit_rate": round(semantic_hits / len(positives), 6),
                "semantic_false_hit_rate": round(semantic_false / len(negatives), 6),
            }
        )
    return curve


def choose_operating_point(
    curve: list[dict[str, float]], budget: float
) -> dict[str, float]:
    """Highest total hit rate whose false-hit rate stays inside the budget.

    Ties are broken towards the *higher* threshold, so the operating point sits as far from the
    negatives as the budget allows rather than balanced on the edge of it.
    """
    admissible = [p for p in curve if p["false_hit_rate"] <= budget]
    if (
        not admissible
    ):  # pragma: no cover - only if the exact tier alone blows the budget
        return max(curve, key=lambda p: p["threshold"])
    return max(admissible, key=lambda p: (p["hit_rate"], p["threshold"]))


def auc(rows: list[dict[str, Any]]) -> float:
    """Area under the ROC of the similarity score alone, as a summary of separability."""
    positives = sorted(r["similarity"] for r in rows if r["label"] == 1)
    negatives = sorted(r["similarity"] for r in rows if r["label"] == 0)
    if not positives or not negatives:
        return 0.0
    # Mann-Whitney U / (n*m) is exactly the AUC, and avoids binning artefacts.
    wins = 0.0
    for p in positives:
        wins += sum(1.0 if p > n else 0.5 if p == n else 0.0 for n in negatives)
    return wins / (len(positives) * len(negatives))


def render_plot(
    curve: list[dict[str, float]],
    operating: dict[str, float],
    rows: list[dict[str, Any]],
    path: Path,
    backend: str,
) -> bool:
    """Draw the ROC curve and the per-kind similarity distributions."""
    try:
        import matplotlib

        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        return False

    fig, (ax_roc, ax_dist) = plt.subplots(1, 2, figsize=(12, 4.8))

    fpr = [p["false_hit_rate"] for p in curve]
    tpr = [p["hit_rate"] for p in curve]
    ax_roc.plot(
        fpr,
        tpr,
        color="#2563eb",
        linewidth=2,
        label=f"two-tier (AUC {auc(rows):.3f} on similarity)",
    )
    ax_roc.scatter(
        [operating["false_hit_rate"]],
        [operating["hit_rate"]],
        color="#dc2626",
        zorder=5,
        s=70,
        label=f"operating point (t={operating['threshold']:.4f})",
    )
    ax_roc.axvline(
        FALSE_HIT_BUDGET, color="#dc2626", linestyle="--", linewidth=1, alpha=0.7
    )
    ax_roc.text(
        FALSE_HIT_BUDGET + 0.005,
        0.05,
        f"budget {FALSE_HIT_BUDGET:.1%}",
        fontsize=8,
        color="#dc2626",
    )
    ax_roc.set_xlim(-0.01, 0.25)
    ax_roc.set_xlabel("false-hit rate (wrong answer served)")
    ax_roc.set_ylabel("hit rate (paraphrase served from cache)")
    ax_roc.set_title(f"Cache admission - {backend} embeddings")
    ax_roc.legend(loc="lower right", fontsize=8)
    ax_roc.grid(alpha=0.25)

    kinds = sorted({r["kind"] for r in rows})
    colours = {
        "paraphrase": "#16a34a",
        "minimal_pair": "#dc2626",
        "subject_swap": "#ea580c",
        "different_question": "#6b7280",
    }
    for kind in kinds:
        values = [r["similarity"] for r in rows if r["kind"] == kind]
        ax_dist.hist(
            values,
            bins=40,
            alpha=0.55,
            label=f"{kind} (n={len(values)})",
            color=colours.get(kind, "#3b82f6"),
        )
    ax_dist.axvline(
        operating["threshold"],
        color="#111827",
        linestyle="--",
        linewidth=1.2,
        label=f"threshold {operating['threshold']:.4f}",
    )
    ax_dist.set_xlabel("cosine similarity")
    ax_dist.set_ylabel("pairs")
    ax_dist.set_title("Why the semantic tier alone is not enough")
    ax_dist.legend(fontsize=7)
    ax_dist.grid(alpha=0.25)

    fig.tight_layout()
    path.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(path, dpi=130)
    plt.close(fig)
    return True


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--pairs", type=Path, default=DEFAULT_PAIRS)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument(
        "--plot", type=Path, default=REPO_ROOT / "docs" / "diagrams" / "cache_roc.png"
    )
    parser.add_argument("--embedder", choices=["onnx", "hash", "http"], default="onnx")
    parser.add_argument("--embedder-url", default="http://localhost:8001")
    parser.add_argument("--model", default="BAAI/bge-small-en-v1.5")
    parser.add_argument("--dim", type=int, default=384)
    parser.add_argument("--budget", type=float, default=FALSE_HIT_BUDGET)
    args = parser.parse_args()

    pairs = load_pairs(args.pairs)
    texts = [t for pair in pairs for t in (pair["left"], pair["right"])]
    embed = build_embedder(
        args.embedder, texts, args.model, args.dim, args.embedder_url
    )

    rows: list[dict[str, Any]] = []
    for pair in pairs:
        rows.append(
            {
                "label": int(pair["label"]),
                "kind": pair["kind"],
                "exact": same_question(pair["left"], pair["right"]),
                "similarity": cosine(
                    unit(embed(pair["left"])), unit(embed(pair["right"]))
                ),
            }
        )

    curve = sweep(rows)
    operating = choose_operating_point(curve, args.budget)

    positives = [r for r in rows if r["label"] == 1]
    negatives = [r for r in rows if r["label"] == 0]
    exact_hits = sum(1 for r in positives if r["exact"])
    exact_false = sum(1 for r in negatives if r["exact"])

    by_kind: dict[str, dict[str, Any]] = {}
    for kind in sorted({r["kind"] for r in rows}):
        group = [r for r in rows if r["kind"] == kind]
        sims = [r["similarity"] for r in group]
        admitted = sum(
            1 for r in group if r["exact"] or r["similarity"] >= operating["threshold"]
        )
        by_kind[kind] = {
            "count": len(group),
            "label": group[0]["label"],
            "mean_similarity": round(sum(sims) / len(sims), 6),
            "max_similarity": round(max(sims), 6),
            "exact_tier_matches": sum(1 for r in group if r["exact"]),
            "admitted_at_operating_point": admitted,
            "admitted_pct": round(admitted / len(group) * 100, 2),
        }

    payload: dict[str, Any] = {
        "embedder": args.embedder,
        "model": args.model
        if args.embedder == "onnx"
        else f"{args.embedder}-{args.dim}",
        "pairs": {
            "total": len(pairs),
            "positive": len(positives),
            "negative": len(negatives),
        },
        "false_hit_budget": args.budget,
        "operating_point": operating,
        "tiers": {
            "exact": {
                "hit_rate": round(exact_hits / len(positives), 6),
                "false_hit_rate": round(exact_false / len(negatives), 6),
                "description": "content-token equality after conservative normalisation",
            },
            "semantic": {
                "threshold": operating["threshold"],
                "incremental_hit_rate": operating["semantic_hit_rate"],
                "incremental_false_hit_rate": operating["semantic_false_hit_rate"],
                "description": "cosine similarity on pairs the exact tier did not resolve",
            },
        },
        "similarity_auc": round(auc(rows), 6),
        "by_kind": by_kind,
        "curve": curve,
        "finding": (
            "Minimal pairs -- one word changed, meaning inverted -- embed closer together than "
            "many genuine paraphrases, so similarity alone cannot be pushed to a useful hit rate "
            "within the false-hit budget. The exact tier therefore carries the safe work and the "
            "semantic tier is calibrated to add only what fits in the budget. A production system "
            "wanting more would add a cross-encoder re-rank over the top-k candidates rather than "
            "lowering this threshold."
        ),
    }

    plotted = render_plot(curve, operating, rows, args.plot, args.embedder)
    # Is the semantic tier earning its budget? Report the marginal trade explicitly rather
    # than letting "maximise hit rate within budget" quietly recommend a bad threshold.
    marginal_true = operating["semantic_hit_rate"] * len(positives)
    marginal_false = operating["semantic_false_hit_rate"] * len(negatives)
    if marginal_true > 2 * marginal_false:
        verdict = "worthwhile"
        recommendation = "enable the semantic tier at the operating point"
    elif marginal_true > marginal_false:
        verdict = "marginal"
        recommendation = (
            "enable only if a missed hit is much more expensive than a wrong answer"
        )
    else:
        verdict = "not worthwhile on this pair set"
        recommendation = (
            "the semantic tier admits at least as many wrong answers as extra right ones on "
            "this deliberately adversarial near-miss set. Set CACHE_SIMILARITY_THRESHOLD above "
            "1.0 to disable it, or accept it knowing that real near-miss traffic contains far "
            "fewer minimal pairs than the 40% in this set -- which is what "
            "benchmarks/results/cache_bench.json measures."
        )

    payload["semantic_tier_verdict"] = {
        "verdict": verdict,
        "marginal_true_hits": marginal_true,
        "marginal_false_hits": marginal_false,
        "recommendation": recommendation,
    }

    payload["plot"] = str(args.plot.relative_to(REPO_ROOT)) if plotted else None

    out = write_result(
        args.out,
        payload,
        mode="offline",
        seed=env_seed(),
        generated_by="benchmarks/cache/calibrate.py",
        provenance_extra={"embedder": args.embedder, "model": payload["model"]},
    )

    print(
        f"cache calibration ({args.embedder}, {len(pairs)} pairs) -> {out.relative_to(REPO_ROOT)}"
    )
    print(
        f"  exact tier      {payload['tiers']['exact']['hit_rate'] * 100:5.1f}% hit, "
        f"{payload['tiers']['exact']['false_hit_rate'] * 100:.2f}% false-hit"
    )
    print(
        f"  semantic tier  +{operating['semantic_hit_rate'] * 100:5.1f}% hit, "
        f"+{operating['semantic_false_hit_rate'] * 100:.2f}% false-hit  (t={operating['threshold']:.4f})"
    )
    print(
        f"  combined        {operating['hit_rate'] * 100:5.1f}% hit, "
        f"{operating['false_hit_rate'] * 100:.2f}% false-hit  (budget {args.budget * 100:.1f}%)"
    )
    print(f"  similarity AUC  {payload['similarity_auc']:.3f}")
    print()
    for kind, stats in by_kind.items():
        verdict = "should hit" if stats["label"] == 1 else "must not hit"
        print(
            f"    {kind:20} n={stats['count']:4}  mean sim {stats['mean_similarity']:.3f}  "
            f"admitted {stats['admitted_pct']:5.1f}%  ({verdict})"
        )
    print()
    print(
        f"  semantic tier: {payload['semantic_tier_verdict']['verdict']} "
        f"(+{marginal_true:.0f} right, +{marginal_false:.0f} wrong)"
    )
    print(f"  Set CACHE_SIMILARITY_THRESHOLD={operating['threshold']:.4f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
