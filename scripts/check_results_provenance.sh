#!/usr/bin/env bash
# Benchmark results are machine-written. Every file must carry the provenance block that
# benchmarks/common/results.py stamps on. This catches hand-edited numbers.
set -euo pipefail

fail=0
for f in "$@"; do
  for key in generated_by generated_at mode seed git_sha; do
    if ! grep -q "\"$key\"" "$f"; then
      echo "FAIL: $f is missing the '$key' provenance field -- was it hand-edited?" >&2
      echo "      Regenerate it with: make bench" >&2
      fail=1
    fi
  done
done
exit "$fail"
