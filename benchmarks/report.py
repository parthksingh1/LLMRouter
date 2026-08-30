#!/usr/bin/env python3
"""Print the claim-to-measurement table from the committed results.

    python benchmarks/report.py             # human-readable
    python benchmarks/report.py --markdown  # the table the README embeds

Every number in the README comes from here, and this reads only
`benchmarks/results/*.json`. Nothing in the documentation is typed by hand, so a claim cannot
drift from what the harness measured -- if a number in the README looks wrong, the fix is to
re-run the benchmark, not to edit the prose.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any, Callable

REPO_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO_ROOT))

from benchmarks.common.results import read_result  # noqa: E402

RESULTS = REPO_ROOT / "benchmarks" / "results"


class Row:
    """One measured claim."""

    def __init__(
        self,
        claim: str,
        source: str,
        extract: Callable[[dict[str, Any]], str],
        command: str,
    ) -> None:
        self.claim = claim
        self.source = source
        self.extract = extract
        self.command = command

    def measure(self) -> tuple[str, str]:
        """Return (measured value, provenance) or an explanation of what is missing."""
        try:
            data = read_result(RESULTS / self.source)
        except FileNotFoundError:
            return "not yet measured", f"run: {self.command}"
        except (KeyError, ValueError) as exc:  # pragma: no cover - corrupt results file
            return "unreadable", str(exc)

        prov = data.get("provenance", {})
        try:
            value = self.extract(data)
        except (KeyError, TypeError, IndexError) as exc:
            return "missing field", f"{self.source}: {exc}"

        mode = prov.get("mode", "?")
        sha = prov.get("git_sha", "?")
        return value, f"{mode} @ {sha}"


ROWS: list[Row] = [
    Row(
        "Cost reduction from policy routing",
        "eval.json",
        lambda d: f"{d['cost']['reduction_pct']:.1f}%",
        "make bench-eval",
    ),
    Row(
        "Quality drift that reduction cost",
        "eval.json",
        lambda d: f"{d['quality']['drift_pp']:.1f} pp "
                  f"({d['quality']['baseline_accuracy_pct']:.1f}% -> {d['quality']['routed_accuracy_pct']:.1f}%)",
        "make bench-eval",
    ),
    Row(
        "Difficulty classifier accuracy",
        "eval.json",
        lambda d: f"{d['classifier']['accuracy_pct']:.1f}%",
        "make bench-eval",
    ),
    Row(
        "Cache hit rate on seeded traffic",
        "cache_bench.json",
        lambda d: f"{d['hit_rate_pct']:.1f}%",
        "make bench-cache",
    ),
    Row(
        "Cache false-hit rate",
        "cache_bench.json",
        lambda d: f"{d['false_hit_rate_pct']:.2f}% of {d.get('near_miss_opportunities', 0):,} near misses",
        "make bench-cache",
    ),
    Row(
        "Latency, uncached -> cached (p50)",
        "cache_bench.json",
        lambda d: f"{d['latency_ms']['uncached']['p50']:,.0f} ms -> {d['latency_ms']['cached']['p50']:.1f} ms",
        "make bench-cache",
    ),
    Row(
        "Calibrated similarity threshold",
        "cache_calibration.json",
        lambda d: f"{d['operating_point']['threshold']:.4f} "
                  f"({d['embedder']}, AUC {d['similarity_auc']:.3f})",
        "make bench-cache",
    ),
    Row(
        "Guardrails p99 (budget 12 ms)",
        "guardrails_latency.json",
        lambda d: f"{d['in_process_ms']['p99']:.2f} ms"
                  + ("" if d["within_budget"] else "  OVER BUDGET"),
        "make bench-guardrails",
    ),
    Row(
        "Stream completion at a 5% mid-stream failure rate",
        "failover.json",
        lambda d: f"{d['completion_rate_pct']:.2f}% of {d['streams']:,} streams",
        "make bench-failover",
    ),
    Row(
        "Streams recovered by failover",
        "failover.json",
        lambda d: f"{d['streams_with_failover']:,} "
                  f"({d['failover_rate_pct']:.1f}%), {d['restarted_streams']} restarted",
        "make bench-failover",
    ),
    Row(
        "Runaway workloads found from spend alone",
        "attribution.json",
        lambda d: f"{d['flagged_tenants']} tenants = {d['flagged_share_of_spend_pct']:.1f}% of spend "
                  f"(precision {d['scoring']['precision']:.2f}, recall {d['scoring']['recall']:.2f})",
        "make bench-attribution",
    ),
]


def render_text() -> str:
    lines = ["", "LLMRouter — measured results", "=" * 78, ""]
    width = max(len(r.claim) for r in ROWS)

    for row in ROWS:
        value, provenance = row.measure()
        lines.append(f"  {row.claim.ljust(width)}  {value}")
        lines.append(f"  {' ' * width}  ({provenance})")
    lines += [
        "",
        "Every figure is read from benchmarks/results/*.json. Nothing is typed by hand.",
        "Regenerate with: make bench",
        "",
    ]
    return "\n".join(lines)


def render_markdown() -> str:
    lines = [
        "| What | Measured | Produced by |",
        "|---|---|---|",
    ]
    for row in ROWS:
        value, provenance = row.measure()
        lines.append(f"| {row.claim} | **{value}** | `{row.command}` <br><sub>{provenance}</sub> |")
    lines += [
        "",
        "<sub>Rendered by `benchmarks/report.py` from `benchmarks/results/*.json`. "
        "No number in this table is typed by hand.</sub>",
    ]
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--markdown", action="store_true", help="emit the README table")
    args = parser.parse_args()

    print(render_markdown() if args.markdown else render_text())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
