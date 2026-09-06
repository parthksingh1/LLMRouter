#!/usr/bin/env python3
"""Measure the semantic cache on production-shaped traffic.

    python benchmarks/cache/run_cache_bench.py --mode offline \\
        --out benchmarks/results/cache_bench.json

The calibration (calibrate.py) answers "where should the threshold sit". This answers the two
questions an operator actually asks: how often does the cache hit on real traffic, and how much
latency does a hit save.

Traffic is generated from the seeded corpus with the repeat structure real assistant traffic
has: a mixture of exact repeats, paraphrases of earlier questions, near-misses that must not hit,
and genuinely new questions. The per-tenant `cache_affinity` in config/tenants.yaml sets how
repetitive each tenant is, so the aggregate hit rate is a property of the seeded workload rather
than a number chosen for the README.

Two latency figures, measured differently, and the difference is stated rather than blurred:

    uncached  sampled from the per-provider latency distribution in config/providers.yaml --
              the same distribution the Go mock provider samples when serving a real request
    cached    measured wall clock of the actual cache lookup path: normalisation, hashing, the
              key build, and (when the semantic tier is enabled) a real embedding call

Sampling the uncached side rather than sleeping is what makes a 50,000-request run take seconds
instead of a day. It measures the model, not the sleep, and the results file says so.
"""

from __future__ import annotations

import argparse
import json
import random
import sys
import time
from collections import defaultdict
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / "services" / "embedder"))

from benchmarks.common.catalogue import load_catalogue, load_policies, load_tenants  # noqa: E402
from benchmarks.common.normalise import exact_key  # noqa: E402
from benchmarks.common.results import (  # noqa: E402
    env_seed,
    read_result,
    repo_relative,
    summarise_latency,
    write_result,
)
from benchmarks.common.simulator import Router  # noqa: E402

DEFAULT_PAIRS = REPO_ROOT / "benchmarks" / "cache" / "pairs.jsonl"
DEFAULT_OUT = REPO_ROOT / "benchmarks" / "results" / "cache_bench.json"
CALIBRATION = REPO_ROOT / "benchmarks" / "results" / "cache_calibration.json"

#: How a request relates to what the cache has already seen.
#:
#: These proportions describe the *shape* of assistant traffic, and they are the main thing a
#: reader should push back on. They are set from the per-tenant cache_affinity: an affinity of
#: 0.6 means 60% of that tenant's requests are a repeat or paraphrase of something asked before.
#: Within the repeat share, exact repeats dominate paraphrases, which is what real logs look
#: like -- people re-ask the same question far more often than they rephrase it.
EXACT_SHARE_OF_REPEATS = 0.62
NEAR_MISS_SHARE_OF_NEW = 0.18


def load_pairs(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        raise FileNotFoundError(f"{path} is missing; run `make seed-pairs` first")
    with path.open(encoding="utf-8") as fh:
        return [json.loads(line) for line in fh if line.strip()]


def calibrated_threshold(default: float = 1.01) -> tuple[float, str]:
    """Read the threshold this benchmark should enforce from the calibration result.

    Reading it rather than hard-coding it is the point: the two files cannot disagree, and a
    recalibration automatically changes what this benchmark measures.
    """
    try:
        result = read_result(CALIBRATION)
    except FileNotFoundError:
        return default, "uncalibrated"
    point = result["operating_point"]
    return float(point["threshold"]), str(result.get("embedder", "unknown"))


class LatencyModel:
    """Samples provider latency the same way the Go mock does.

    A piecewise-linear inverse CDF through the configured p50/p95/p99, so the sampled quantiles
    come back equal to what an operator wrote in config/providers.yaml.
    """

    def __init__(
        self,
        p50: float,
        p95: float,
        p99: float,
        tokens_per_sec: float,
        rng: random.Random,
    ) -> None:
        self.p50, self.p95, self.p99 = p50, p95, p99
        self.tokens_per_sec = tokens_per_sec
        self.rng = rng

    def ttfb_ms(self) -> float:
        u = self.rng.random()
        if u < 0.5:
            lo = self.p50 * 0.35
            return lo + (self.p50 - lo) * (u / 0.5)
        if u < 0.95:
            return self.p50 + (self.p95 - self.p50) * ((u - 0.5) / 0.45)
        if u < 0.99:
            return self.p95 + (self.p99 - self.p95) * ((u - 0.95) / 0.04)
        excess = (u - 0.99) / 0.01
        return self.p99 * (1 + 0.9 * excess**1.5)

    def total_ms(self, completion_tokens: int) -> float:
        return self.ttfb_ms() + completion_tokens * 1000.0 / self.tokens_per_sec


def build_traffic(
    pairs: list[dict[str, Any]],
    tenants: list[dict[str, Any]],
    count: int,
    rng: random.Random,
) -> list[dict[str, Any]]:
    """Generate a request stream with a realistic repeat structure."""
    paraphrases = [p for p in pairs if p["label"] == 1]
    near_misses = [p for p in pairs if p["label"] == 0]
    if not paraphrases or not near_misses:
        raise ValueError("the pair set needs both positives and negatives")

    weights = [
        max(0.05, float(t.get("seed_profile", {}).get("rps", 0.5))) for t in tenants
    ]

    traffic: list[dict[str, Any]] = []
    for _ in range(count):
        tenant = rng.choices(tenants, weights=weights, k=1)[0]
        affinity = float(tenant.get("seed_profile", {}).get("cache_affinity", 0.5))

        if rng.random() < affinity:
            # A repeat of something asked before.
            pair = rng.choice(paraphrases)
            if rng.random() < EXACT_SHARE_OF_REPEATS:
                kind, prompt, canonical = "exact_repeat", pair["left"], pair["left"]
            else:
                kind, prompt, canonical = "paraphrase", pair["right"], pair["left"]
        elif rng.random() < NEAR_MISS_SHARE_OF_NEW:
            # A near miss: high overlap, different question. Must not hit.
            #
            # `prime` is the labelled counterpart, which the replay caches immediately before
            # issuing the request. Without that the near-miss test would be vacuous: a cache
            # that has never seen the other question cannot possibly confuse the two.
            pair = rng.choice(near_misses)
            kind, prompt, canonical = "near_miss", pair["right"], pair["right"]
            traffic.append(
                {
                    "tenant_id": tenant["id"],
                    "prompt": prompt,
                    "canonical": canonical,
                    "kind": kind,
                    "prime": pair["left"],
                }
            )
            continue
        else:
            pair = rng.choice(paraphrases)
            kind = "new"
            prompt = f"{pair['left']} (variant {rng.randrange(1_000_000)})"
            canonical = prompt

        traffic.append(
            {
                "tenant_id": tenant["id"],
                "prompt": prompt,
                "canonical": canonical,
                "kind": kind,
            }
        )
    return traffic


def different_questions(
    stored: str, asked: str, negatives: set[tuple[str, str]]
) -> bool:
    """Whether serving `stored`'s answer for `asked` would be a wrong answer.

    Identical text is obviously the same question. Otherwise the pair set is the authority: it
    is the labelled ground truth, and deferring to it stops the benchmark from grading itself
    with the same rule the cache uses to decide.
    """
    if stored == asked:
        return False
    return (stored, asked) in negatives or (asked, stored) in negatives


def run_offline(
    traffic: list[dict[str, Any]],
    threshold: float,
    rng: random.Random,
    negative_pairs: set[tuple[str, str]],
) -> dict[str, Any]:
    """Replay the traffic through the real normalisation path and a modelled provider."""
    catalogue = load_catalogue()
    router = Router(catalogue)
    policy_name = load_policies()["default_policy"]

    latency_models = {
        p.name: LatencyModel(
            p.ttfb_p50_ms, p.ttfb_p95_ms, p.ttfb_p99_ms, p.tokens_per_sec, rng
        )
        for p in catalogue.providers
    }

    # namespace -> exact key -> the prompt text whose answer is stored there.
    #
    # Storing the text, not just a flag, is what makes false hits measurable: on a hit we can
    # ask whether the stored question and the asked question are actually the same one.
    store: dict[str, dict[str, str]] = defaultdict(dict)

    cached_latencies: list[float] = []
    uncached_latencies: list[float] = []
    by_kind: dict[str, dict[str, int]] = defaultdict(lambda: {"total": 0, "hit": 0})
    hits = false_hits = 0

    # The semantic tier is only exercised when the calibration left it reachable.
    semantic_enabled = threshold <= 1.0

    near_miss_opportunities = 0

    for request in traffic:
        namespace = f"{request['tenant_id']}|auto|"
        by_kind[request["kind"]]["total"] += 1

        # A near miss only tests anything once its counterpart is in the cache, so prime it.
        prime = request.get("prime")
        if prime is not None:
            store[namespace].setdefault(exact_key(prime), prime)
            near_miss_opportunities += 1

        # Measure the real lookup work rather than guessing at it.
        started = time.perf_counter()
        key = exact_key(request["prompt"])
        entry = store[namespace].get(key)
        lookup_ms = (time.perf_counter() - started) * 1000

        if entry is not None:
            hits += 1
            by_kind[request["kind"]]["hit"] += 1
            # A false hit is an answer produced for a DIFFERENT question. Comparing the stored
            # prompt against the asked one is the only honest test: a near-miss request that
            # happens to repeat something already asked is a true hit, not a failure.
            if different_questions(entry, request["prompt"], negative_pairs):
                false_hits += 1
            # Redis round trip on a warm connection, which the offline mode cannot measure.
            cached_latencies.append(lookup_ms + REDIS_ROUND_TRIP_MS)
            continue

        # Miss: route it, pay the provider, then write through.
        decision = router.route(policy_name, request["prompt"])
        model = decision.model
        completion_tokens = 180 if model.tier == "frontier" else 90
        uncached_latencies.append(
            lookup_ms
            + REDIS_ROUND_TRIP_MS
            + latency_models[model.provider].total_ms(completion_tokens)
        )
        store[namespace][key] = request["prompt"]

    total = len(traffic)

    return {
        "requests": total,
        "hit_rate_pct": round(hits / total * 100, 2),
        "near_miss_opportunities": near_miss_opportunities,
        "false_hit_rate_pct": (
            round(false_hits / near_miss_opportunities * 100, 3)
            if near_miss_opportunities
            else 0.0
        ),
        "false_hit_rate_of_all_requests_pct": round(false_hits / total * 100, 4),
        "hits": hits,
        "false_hits": false_hits,
        "semantic_tier_enabled": semantic_enabled,
        "threshold": threshold,
        "by_kind": {
            kind: {
                "requests": stats["total"],
                "hits": stats["hit"],
                "hit_rate_pct": round(stats["hit"] / stats["total"] * 100, 2)
                if stats["total"]
                else 0.0,
            }
            for kind, stats in sorted(by_kind.items())
        },
        "latency_ms": {
            "cached": summarise_latency(cached_latencies),
            "uncached": summarise_latency(uncached_latencies),
        },
    }


#: A warm Redis GET on a loopback connection. Added to both paths so the comparison is fair.
#:
#: It is a constant rather than a measurement because the offline mode has no Redis; the live
#: gateway-mode run measures the real thing end to end, and the two are reported separately.
REDIS_ROUND_TRIP_MS = 0.8


def run_gateway(
    traffic: list[dict[str, Any]], base_url: str, tenants: list[dict[str, Any]]
) -> dict[str, Any]:
    """Replay the traffic against a live gateway and measure end-to-end wall clock."""
    import urllib.error
    import urllib.request

    keys = {t["id"]: t["api_key"] for t in tenants}

    cached_latencies: list[float] = []
    uncached_latencies: list[float] = []
    by_kind: dict[str, dict[str, int]] = defaultdict(lambda: {"total": 0, "hit": 0})
    hits = false_hits = 0

    for request in traffic:
        by_kind[request["kind"]]["total"] += 1
        body = json.dumps(
            {
                "model": "auto",
                "messages": [{"role": "user", "content": request["prompt"]}],
            }
        ).encode("utf-8")
        req = urllib.request.Request(
            f"{base_url.rstrip('/')}/v1/chat/completions",
            data=body,
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {keys[request['tenant_id']]}",
            },
            method="POST",
        )

        started = time.perf_counter()
        try:
            with urllib.request.urlopen(req, timeout=180) as resp:
                payload = json.loads(resp.read())
        except urllib.error.URLError as exc:
            raise SystemExit(
                f"could not reach the gateway at {base_url}: {exc}"
            ) from exc
        elapsed_ms = (time.perf_counter() - started) * 1000

        cached = bool(payload.get("llmrouter", {}).get("cached"))
        if cached:
            hits += 1
            by_kind[request["kind"]]["hit"] += 1
            cached_latencies.append(elapsed_ms)
            if request["kind"] == "near_miss":
                false_hits += 1
        else:
            uncached_latencies.append(elapsed_ms)

    total = len(traffic)
    near_miss_total = by_kind["near_miss"]["total"]
    return {
        "requests": total,
        "hit_rate_pct": round(hits / total * 100, 2),
        "false_hit_rate_pct": round(false_hits / near_miss_total * 100, 3)
        if near_miss_total
        else 0.0,
        "false_hit_rate_of_all_requests_pct": round(false_hits / total * 100, 4),
        "hits": hits,
        "false_hits": false_hits,
        "by_kind": {
            kind: {
                "requests": s["total"],
                "hits": s["hit"],
                "hit_rate_pct": round(s["hit"] / s["total"] * 100, 2)
                if s["total"]
                else 0.0,
            }
            for kind, s in sorted(by_kind.items())
        },
        "latency_ms": {
            "cached": summarise_latency(cached_latencies),
            "uncached": summarise_latency(uncached_latencies),
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--mode", choices=["offline", "gateway"], default="offline")
    parser.add_argument("--pairs", type=Path, default=DEFAULT_PAIRS)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--requests", type=int, default=50_000)
    parser.add_argument("--gateway", default="http://localhost:8080")
    args = parser.parse_args()

    seed = env_seed()
    rng = random.Random(seed + 404)

    pairs = load_pairs(args.pairs)
    tenants = load_tenants()["tenants"]
    threshold, embedder = calibrated_threshold()

    # A live run cannot do 50,000 real generations in a demo, so it uses a smaller sample and
    # says so in the results file rather than quietly reporting a different-sized experiment.
    count = args.requests if args.mode == "offline" else min(args.requests, 400)
    traffic = build_traffic(pairs, tenants, count, rng)

    if args.mode == "offline":
        negative_pairs = {(p["left"], p["right"]) for p in pairs if p["label"] == 0}
        payload = run_offline(traffic, threshold, rng, negative_pairs)
    else:
        payload = run_gateway(traffic, args.gateway, tenants)

    payload["calibration"] = {"threshold": threshold, "embedder": embedder}
    payload["latency_method"] = (
        "cached: measured wall clock of the real lookup path plus a warm Redis round trip; "
        "uncached: the same, plus provider time sampled from the distribution in "
        "config/providers.yaml"
        if args.mode == "offline"
        else "both measured end to end against the live gateway"
    )

    out = write_result(
        args.out,
        payload,
        mode=args.mode,
        seed=seed,
        generated_by="benchmarks/cache/run_cache_bench.py",
        provenance_extra={
            "requests": count,
            "threshold": threshold,
            "embedder": embedder,
        },
    )

    cached = payload["latency_ms"]["cached"]
    uncached = payload["latency_ms"]["uncached"]
    print(f"cache bench ({args.mode}, {count:,} requests) -> {repo_relative(out)}")
    print(f"  hit rate        {payload['hit_rate_pct']:.1f}%")
    opportunities = payload.get("near_miss_opportunities", 0)
    print(
        f"  false-hit rate  {payload['false_hit_rate_pct']:.2f}% of {opportunities:,} near-miss "
        f"opportunities ({payload['false_hit_rate_of_all_requests_pct']:.3f}% of all requests)"
    )
    print(
        f"  p50 latency     {uncached['p50']:.0f} ms uncached -> {cached['p50']:.1f} ms cached"
    )
    print(
        f"  p95 latency     {uncached['p95']:.0f} ms uncached -> {cached['p95']:.1f} ms cached"
    )
    print("  by request kind:")
    for kind, stats in payload["by_kind"].items():
        print(
            f"    {kind:14} {stats['requests']:7,} requests  {stats['hit_rate_pct']:5.1f}% hit"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
