#!/usr/bin/env python3
"""Generate ~30 days of synthetic request history.

    python seed/generate_traffic.py --days 30 --target 200000 --out seed/out/events.tsv

The output is a TSV ready for ClickHouse's native insert, not JSON: 200,000 rows of JSON is
~90 MB and takes minutes to parse, while the same data as TSV is ~20 MB and loads in seconds.
It is written to seed/out/, which is gitignored -- generated data is never committed, only the
generator is.

The traffic has structure, because a uniform random stream would make every dashboard flat and
the anomaly detector meaningless:

  - Diurnal and weekly shape. Business-hours tenants have a working-day peak and a weekend
    trough; steady tenants do not.
  - Per-tenant cache affinity, from config/tenants.yaml, which drives the cache hit rate.
  - Per-tenant routing policy, so the model mix differs by tenant the way it would in reality.
  - Three tenants whose behaviour turns pathological partway through the month.

The three runaway workloads are the point of the exercise. Nothing marks them as anomalous in
the data -- no flag, no tag. They are three tenants whose spend happens to be wrong, and the
detector in benchmarks/attribution/run_attribution.py has to find them from the event stream the
same way an on-call engineer would.
"""

from __future__ import annotations

import argparse
import math
import random
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any, TextIO

REPO_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / "seed"))

from benchmarks.common.catalogue import load_catalogue, load_policies, load_tenants  # noqa: E402
from benchmarks.common.simulator import Router  # noqa: E402
from corpus import (  # noqa: E402
    EASY_TEMPLATES,
    HARD_TEMPLATES,
    MEDIUM_TEMPLATES,
    estimate_tokens,
    render,
)

ALL_TEMPLATES = EASY_TEMPLATES + MEDIUM_TEMPLATES + HARD_TEMPLATES

#: Column order for the TSV. Must match the INSERT in seed_clickhouse.sh.
COLUMNS = [
    "ts", "request_id", "tenant_id", "policy", "model", "provider",
    "prompt_tokens", "completion_tokens", "cached", "cache_similarity",
    "guardrail_blocked", "guardrail_findings", "failover_count",
    "status_code", "latency_ms", "cost_usd",
]


def diurnal_weight(when: datetime, shape: str) -> float:
    """How busy a tenant is at a given moment, relative to its average.

    A flat traffic profile makes every chart a straight line and hides exactly the kind of
    change an operator needs to see, so the shapes here are deliberate.
    """
    hour = when.hour + when.minute / 60
    weekday = when.weekday()  # 0 = Monday

    if shape == "business_hours":
        if weekday >= 5:
            return 0.12
        # A working day with a lunch dip, which is what real internal-tool traffic looks like.
        peak = math.exp(-((hour - 10.5) ** 2) / 8) + math.exp(-((hour - 15.0) ** 2) / 8)
        return 0.08 + 1.6 * peak

    if shape == "bursty":
        # Long quiet periods punctuated by short spikes: batch jobs and scheduled runs.
        return 2.6 if (int(hour) % 6 == 0 and when.minute < 20) else 0.25

    if shape.startswith("runaway"):
        # Runaways do not sleep. That alone is a signal, but not the one the detector uses.
        return 1.0

    # steady: a gentle day/night curve, never zero.
    return 0.7 + 0.5 * math.sin((hour - 6) / 24 * 2 * math.pi)


def runaway_multiplier(profile: dict[str, Any], day_index: int) -> float:
    """How much a runaway tenant's per-request cost is inflated, by day.

    Each pathology ramps rather than stepping, because a step is trivially detectable by
    eyeballing a chart and a ramp is what actually happens: a bad deploy goes out, adoption
    grows, and the bill arrives three weeks later.
    """
    shape = profile.get("shape", "steady")
    onset = int(profile.get("onset_day", 999))
    if not shape.startswith("runaway") or day_index < onset:
        return 1.0

    days_since = day_index - onset
    ramp = min(1.0, days_since / 4)

    if shape == "runaway_retry_storm":
        # A broken retry loop: the same request three to nine times over.
        return 1.0 + 8.0 * ramp
    if shape == "runaway_no_cache":
        # A UUID in every prompt defeats the cache entirely, so every request pays full price.
        return 1.0 + 5.0 * ramp
    if shape == "runaway_context_bloat":
        # Whole documents stuffed into every call: the prompt-token count explodes.
        return 1.0 + 14.0 * ramp
    return 1.0


def build_row(
    when: datetime,
    tenant: dict[str, Any],
    router: Router,
    rng: random.Random,
    day_index: int,
    request_index: int,
) -> dict[str, Any]:
    """Produce one event."""
    profile = tenant.get("seed_profile", {})
    shape = profile.get("shape", "steady")
    policy = tenant.get("default_policy", "quality_tiered")

    template = rng.choice(ALL_TEMPLATES)
    prompt = render(template, rng)

    multiplier = runaway_multiplier(profile, day_index)

    # --- cache ------------------------------------------------------------------------
    affinity = float(profile.get("cache_affinity", 0.5))
    if shape == "runaway_no_cache" and multiplier > 1.0:
        affinity = 0.0
    cached = rng.random() < affinity

    # --- routing ----------------------------------------------------------------------
    # Routed by the same simulator the eval uses, so the model mix in the dashboard is the mix
    # the configured policies actually produce rather than an invented distribution.
    try:
        decision = router.route(policy, prompt)
        model = decision.model
    except NotImplementedError:
        # weighted_round_robin and canary are stateful; approximate them with the default.
        decision = router.route("quality_tiered", prompt)
        model = decision.model

    # --- tokens -----------------------------------------------------------------------
    prompt_tokens = estimate_tokens(prompt) + rng.randint(20, 200)
    if shape == "runaway_context_bloat" and multiplier > 1.0:
        # 30k-40k tokens of stuffed context on every call.
        prompt_tokens = int(rng.uniform(30_000, 40_000) * min(1.0, multiplier / 15))
    completion_tokens = 0 if cached else rng.randint(60, 420)

    # --- guardrails --------------------------------------------------------------------
    findings = 0
    blocked = 0
    if rng.random() < 0.031:
        findings = rng.randint(1, 3)
        if rng.random() < 0.28:
            blocked = 1

    # --- failover ----------------------------------------------------------------------
    failovers = 0
    if not cached and rng.random() < 0.006:
        failovers = 1

    # --- latency -----------------------------------------------------------------------
    if cached:
        latency = rng.randint(3, 40)
    else:
        provider = router.catalogue.provider(model.provider)
        latency = int(
            rng.gauss(provider.ttfb_p50_ms, provider.ttfb_p50_ms * 0.35)
            + completion_tokens * 1000 / provider.tokens_per_sec
        )
        latency = max(50, latency)
        if failovers:
            latency = int(latency * 1.7)

    # --- cost --------------------------------------------------------------------------
    #
    # A cache hit is billed at zero but the counterfactual is recorded, because "what did the
    # cache save" is unanswerable otherwise. The materialised view sums cost_usd only where
    # cached = 1 to produce the saving.
    cost = model.cost_usd(prompt_tokens, completion_tokens if not cached else rng.randint(60, 420))
    if shape == "runaway_retry_storm" and multiplier > 1.0:
        cost *= multiplier
        prompt_tokens = int(prompt_tokens * multiplier)
    elif multiplier > 1.0 and shape != "runaway_context_bloat":
        cost *= multiplier

    status = 400 if blocked else 200

    return {
        "ts": when.strftime("%Y-%m-%d %H:%M:%S.") + f"{when.microsecond // 1000:03d}",
        "request_id": f"req-{day_index:02d}-{request_index:07d}",
        "tenant_id": tenant["id"],
        "policy": policy,
        "model": model.id,
        "provider": model.provider,
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "cached": int(cached),
        "cache_similarity": round(rng.uniform(0.93, 1.0), 4) if cached else 0.0,
        "guardrail_blocked": blocked,
        "guardrail_findings": findings,
        "failover_count": failovers,
        "status_code": status,
        "latency_ms": latency,
        "cost_usd": round(cost, 8),
    }


def generate(days: int, target: int, seed: int, out: TextIO) -> dict[str, Any]:
    """Write the traffic and return a summary."""
    rng = random.Random(seed + 909)
    tenants = load_tenants()["tenants"]
    router = Router(load_catalogue())
    _ = load_policies()

    end = datetime.now(UTC).replace(hour=0, minute=0, second=0, microsecond=0)
    start = end - timedelta(days=days)

    # Allocate the request budget across tenants by their configured rate, then across days by
    # the diurnal shape. This is what makes the totals land near `target` without a second pass.
    weights = [max(0.05, float(t.get("seed_profile", {}).get("rps", 0.5))) for t in tenants]
    total_weight = sum(weights)

    written = 0
    per_tenant: dict[str, int] = {}
    per_tenant_cost: dict[str, float] = {}

    for tenant, weight in zip(tenants, weights, strict=True):
        profile = tenant.get("seed_profile", {})
        shape = profile.get("shape", "steady")
        tenant_budget = int(target * weight / total_weight)
        per_tenant[tenant["id"]] = 0
        per_tenant_cost[tenant["id"]] = 0.0

        # Day weights include the runaway ramp, so a runaway tenant sends more requests as well
        # as more expensive ones -- which is what a retry storm actually looks like.
        day_weights = []
        for day_index in range(days):
            base = 1.0
            if shape.startswith("runaway"):
                base = runaway_multiplier(profile, day_index) ** 0.5
            day_weights.append(base)
        day_total = sum(day_weights)

        for day_index in range(days):
            day_start = start + timedelta(days=day_index)
            day_budget = int(tenant_budget * day_weights[day_index] / day_total)
            if day_budget <= 0:
                continue

            # Sample timestamps within the day according to the diurnal shape.
            hour_weights = [diurnal_weight(day_start + timedelta(hours=h), shape) for h in range(24)]
            hour_total = sum(hour_weights) or 1.0

            # Fractional carry across hours.
            #
            # int()-truncating each hour independently silently deletes the whole day for a
            # low-volume tenant: ten requests spread over 24 hours truncates to zero every
            # time. That left the quiet tenants with no early history at all, which is exactly
            # the pre-onset baseline the anomaly detector needs, so a runaway with a low request
            # rate became undetectable. Carrying the remainder forward preserves the total.
            carry = 0.0
            for hour in range(24):
                exact = day_budget * hour_weights[hour] / hour_total + carry
                count = int(exact)
                carry = exact - count
                for i in range(count):
                    when = day_start + timedelta(
                        hours=hour, minutes=rng.randint(0, 59),
                        seconds=rng.randint(0, 59), milliseconds=rng.randint(0, 999),
                    )
                    row = build_row(when, tenant, router, rng, day_index, written)
                    out.write("\t".join(str(row[c]) for c in COLUMNS) + "\n")
                    written += 1
                    per_tenant[tenant["id"]] += 1
                    # Billed spend only. A cached row carries its counterfactual cost so the
                    # cache saving is measurable, but the tenant was never charged for it, and
                    # summing it here would overstate every cache-friendly tenant's bill.
                    if not row["cached"]:
                        per_tenant_cost[tenant["id"]] += row["cost_usd"]
                    _ = i

    total_cost = sum(per_tenant_cost.values())
    runaways = [
        t["id"] for t in tenants
        if str(t.get("seed_profile", {}).get("shape", "")).startswith("runaway")
    ]
    runaway_cost = sum(per_tenant_cost[t] for t in runaways)

    return {
        "events": written,
        "days": days,
        "tenants": len(tenants),
        "total_cost_usd": round(total_cost, 2),
        "per_tenant_cost": {k: round(v, 2) for k, v in sorted(per_tenant_cost.items(), key=lambda kv: -kv[1])},
        "runaway_tenants": runaways,
        "runaway_share_pct": round(runaway_cost / total_cost * 100, 2) if total_cost else 0.0,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--days", type=int, default=30)
    parser.add_argument("--target", type=int, default=200_000, help="approximate number of events")
    parser.add_argument("--seed", type=int, default=1337)
    parser.add_argument("--out", type=Path, default=REPO_ROOT / "seed" / "out" / "events.tsv")
    args = parser.parse_args()

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("w", encoding="utf-8", newline="\n") as fh:
        summary = generate(args.days, args.target, args.seed, fh)

    size_mb = args.out.stat().st_size / 1024 / 1024
    print(f"wrote {summary['events']:,} events to {args.out} ({size_mb:.1f} MB)")
    print(f"  days            {summary['days']}")
    print(f"  billed spend    ${summary['total_cost_usd']:,.2f}  (cache hits excluded)")
    print(f"  runaway tenants {summary['runaway_tenants']}")
    print(f"  runaway share   {summary['runaway_share_pct']:.1f}% of the month's spend")
    print("  top spenders:")
    for tenant, cost in list(summary["per_tenant_cost"].items())[:6]:
        share = cost / summary["total_cost_usd"] * 100 if summary["total_cost_usd"] else 0
        print(f"    {tenant:12} ${cost:10,.2f}  {share:5.1f}%")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
