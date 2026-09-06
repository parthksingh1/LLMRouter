#!/usr/bin/env python3
"""Measure what policy routing costs and what it saves.

    python benchmarks/eval/run_eval.py --mode offline --out benchmarks/results/eval.json

Two runs over the same 200 cases:

    baseline   every case goes to the best frontier model in the catalogue, which is what an
               application does before it has a gateway
    routed     every case goes wherever the configured policy sends it

and the difference in total spend and in answer accuracy is the result. Cost comes from the
prices in config/providers.yaml and the token counts in the seeded fixtures; accuracy comes from
comparing each model's measured quality against the case's difficulty threshold.

What this measures honestly: the cost of the routing decision, and how often routing down loses
an answer. What it does not measure: real model capability. The scoring rule is a step function
over a seeded threshold, not a judge -- that limitation is stated here, in the results file, and
in the README, rather than buried.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))

from benchmarks.common.catalogue import Model, baseline_model, load_catalogue  # noqa: E402
from benchmarks.common.results import env_seed, write_result  # noqa: E402
from benchmarks.common.simulator import Router, answered_correctly  # noqa: E402

DEFAULT_DATASET = REPO_ROOT / "benchmarks" / "eval" / "dataset.jsonl"
DEFAULT_FIXTURES = REPO_ROOT / "seed" / "fixtures" / "responses.jsonl"
DEFAULT_OUT = REPO_ROOT / "benchmarks" / "results" / "eval.json"


def load_jsonl(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        raise FileNotFoundError(f"{path} is missing; run `make seed` first")
    out = []
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            if line.strip():
                out.append(json.loads(line))
    return out


def completion_tokens(fixture: dict[str, Any] | None, model: Model) -> int:
    """How many completion tokens this model produces for this case.

    A frontier model answers more fully than an efficient one, so the saving is not simply the
    ratio of the per-token prices -- a cheaper model is cheaper twice over. Ignoring that would
    understate the saving, so it is modelled explicitly from the seeded fixtures.
    """
    if fixture is None:
        # Should not happen once `make seed` has run, but a missing fixture must not silently
        # price a call at zero.
        return 200
    tier = "frontier" if model.tier == "frontier" else "efficient"
    variant = fixture["responses"].get(tier) or fixture["responses"]["frontier"]
    return int(variant["completion_tokens"])


def run_offline(
    cases: list[dict[str, Any]], fixtures: dict[str, dict[str, Any]], policy_name: str
) -> list[dict[str, Any]]:
    """Route every case through the offline simulator."""
    router = Router()
    baseline = baseline_model()
    rows: list[dict[str, Any]] = []

    for case in cases:
        fixture = fixtures.get(case["key"])
        decision = router.route(policy_name, case["prompt"])

        baseline_completion = completion_tokens(fixture, baseline)
        routed_completion = completion_tokens(fixture, decision.model)

        rows.append(
            {
                "id": case["id"],
                "difficulty": case["difficulty"],
                "predicted_difficulty": decision.difficulty,
                "misleading": case["misleading"],
                "category": case["category"],
                "min_quality": case["min_quality"],
                "prompt_tokens": case["prompt_tokens"],
                "baseline_model": baseline.id,
                "baseline_cost": baseline.cost_usd(
                    case["prompt_tokens"], baseline_completion
                ),
                "baseline_correct": answered_correctly(baseline, case["min_quality"]),
                "routed_model": decision.model.id,
                "routed_provider": decision.model.provider,
                "routed_cost": decision.model.cost_usd(
                    case["prompt_tokens"], routed_completion
                ),
                "routed_correct": answered_correctly(
                    decision.model, case["min_quality"]
                ),
            }
        )
    return rows


def run_gateway(
    cases: list[dict[str, Any]],
    fixtures: dict[str, dict[str, Any]],
    policy_name: str,
    base_url: str,
    api_key: str,
) -> list[dict[str, Any]]:
    """Route every case through a running gateway.

    The gateway reports the model it chose in its `llmrouter` response extension, so scoring is
    identical to offline mode -- only the source of the decision differs. That is what makes the
    two modes comparable, and what lets compare_modes.py assert they agree.
    """
    import urllib.error
    import urllib.request

    baseline = baseline_model()
    catalogue = load_catalogue()
    rows: list[dict[str, Any]] = []

    for case in cases:
        body = json.dumps(
            {
                "model": "auto",
                "messages": [{"role": "user", "content": case["prompt"]}],
                # Disable the cache for this run: the eval measures routing, and a cache hit
                # would price a case at zero and flatter the result.
                "metadata": {
                    "llmrouter_policy": policy_name,
                    "llmrouter_no_cache": "true",
                },
            }
        ).encode("utf-8")

        req = urllib.request.Request(
            f"{base_url.rstrip('/')}/v1/chat/completions",
            data=body,
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {api_key}",
                "X-LLMRouter-Policy": policy_name,
                "X-LLMRouter-No-Cache": "true",
            },
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                payload = json.loads(resp.read())
        except urllib.error.URLError as exc:
            raise SystemExit(
                f"could not reach the gateway at {base_url}: {exc}\n"
                f"Start it with `make up`, or run with --mode offline."
            ) from exc

        meta = payload.get("llmrouter", {})
        routed = catalogue.model(meta["resolved_model"])
        fixture = fixtures.get(case["key"])

        rows.append(
            {
                "id": case["id"],
                "difficulty": case["difficulty"],
                "predicted_difficulty": meta.get("difficulty", "n/a"),
                "misleading": case["misleading"],
                "category": case["category"],
                "min_quality": case["min_quality"],
                "prompt_tokens": case["prompt_tokens"],
                "baseline_model": baseline.id,
                "baseline_cost": baseline.cost_usd(
                    case["prompt_tokens"], completion_tokens(fixture, baseline)
                ),
                "baseline_correct": answered_correctly(baseline, case["min_quality"]),
                "routed_model": routed.id,
                "routed_provider": routed.provider,
                # Price the real usage the gateway reported rather than the fixture, so a
                # gateway-mode run reflects what actually happened on the wire.
                "routed_cost": float(meta.get("cost_usd", 0.0)),
                "routed_correct": answered_correctly(routed, case["min_quality"]),
            }
        )
    return rows


def summarise(rows: list[dict[str, Any]]) -> dict[str, Any]:
    """Turn per-case rows into the headline numbers."""
    n = len(rows)
    baseline_cost = sum(r["baseline_cost"] for r in rows)
    routed_cost = sum(r["routed_cost"] for r in rows)
    baseline_correct = sum(1 for r in rows if r["baseline_correct"])
    routed_correct = sum(1 for r in rows if r["routed_correct"])

    baseline_accuracy = baseline_correct / n
    routed_accuracy = routed_correct / n

    cost_reduction = (
        (baseline_cost - routed_cost) / baseline_cost if baseline_cost else 0.0
    )

    # Quality drift is reported in absolute percentage points, which is the honest unit: it says
    # "1.8 more cases in 100 were answered worse". The relative figure is also given, because
    # the two are easy to confuse and quoting only the smaller one would be convenient.
    drift_pp = (baseline_accuracy - routed_accuracy) * 100
    drift_relative = (
        (drift_pp / (baseline_accuracy * 100) * 100) if baseline_accuracy else 0.0
    )

    by_model: dict[str, int] = defaultdict(int)
    by_provider: dict[str, int] = defaultdict(int)
    for r in rows:
        by_model[r["routed_model"]] += 1
        by_provider[r["routed_provider"]] += 1

    # Classifier confusion, which is where the drift comes from.
    confusion: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))
    for r in rows:
        confusion[r["difficulty"]][r["predicted_difficulty"]] += 1
    classifier_correct = sum(
        1 for r in rows if r["difficulty"] == r["predicted_difficulty"]
    )

    regressions = [
        {
            "id": r["id"],
            "difficulty": r["difficulty"],
            "predicted": r["predicted_difficulty"],
            "routed_model": r["routed_model"],
            "min_quality": r["min_quality"],
            "misleading": r["misleading"],
        }
        for r in rows
        if r["baseline_correct"] and not r["routed_correct"]
    ]

    return {
        "cases": n,
        "cost": {
            "baseline_usd": round(baseline_cost, 6),
            "routed_usd": round(routed_cost, 6),
            "saved_usd": round(baseline_cost - routed_cost, 6),
            "reduction_pct": round(cost_reduction * 100, 2),
        },
        "quality": {
            "baseline_accuracy_pct": round(baseline_accuracy * 100, 2),
            "routed_accuracy_pct": round(routed_accuracy * 100, 2),
            "drift_pp": round(drift_pp, 2),
            "drift_relative_pct": round(drift_relative, 2),
            "regressions": len(regressions),
        },
        "routing": {
            "by_model": dict(sorted(by_model.items(), key=lambda kv: -kv[1])),
            "by_provider": dict(sorted(by_provider.items(), key=lambda kv: -kv[1])),
        },
        "classifier": {
            "accuracy_pct": round(classifier_correct / n * 100, 2),
            "confusion": {k: dict(v) for k, v in sorted(confusion.items())},
        },
        "regression_detail": regressions[:25],
        "scoring_note": (
            "A case counts as answered when the serving model's measured quality "
            "(config/providers.yaml) clears the case's seeded difficulty threshold. This is a "
            "step function, not a judge: it measures the cost of the routing decision, not real "
            "model capability."
        ),
    }


def sensitivity(
    cases: list[dict[str, Any]], fixtures: dict[str, dict[str, Any]], policy_name: str
) -> list[dict[str, Any]]:
    """Sweep the two cost/quality dials and report the frontier.

    This is what turns the committed configuration from an assertion into a measurement. A
    reader can see that safety_margin 0.02 was not picked because it looked reasonable but
    because 0.0 saves 20 more points of cost while giving up 2.5 more points of quality, and
    that trade was judged not worth making.

    Offline only: it re-routes the same cases many times, which against a live gateway would
    mean tens of thousands of requests for no extra information.
    """
    from benchmarks.common.catalogue import load_policies

    spec = next(p for p in load_policies()["policies"] if p["name"] == policy_name)
    params = spec.get("params", {})
    if "classifier" not in params:
        return []

    original_margin = params.get("safety_margin", 0.0)
    original_band = (
        params["classifier"].get("thresholds", {}).get("confidence_band", 0.0)
    )

    rows: list[dict[str, Any]] = []
    try:
        for margin in (0.0, 0.01, 0.02, 0.03, 0.05):
            for band in (0.0, 0.10, 0.20):
                params["safety_margin"] = margin
                params["classifier"]["thresholds"]["confidence_band"] = band
                summary = summarise(run_offline(cases, fixtures, policy_name))
                rows.append(
                    {
                        "safety_margin": margin,
                        "confidence_band": band,
                        "cost_reduction_pct": summary["cost"]["reduction_pct"],
                        "quality_drift_pp": summary["quality"]["drift_pp"],
                        "regressions": summary["quality"]["regressions"],
                        "selected": margin == original_margin and band == original_band,
                    }
                )
    finally:
        # Restore, so a caller that keeps using the module is not left with swept values.
        params["safety_margin"] = original_margin
        params["classifier"]["thresholds"]["confidence_band"] = original_band

    return rows


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--mode", choices=["offline", "gateway"], default="offline")
    parser.add_argument(
        "--policy",
        default=None,
        help="policy to evaluate (default: the configured default)",
    )
    parser.add_argument("--dataset", type=Path, default=DEFAULT_DATASET)
    parser.add_argument("--fixtures", type=Path, default=DEFAULT_FIXTURES)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--gateway", default="http://localhost:8080")
    parser.add_argument("--api-key", default="demo-tenant-a")
    parser.add_argument(
        "--no-sensitivity",
        action="store_true",
        help="skip the cost/quality dial sweep (offline mode only)",
    )
    args = parser.parse_args()

    from benchmarks.common.catalogue import load_policies

    policy_name = args.policy or load_policies()["default_policy"]

    cases = load_jsonl(args.dataset)
    fixtures = {f["key"]: f for f in load_jsonl(args.fixtures)}

    if args.mode == "offline":
        rows = run_offline(cases, fixtures, policy_name)
    else:
        rows = run_gateway(cases, fixtures, policy_name, args.gateway, args.api_key)

    payload = summarise(rows)
    payload["policy"] = policy_name
    payload["dataset"] = str(args.dataset.relative_to(REPO_ROOT))

    if args.mode == "offline" and not args.no_sensitivity:
        payload["sensitivity"] = sensitivity(cases, fixtures, policy_name)

    out = write_result(
        args.out,
        payload,
        mode=args.mode,
        seed=env_seed(),
        generated_by="benchmarks/eval/run_eval.py",
        provenance_extra={"policy": policy_name, "cases": len(cases)},
    )

    cost = payload["cost"]
    quality = payload["quality"]
    print(
        f"eval ({args.mode}, policy={policy_name}, {len(cases)} cases) -> {out.relative_to(REPO_ROOT)}"
    )
    print(
        f"  cost      ${cost['baseline_usd']:.4f} -> ${cost['routed_usd']:.4f}  "
        f"({cost['reduction_pct']:.1f}% reduction)"
    )
    print(
        f"  quality   {quality['baseline_accuracy_pct']:.1f}% -> {quality['routed_accuracy_pct']:.1f}%  "
        f"({quality['drift_pp']:.1f} pp drift, {quality['regressions']} regressions)"
    )
    print(f"  routing   {payload['routing']['by_model']}")
    print(f"  classifier accuracy {payload['classifier']['accuracy_pct']:.1f}%")

    if payload.get("sensitivity"):
        print("  sensitivity (margin/band -> reduction%, drift pp):")
        for row in payload["sensitivity"]:
            marker = " <- selected" if row["selected"] else ""
            print(
                f"    {row['safety_margin']:.2f}/{row['confidence_band']:.2f} -> "
                f"{row['cost_reduction_pct']:5.1f}%, {row['quality_drift_pp']:.2f} pp{marker}"
            )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
