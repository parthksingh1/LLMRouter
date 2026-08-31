#!/usr/bin/env bash
# Fail if the repository exceeds its size budget.
set -euo pipefail

MAX_REPO_MB=${MAX_REPO_MB:-50}
MAX_FILE_MB=${MAX_FILE_MB:-5}

cd "$(dirname "$0")/.."

# Total size of tracked + untracked files, excluding .git and ignored paths.
total_kb=$(du -sk --exclude=.git --exclude=.venv --exclude=node_modules . | cut -f1)
total_mb=$(( total_kb / 1024 ))

fail=0
if [ "$total_mb" -gt "$MAX_REPO_MB" ]; then
  echo "FAIL: working tree is ${total_mb} MB, budget is ${MAX_REPO_MB} MB" >&2
  fail=1
fi

max_bytes=$(( MAX_FILE_MB * 1024 * 1024 ))
while IFS= read -r -d '' f; do
  sz=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f")
  if [ "$sz" -gt "$max_bytes" ]; then
    echo "FAIL: $f is $(( sz / 1024 / 1024 )) MB, per-file budget is ${MAX_FILE_MB} MB" >&2
    fail=1
  fi
done < <(find . -type f \
           -not -path './.git/*' -not -path './.venv/*' -not -path './node_modules/*' \
           -print0)

if [ "$fail" -eq 0 ]; then
  echo "OK: working tree ${total_mb} MB / ${MAX_REPO_MB} MB, no file over ${MAX_FILE_MB} MB"
fi
exit "$fail"
