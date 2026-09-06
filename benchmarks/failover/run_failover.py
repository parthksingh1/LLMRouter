#!/usr/bin/env python3
"""Run the mid-stream failover benchmark.

    python benchmarks/failover/run_failover.py --mode offline --out benchmarks/results/failover.json

This is a thin wrapper. The benchmark itself is Go (`services/gateway/cmd/failoverbench`) and
that is deliberate: failover is the one claim where a Python reimplementation would be
worthless. The whole point is to exercise the real `stream.Runner`, the real stall detection and
the real prefix-continuation logic rather than a model of them, so the harness has to be in the
same language as the code under test.

The wrapper exists so `make bench` has one uniform entry point per claim, and so a run without a
Go toolchain fails with an explanation and the existing committed result rather than a
`command not found`.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))

from benchmarks.common.results import env_seed, read_result  # noqa: E402

GATEWAY_DIR = REPO_ROOT / "services" / "gateway"
DEFAULT_OUT = REPO_ROOT / "benchmarks" / "results" / "failover.json"


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--mode",
        choices=["offline", "gateway"],
        default="offline",
        help="accepted for symmetry with the other benchmarks; this one always "
        "drives the real Go implementation",
    )
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--streams", type=int, default=10_000)
    parser.add_argument("--failure-rate", type=float, default=0.05)
    parser.add_argument("--stall-rate", type=float, default=0.01)
    args = parser.parse_args()

    if shutil.which("go") is None:
        # Do not fail the whole `make bench` run: report clearly and leave the committed result
        # in place, which is more useful than deleting a measurement nobody can reproduce here.
        print(
            "failover benchmark SKIPPED: no Go toolchain on PATH.\n"
            "  This benchmark drives the real stream.Runner, so it needs Go 1.23+.\n"
            "  Install Go, or run it in a container:\n"
            '    docker run --rm -v "$PWD/services/gateway":/src -w /src golang:1.23-alpine \\\n'
            "      go run ./cmd/failoverbench -config /src/../../config",
            file=sys.stderr,
        )
        try:
            existing = read_result(args.out)
            print(
                f"  keeping the committed result: "
                f"{existing['completion_rate_pct']}% completion over {existing['streams']:,} streams"
            )
        except (FileNotFoundError, KeyError):
            print("  and there is no committed result to fall back on", file=sys.stderr)
            return 1
        return 0

    command = [
        "go",
        "run",
        "./cmd/failoverbench",
        "-streams",
        str(args.streams),
        "-failure-rate",
        str(args.failure_rate),
        "-stall-rate",
        str(args.stall_rate),
        "-seed",
        str(env_seed()),
        "-config",
        str(REPO_ROOT / "config"),
        "-out",
        str(args.out.resolve()),
    ]

    print(f"running the failover benchmark in Go ({args.streams:,} streams)...")
    result = subprocess.run(command, cwd=GATEWAY_DIR, check=False)
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
