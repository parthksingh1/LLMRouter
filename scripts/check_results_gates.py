"""Compare freshly-run benchmark results against the committed ones, and assert the gates.

Run after `make bench`:

    python scripts/check_results_gates.py

This replaces a plain `git diff --exit-code -- benchmarks/results`, which cannot work: every
results file carries a provenance block stamped with the wall-clock time, the git SHA, the
platform and the Python version, so a byte comparison fails on a clean tree every time. Worse,
it would fail for the wrong reason -- drowning a genuine change in a number underneath four
fields that were always going to differ.

So the comparison is made field by field, in three classes:

  measured     wall-clock timings. Compared against the committed value only loosely, because
               a GitHub runner is not the machine the numbers were committed from, and a p99
               that moves by a millisecond between machines is not a regression. The absolute
               gates below are what actually constrain these.
  provenance   ignored entirely; it is metadata about the run, not a result of it.
  everything   compared exactly. Every remaining number is a deterministic function of the
               else         seeded input, so any difference is a real change and must be
                            committed alongside the change that caused it.

Then the gates: absolute floors and ceilings that hold regardless of the machine. These are
deliberately set with headroom below the committed measurements -- they exist to catch a
regression that breaks a claim in the README, not to pin a number to three decimal places.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[1]
RESULTS = REPO_ROOT / "benchmarks" / "results"

#: Key names whose subtree is a wall-clock measurement of the machine that ran it.
MEASURED_KEYS = frozenset(
    {"latency_ms", "http_ms", "in_process_ms", "wall_clock_seconds", "detectors"}
)

#: How far a measured value may drift from the committed one before it is worth a look. Wide on
#: purpose: this catches an order-of-magnitude regression, not runner noise.
MEASURED_TOLERANCE = 10.0

#: Absolute gates. Each is (dotted path, comparison, bound, why it matters).
GATES: tuple[tuple[str, str, str, float, str], ...] = (
    ("eval", "cost.reduction_pct", ">=", 45.0, "routing must still pay for itself"),
    ("eval", "quality.drift_pp", "<=", 3.0, "cheap routing must not cost real quality"),
    (
        "eval",
        "classifier.accuracy_pct",
        ">=",
        70.0,
        "the difficulty classifier must beat guessing",
    ),
    (
        "cache_bench",
        "false_hits",
        "==",
        0.0,
        "a wrong cached answer is worse than no cache",
    ),
    ("cache_bench", "hit_rate_pct", ">=", 50.0, "the cache must earn its dependencies"),
    (
        "guardrails_latency",
        "in_process_ms.p99",
        "<=",
        12.0,
        "screening is on every request",
    ),
    (
        "failover",
        "completion_rate_pct",
        "==",
        100.0,
        "a truncated stream is a failed request",
    ),
    (
        "attribution",
        "scoring.precision",
        "==",
        1.0,
        "a false runaway alert costs trust",
    ),
    ("attribution", "scoring.recall", "==", 1.0, "a missed runaway is the whole point"),
)

FILES = ("eval", "cache_bench", "guardrails_latency", "failover", "attribution")


def committed(name: str) -> dict[str, Any] | None:
    """The version of a results file at HEAD, or None when it is not committed yet."""
    rel = f"benchmarks/results/{name}.json"
    proc = subprocess.run(
        ["git", "show", f"HEAD:{rel}"],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        return None
    data: dict[str, Any] = json.loads(proc.stdout)
    return data


def current(name: str) -> dict[str, Any]:
    with (RESULTS / f"{name}.json").open(encoding="utf-8") as fh:
        data: dict[str, Any] = json.load(fh)
    return data


def dotted(data: dict[str, Any], path: str) -> Any:
    node: Any = data
    for part in path.split("."):
        node = node[part]
    return node


def compare(was: Any, now: Any, path: str, measured: bool, out: list[str]) -> None:
    """Walk both documents together, collecting differences into `out`."""
    if isinstance(was, dict) and isinstance(now, dict):
        for key in sorted(set(was) | set(now)):
            if path == "" and key == "provenance":
                continue
            if key not in was:
                out.append(f"{path}.{key}: added ({now[key]!r})")
            elif key not in now:
                out.append(f"{path}.{key}: removed (was {was[key]!r})")
            else:
                compare(
                    was[key],
                    now[key],
                    f"{path}.{key}" if path else key,
                    measured or key in MEASURED_KEYS,
                    out,
                )
        return

    if isinstance(was, list) and isinstance(now, list):
        if len(was) != len(now):
            out.append(f"{path}: length {len(was)} -> {len(now)}")
            return
        for i, (a, b) in enumerate(zip(was, now, strict=True)):
            compare(a, b, f"{path}[{i}]", measured, out)
        return

    if isinstance(was, bool) or isinstance(now, bool):
        if was is not now:
            out.append(f"{path}: {was} -> {now}")
        return

    if isinstance(was, (int, float)) and isinstance(now, (int, float)):
        if measured:
            # A measured value is allowed to move; only an order-of-magnitude shift is reported,
            # and the absolute gates are what hold the line.
            if was and abs(now - was) / abs(was) > MEASURED_TOLERANCE:
                out.append(
                    f"{path}: {was} -> {now} (measured, beyond {MEASURED_TOLERANCE:.0f}x)"
                )
        elif was != now:
            out.append(f"{path}: {was} -> {now}")
        return

    if was != now:
        out.append(f"{path}: {was!r} -> {now!r}")


def main() -> int:
    failures: list[str] = []

    print("comparing regenerated results against the committed ones")
    for name in FILES:
        was = committed(name)
        if was is None:
            print(f"  {name:22s} not committed yet, skipping comparison")
            continue
        diffs: list[str] = []
        compare(was, current(name), "", False, diffs)
        if diffs:
            failures.append(
                f"{name}.json changed and the new file is not committed:\n"
                + "\n".join(f"      {d}" for d in diffs)
            )
            print(f"  {name:22s} DRIFTED ({len(diffs)})")
        else:
            print(f"  {name:22s} unchanged")

    print("\nregression gates")
    for name, path, op, bound, why in GATES:
        value = float(dotted(current(name), path))
        ok = {
            ">=": value >= bound,
            "<=": value <= bound,
            "==": value == bound,
        }[op]
        print(
            f"  {'PASS' if ok else 'FAIL'}  {name}:{path} = {value:g} {op} {bound:g}  ({why})"
        )
        if not ok:
            failures.append(
                f"gate failed: {name}:{path} = {value:g}, needs {op} {bound:g} -- {why}"
            )

    if failures:
        print("\n" + "=" * 78)
        for f in failures:
            print(f"  {f}")
        print(
            "\nIf a change legitimately moves a number, re-run `make bench` and commit the new\n"
            "benchmarks/results/*.json alongside it. Never edit the JSON by hand."
        )
        return 1

    print("\nOK: results match the committed run and every gate holds.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
