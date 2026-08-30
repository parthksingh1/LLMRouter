#!/usr/bin/env python3
"""Find runaway workloads in the seeded month of spend.

    python benchmarks/attribution/run_attribution.py --mode offline \\
        --out benchmarks/results/attribution.json

The detector gets no hints. `seed_profile` in config/tenants.yaml says which tenants were
generated as pathological, and this benchmark reads it only at the very end, to score itself.
The detection runs on daily spend alone -- the same signal an on-call engineer has.

Two modes:

    offline  reads the generated TSV directly, so it reproduces without Docker
    gateway  queries ClickHouse through the same rollup view the dashboard uses

Both run the identical detector from benchmarks/common/anomaly.py, which is also what the
dashboard imports. The number in the results file and the number on the screen cannot disagree.
"""

from __future__ import annotations

import argparse
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))

from benchmarks.common.anomaly import Anomaly, detect  # noqa: E402
from benchmarks.common.catalogue import load_tenants  # noqa: E402
from benchmarks.common.results import env_seed, write_result  # noqa: E402

DEFAULT_EVENTS = REPO_ROOT / "seed" / "out" / "events.tsv"
DEFAULT_OUT = REPO_ROOT / "benchmarks" / "results" / "attribution.json"

#: Column order written by seed/generate_traffic.py.
COLUMNS = [
    "ts", "request_id", "tenant_id", "policy", "model", "provider",
    "prompt_tokens", "completion_tokens", "cached", "cache_similarity",
    "guardrail_blocked", "guardrail_findings", "failover_count",
    "status_code", "latency_ms", "cost_usd",
]


def load_offline(path: Path) -> tuple[dict[str, dict[str, float]], dict[str, Any]]:
    """Aggregate the generated TSV into daily spend per tenant."""
    if not path.exists():
        raise FileNotFoundError(
            f"{path} is missing. Generate it with:\n"
            f"  python seed/generate_traffic.py --days 30 --target 200000"
        )

    index = {name: i for i, name in enumerate(COLUMNS)}
    daily: dict[str, dict[str, float]] = defaultdict(lambda: defaultdict(float))

    events = 0
    cached = 0
    blocked = 0
    tokens = 0
    by_model: dict[str, float] = defaultdict(float)

    with path.open(encoding="utf-8") as fh:
        for line in fh:
            if not line.strip():
                continue
            parts = line.rstrip("\n").split("\t")
            events += 1

            day = parts[index["ts"]][:10]
            tenant = parts[index["tenant_id"]]
            cost = float(parts[index["cost_usd"]])
            is_cached = parts[index["cached"]] == "1"

            # A cached request is billed at zero. The TSV records the counterfactual cost so the
            # saving is measurable, so it must be excluded here or the attribution would charge
            # tenants for requests they never paid for.
            if not is_cached:
                daily[tenant][day] += cost
                by_model[parts[index["model"]]] += cost
            else:
                cached += 1
                # Ensure the day exists even when every request that day was a cache hit,
                # otherwise the series has holes and the baseline is computed over fewer days.
                daily[tenant].setdefault(day, 0.0)

            if parts[index["guardrail_blocked"]] == "1":
                blocked += 1
            tokens += int(parts[index["prompt_tokens"]]) + int(parts[index["completion_tokens"]])

    # Fill gaps. A day on which a tenant sent nothing is a real $0, and leaving it out shortens
    # the series and hides exactly the quiet-then-expensive pattern the detector looks for.
    all_days = sorted({d for days in daily.values() for d in days})
    for tenant_days in daily.values():
        for day in all_days:
            tenant_days.setdefault(day, 0.0)

    stats = {
        "events": events,
        "cached_requests": cached,
        "cache_hit_rate_pct": round(cached / events * 100, 2) if events else 0.0,
        "guardrail_blocked": blocked,
        "guardrail_block_rate_pct": round(blocked / events * 100, 3) if events else 0.0,
        "total_tokens": tokens,
        "spend_by_model": {k: round(v, 4) for k, v in sorted(by_model.items(), key=lambda kv: -kv[1])},
    }
    return {t: dict(days) for t, days in daily.items()}, stats


def load_gateway(url: str, database: str) -> tuple[dict[str, dict[str, float]], dict[str, Any]]:
    """Read daily spend from ClickHouse, through the same view the dashboard uses."""
    import json
    import urllib.parse
    import urllib.request

    def query(sql: str) -> list[dict[str, Any]]:
        params = urllib.parse.urlencode({"database": database, "default_format": "JSONEachRow"})
        req = urllib.request.Request(f"{url.rstrip('/')}/?{params}", data=sql.encode("utf-8"), method="POST")
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                body = resp.read().decode("utf-8")
        except OSError as exc:
            raise SystemExit(
                f"could not query ClickHouse at {url}: {exc}\n"
                f"Start the stack with `make up`, or run with --mode offline."
            ) from exc
        return [json.loads(line) for line in body.splitlines() if line.strip()]

    rows = query(
        "SELECT day, tenant_id, cost_usd FROM v_tenant_daily "
        "WHERE day >= today() - 45 ORDER BY tenant_id, day"
    )
    daily: dict[str, dict[str, float]] = defaultdict(dict)
    for row in rows:
        daily[row["tenant_id"]][str(row["day"])] = float(row["cost_usd"])

    totals = query(
        "SELECT count() AS events, sum(cached) AS cached, sum(guardrail_blocked) AS blocked, "
        "sum(prompt_tokens + completion_tokens) AS tokens FROM events"
    )
    t = totals[0] if totals else {"events": 0, "cached": 0, "blocked": 0, "tokens": 0}
    events = int(t["events"]) or 1

    models = query("SELECT model, sum(cost_usd) AS cost FROM events GROUP BY model ORDER BY cost DESC")

    stats = {
        "events": int(t["events"]),
        "cached_requests": int(t["cached"]),
        "cache_hit_rate_pct": round(int(t["cached"]) / events * 100, 2),
        "guardrail_blocked": int(t["blocked"]),
        "guardrail_block_rate_pct": round(int(t["blocked"]) / events * 100, 3),
        "total_tokens": int(t["tokens"]),
        "spend_by_model": {m["model"]: round(float(m["cost"]), 4) for m in models},
    }
    return dict(daily), stats


def score(anomalies: list[Anomaly], planted: set[str]) -> dict[str, Any]:
    """Score the detector against the tenants the generator actually made pathological.

    Read only here, at the end. The detector never sees it.
    """
    found = {a.tenant_id for a in anomalies}
    true_positives = found & planted
    false_positives = found - planted
    false_negatives = planted - found

    precision = len(true_positives) / len(found) if found else 0.0
    recall = len(true_positives) / len(planted) if planted else 0.0

    return {
        "planted": sorted(planted),
        "detected": sorted(found),
        "true_positives": sorted(true_positives),
        "false_positives": sorted(false_positives),
        "false_negatives": sorted(false_negatives),
        "precision": round(precision, 4),
        "recall": round(recall, 4),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mode", choices=["offline", "gateway"], default="offline")
    parser.add_argument("--events", type=Path, default=DEFAULT_EVENTS)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--clickhouse", default="http://localhost:8123")
    parser.add_argument("--database", default="llmrouter")
    parser.add_argument("--threshold", type=float, default=2.5)
    args = parser.parse_args()

    if args.mode == "offline":
        daily, stats = load_offline(args.events)
    else:
        daily, stats = load_gateway(args.clickhouse, args.database)

    anomalies = detect(daily, threshold=args.threshold)

    total_spend = sum(sum(days.values()) for days in daily.values())
    flagged_spend = sum(a.total_usd for a in anomalies)

    tenants = load_tenants()["tenants"]
    planted = {
        t["id"] for t in tenants
        if str(t.get("seed_profile", {}).get("shape", "")).startswith("runaway")
    }

    payload: dict[str, Any] = {
        "window_days": max((len(d) for d in daily.values()), default=0),
        "tenants": len(daily),
        "total_spend_usd": round(total_spend, 4),
        "flagged_tenants": len(anomalies),
        "flagged_spend_usd": round(flagged_spend, 4),
        "flagged_share_of_spend_pct": round(flagged_spend / total_spend * 100, 2) if total_spend else 0.0,
        "zscore_threshold": args.threshold,
        "anomalies": [
            {
                "tenant_id": a.tenant_id,
                "baseline_usd_per_day": round(a.baseline_usd, 4),
                "peak_usd_per_day": round(a.peak_usd, 4),
                "multiple_of_baseline": round(a.multiple_of_baseline, 2),
                "total_usd": round(a.total_usd, 4),
                "share_of_spend_pct": round(a.share_of_spend_pct, 2),
                "max_zscore": round(a.max_zscore, 2),
                "onset_day": a.onset_day,
                "anomalous_days": len(a.anomalous_days),
                "explanation": a.explain(),
            }
            for a in anomalies
        ],
        "traffic": stats,
        "detector": {
            "method": "robust z-score (median and MAD) over each tenant's own daily spend",
            "why_robust": "the runaway days are inside the series used to compute the baseline; "
                          "a mean would be dragged upwards by exactly the outliers it is meant "
                          "to find, so a large enough anomaly would hide itself",
            "ranking": "by total spend, not by z-score: a spectacular z-score on a $2 baseline "
                       "matters less than a moderate one on a $2,000 baseline",
        },
        "scoring": score(anomalies, planted),
        "note": "The detector reads only daily spend per tenant. The seed_profile that marks "
                "which tenants were generated as pathological is read once, at the end, to "
                "score the result -- never as an input.",
    }

    out = write_result(
        args.out, payload, mode=args.mode, seed=env_seed(),
        generated_by="benchmarks/attribution/run_attribution.py",
        provenance_extra={"threshold": args.threshold, "events": stats["events"]},
    )

    print(f"attribution ({args.mode}, {stats['events']:,} events) -> {out.relative_to(REPO_ROOT)}")
    print(f"  total spend       ${total_spend:,.2f} across {len(daily)} tenants")
    print(f"  flagged           {len(anomalies)} tenants = "
          f"{payload['flagged_share_of_spend_pct']:.1f}% of spend")
    for a in anomalies:
        print(f"    {a.explain()}")
    sc = payload["scoring"]
    print(f"  precision {sc['precision']:.2f}  recall {sc['recall']:.2f}")
    if sc["false_positives"]:
        print(f"    false positives: {sc['false_positives']}")
    if sc["false_negatives"]:
        print(f"    missed:          {sc['false_negatives']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
