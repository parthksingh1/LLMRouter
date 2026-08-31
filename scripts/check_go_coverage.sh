#!/usr/bin/env bash
# check_go_coverage.sh <min_percent> -- enforce a coverage floor over internal/ only.
set -euo pipefail

min=${1:-70}
here="$(cd "$(dirname "$0")/.." && pwd)"
profile="${COVER_PROFILE:-$here/services/gateway/coverage.out}"

[ -f "$profile" ] || { echo "no coverage profile at $profile -- run 'make test-go' first" >&2; exit 1; }

# Keep the mode line plus internal/ blocks, then let `go tool cover` do the arithmetic.
filtered=$(mktemp)
head -1 "$profile" > "$filtered"
grep '/internal/' "$profile" >> "$filtered" || true

pct=$(go tool cover -func="$filtered" | awk '/^total:/ {gsub(/%/,"",$3); print $3}')

echo "internal/ coverage: ${pct}% (floor ${min}%)"
awk -v p="$pct" -v m="$min" 'BEGIN { exit (p+0 >= m+0) ? 0 : 1 }' || {
  echo "FAIL: coverage ${pct}% is below the ${min}% floor" >&2
  exit 1
}
