"""Writing benchmark results.

Every file under `benchmarks/results/` is written through :func:`write_result`, which stamps a
provenance block onto it: what produced it, when, in which mode, from which seed and at which
commit. A pre-commit hook (`scripts/check_results_provenance.sh`) rejects any results file
missing those fields, which is the mechanism that stops a number in the README from being
quietly hand-edited to something more flattering.

The rule the repository follows: if a change moves a number, re-run the benchmark and commit the
new JSON alongside the change. Never edit the JSON.
"""

from __future__ import annotations

import json
import os
import platform
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

#: Benchmark execution modes.
#:
#: offline  the harness computes the result itself, using the same YAML configuration the Go
#:          gateway reads. Needs nothing but Python, so a reviewer can reproduce every number
#:          on a fresh clone without Docker.
#: gateway  the harness drives the live stack over HTTP. This is the real thing, and it is what
#:          CI runs on the integration job.
MODES = ("offline", "gateway")

REPO_ROOT = Path(__file__).resolve().parents[2]


def git_sha() -> str:
    """Return the current commit, or a marker when git is unavailable or the tree is dirty."""
    try:
        sha = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=REPO_ROOT,
            capture_output=True,
            text=True,
            check=True,
            timeout=10,
        ).stdout.strip()
    except (subprocess.SubprocessError, OSError, FileNotFoundError):
        return "unknown"

    try:
        dirty = subprocess.run(
            ["git", "status", "--porcelain"],
            cwd=REPO_ROOT,
            capture_output=True,
            text=True,
            check=True,
            timeout=10,
        ).stdout.strip()
    except (subprocess.SubprocessError, OSError, FileNotFoundError):
        return sha

    # A dirty tree is flagged rather than hidden: a number measured against uncommitted code is
    # not reproducible from the repository, and the reader deserves to know that.
    return f"{sha}-dirty" if dirty else sha


def provenance(
    mode: str, seed: int, generated_by: str, extra: dict[str, Any] | None = None
) -> dict[str, Any]:
    """Build the provenance block."""
    if mode not in MODES:
        raise ValueError(f"mode must be one of {MODES}, got {mode!r}")

    block: dict[str, Any] = {
        "generated_by": generated_by,
        "generated_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat(),
        "mode": mode,
        "seed": seed,
        "git_sha": git_sha(),
        "python": platform.python_version(),
        "platform": f"{platform.system().lower()}-{platform.machine().lower()}",
    }
    if extra:
        block.update(extra)
    return block


def write_result(
    path: Path | str,
    payload: dict[str, Any],
    *,
    mode: str,
    seed: int,
    generated_by: str,
    provenance_extra: dict[str, Any] | None = None,
) -> Path:
    """Write a results file with its provenance block, and return the path."""
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)

    document = {"provenance": provenance(mode, seed, generated_by, provenance_extra)}
    document.update(payload)

    with out.open("w", encoding="utf-8", newline="\n") as fh:
        json.dump(document, fh, indent=2, sort_keys=True)
        fh.write("\n")
    return out


def read_result(path: Path | str) -> dict[str, Any]:
    """Read a results file, raising a helpful error when it has not been generated yet."""
    p = Path(path)
    if not p.exists():
        raise FileNotFoundError(
            f"{p} does not exist yet. Run `make bench` (or the specific bench target) to "
            f"produce it; results are never committed by hand."
        )
    with p.open(encoding="utf-8") as fh:
        data: dict[str, Any] = json.load(fh)
    return data


def env_seed(default: int = 1337) -> int:
    """Read SEED from the environment, so every harness pins the same stream."""
    raw = os.getenv("SEED")
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        print(
            f"warning: SEED={raw!r} is not an integer; falling back to {default}",
            file=sys.stderr,
        )
        return default


def percentile(values: list[float], q: float) -> float:
    """Return the q-th percentile (0..100) using nearest-rank.

    Nearest-rank rather than an interpolating percentile because these are latency samples: an
    interpolated p99 reports a duration that no request actually experienced, which invites
    exactly the kind of argument a benchmark is supposed to settle.
    """
    if not values:
        return 0.0
    ordered = sorted(values)
    if q <= 0:
        return ordered[0]
    if q >= 100:
        return ordered[-1]
    rank = max(1, min(len(ordered), int(round(q / 100.0 * len(ordered) + 0.5))))
    return ordered[rank - 1]


def summarise_latency(samples: list[float]) -> dict[str, float]:
    """Return the standard latency summary used across every results file."""
    if not samples:
        return {
            "count": 0,
            "p50": 0.0,
            "p95": 0.0,
            "p99": 0.0,
            "mean": 0.0,
            "min": 0.0,
            "max": 0.0,
        }
    return {
        "count": len(samples),
        "p50": round(percentile(samples, 50), 3),
        "p95": round(percentile(samples, 95), 3),
        "p99": round(percentile(samples, 99), 3),
        "mean": round(sum(samples) / len(samples), 3),
        "min": round(min(samples), 3),
        "max": round(max(samples), 3),
    }
