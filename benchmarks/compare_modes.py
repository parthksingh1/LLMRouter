#!/usr/bin/env python3
"""Assert that offline mode and gateway mode agree.

    python benchmarks/compare_modes.py

This is the mechanism that keeps `benchmarks/common/simulator.py` honest. The simulator is a
Python reimplementation of the Go routing decision, and a simulator that drifts from the router
turns an honest benchmark into a flattering one — silently, in the direction nobody checks.

CI's integration job runs the eval in both modes and then runs this. A divergence beyond the
tolerance fails the build, which is what makes ADR-0007's duplication defensible rather than
merely convenient.

Usage in CI:

    make bench MODE=offline     # writes benchmarks/results/*.json
    cp -r benchmarks/results /tmp/offline
    make bench MODE=gateway
    python benchmarks/compare_modes.py --offline /tmp/offline
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


class Check:
    """One value that must agree across modes."""

    def __init__(self, name: str, source: str, extract: Callable[[dict[str, Any]], float], tolerance: float, unit: str) -> None:
        self.name = name
        self.source = source
        self.extract = extract
        self.tolerance = tolerance
        self.unit = unit


#: What must agree, and by how much.
#:
#: The tolerances are not tight for their own sake. Gateway mode prices the *actual* usage a
#: provider reported, while offline mode prices the seeded fixture, so the two differ by a token
#: or two per call even when the routing decision is identical. What must not differ is which
#: model was chosen, and that shows up as a large cost-saving gap long before a small one.
CHECKS: list[Check] = [
    Check("cost reduction", "eval.json",
          lambda d: d["cost"]["reduction_pct"], tolerance=2.0, unit="pp"),
    Check("quality drift", "eval.json",
          lambda d: d["quality"]["drift_pp"], tolerance=1.0, unit="pp"),
    Check("classifier accuracy", "eval.json",
          lambda d: d["classifier"]["accuracy_pct"], tolerance=1.0, unit="pp"),
]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--offline", type=Path, default=RESULTS,
                        help="directory holding the offline-mode results")
    parser.add_argument("--gateway", type=Path, default=RESULTS,
                        help="directory holding the gateway-mode results")
    args = parser.parse_args()

    failures = 0
    checked = 0

    for check in CHECKS:
        try:
            offline = read_result(args.offline / check.source)
            gateway = read_result(args.gateway / check.source)
        except FileNotFoundError as exc:
            print(f"SKIP  {check.name}: {exc}")
            continue

        offline_mode = offline.get("provenance", {}).get("mode")
        gateway_mode = gateway.get("provenance", {}).get("mode")

        if offline_mode == gateway_mode:
            print(
                f"SKIP  {check.name}: both files were produced in {offline_mode!r} mode.\n"
                f"      Run `make bench MODE=offline`, copy benchmarks/results elsewhere, then\n"
                f"      `make bench MODE=gateway` and pass --offline <copy>."
            )
            continue

        left = check.extract(offline)
        right = check.extract(gateway)
        delta = abs(left - right)
        checked += 1

        if delta <= check.tolerance:
            print(f"ok    {check.name}: offline {left:.2f} vs gateway {right:.2f} "
                  f"(delta {delta:.2f} {check.unit}, tolerance {check.tolerance})")
        else:
            failures += 1
            print(
                f"FAIL  {check.name}: offline {left:.2f} vs gateway {right:.2f} "
                f"(delta {delta:.2f} {check.unit}, tolerance {check.tolerance})\n"
                f"      The offline simulator has drifted from the Go router. Either\n"
                f"      internal/router changed without benchmarks/common/simulator.py, or\n"
                f"      the reverse. See docs/adr/0007-offline-benchmark-simulator.md.",
                file=sys.stderr,
            )

    if checked == 0:
        print("\nNothing compared. This is not a pass — see the SKIP lines above.")
        return 0

    if failures:
        print(f"\n{failures} of {checked} checks diverged.", file=sys.stderr)
        return 1

    print(f"\nAll {checked} checks agree: the offline simulator still matches the router.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
