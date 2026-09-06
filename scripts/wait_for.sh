#!/usr/bin/env bash
# wait_for.sh <name> <url> [timeout_sec] -- poll an HTTP endpoint until it answers.
#
# On timeout it prints why, rather than only that it happened. A bare "TIMEOUT after 180s" tells
# you a service did not come up and nothing about whether the container crashed, is restarting,
# never got scheduled, or is running fine and returning 503. That distinction is the whole
# diagnosis, and reading it out of a separate log-dump step at the end of the job means matching
# it up by hand -- so the check that failed reports it.
set -euo pipefail

name=$1
url=$2
timeout=${3:-120}
start=$(date +%s)

# Best-effort diagnosis. Every command here is allowed to fail: this runs on a path that is
# already failing, and it must not mask the original error with one of its own.
diagnose() {
  command -v docker >/dev/null 2>&1 || return 0

  echo
  echo "--- container status -------------------------------------------------"
  docker compose ps 2>&1 | sed 's/^/  /' || true

  echo
  echo "--- $name: exit code and restart count -------------------------------"
  cid=$(docker compose ps -q "$name" 2>/dev/null | head -1 || true)
  if [ -n "${cid:-}" ]; then
    docker inspect \
      --format '  state={{.State.Status}} exit={{.State.ExitCode}} restarts={{.RestartCount}} oom={{.State.OOMKilled}} error={{.State.Error}}' \
      "$cid" 2>&1 || true
  else
    echo "  no container found for service '$name' -- it never started"
  fi

  echo
  echo "--- $name: last 80 log lines -----------------------------------------"
  docker compose logs --no-color --tail=80 "$name" 2>&1 | sed 's/^/  /' || true

  echo
  echo "--- last response from $url ------------------------------------------"
  # -f is deliberately omitted: a 503 body is the interesting case, and -f discards it.
  curl -sS -i --max-time 5 "$url" 2>&1 | head -20 | sed 's/^/  /' || true
  echo
}

printf '  waiting for %-14s ' "$name"
while true; do
  if curl -fsS -o /dev/null --max-time 2 "$url" 2>/dev/null; then
    echo "ready"
    exit 0
  fi
  now=$(date +%s)
  if [ $(( now - start )) -ge "$timeout" ]; then
    echo "TIMEOUT after ${timeout}s ($url)"
    diagnose
    exit 1
  fi
  printf '.'
  sleep 2
done
