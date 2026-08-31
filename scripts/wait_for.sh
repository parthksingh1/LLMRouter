#!/usr/bin/env bash
# wait_for.sh <name> <url> [timeout_sec] -- poll an HTTP endpoint until it answers.
set -euo pipefail

name=$1
url=$2
timeout=${3:-120}
start=$(date +%s)

printf '  waiting for %-14s ' "$name"
while true; do
  if curl -fsS -o /dev/null --max-time 2 "$url" 2>/dev/null; then
    echo "ready"
    exit 0
  fi
  now=$(date +%s)
  if [ $(( now - start )) -ge "$timeout" ]; then
    echo "TIMEOUT after ${timeout}s ($url)"
    exit 1
  fi
  printf '.'
  sleep 2
done
