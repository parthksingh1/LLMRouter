#!/usr/bin/env bash
# `make demo` -- bring the whole offline stack up, seed it, warm it, and print where to look.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=${COMPOSE:-docker compose}
LOAD_SECONDS=${LOAD_SECONDS:-60}

bold() { printf '\033[1m%s\033[0m\n' "$1"; }
step() { printf '\n\033[1;36m==>\033[0m \033[1m%s\033[0m\n' "$1"; }

# ---------------------------------------------------------------------------
step "Checking prerequisites"
command -v docker >/dev/null || { echo "docker is required"; exit 1; }
docker info >/dev/null 2>&1 || { echo "the docker daemon is not running -- start Docker Desktop and retry"; exit 1; }
python -c 'import sys; sys.exit(0 if sys.version_info >= (3,11) else 1)' 2>/dev/null \
  || echo "  note: python 3.11+ not found on PATH; seeding will run inside a container"
echo "  ok"

# ---------------------------------------------------------------------------
step "Building and starting the stack"
$COMPOSE up -d --build

# ---------------------------------------------------------------------------
step "Waiting for readiness"
bash scripts/wait_for.sh clickhouse http://localhost:8123/ping        120
bash scripts/wait_for.sh qdrant     http://localhost:6333/readyz      60
bash scripts/wait_for.sh embedder   http://localhost:8001/healthz     180
bash scripts/wait_for.sh guardrails http://localhost:8000/healthz     120
bash scripts/wait_for.sh gateway    http://localhost:8080/readyz      120
bash scripts/wait_for.sh grafana    http://localhost:3000/api/health  120
bash scripts/wait_for.sh dashboard  http://localhost:8501/_stcore/health 120

# ---------------------------------------------------------------------------
step "Seeding ~30 days of synthetic traffic into ClickHouse"
bash seed/seed_clickhouse.sh

# ---------------------------------------------------------------------------
step "Running a ${LOAD_SECONDS}s live burst through the gateway"
python benchmarks/load/warm_traffic.py --seconds "$LOAD_SECONDS" --gateway http://localhost:8080

# ---------------------------------------------------------------------------
cat <<'BANNER'

LLMRouter demo is ready.

  Gateway            http://localhost:8080/v1
  Dashboard          http://localhost:8501
  Grafana            http://localhost:3000  (admin / admin)
  Jaeger             http://localhost:16686
  Prometheus         http://localhost:9091
  ClickHouse         http://localhost:8123/play

  Try it:
    curl -H "Authorization: Bearer demo-tenant-a" \
         -H "Content-Type: application/json" \
         -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}' \
         http://localhost:8080/v1/chat/completions

  Reproduce resume numbers:
    make bench         # prints cost savings, cache hit rate, guardrail latency
    make failover-demo # forces a mid-stream failover you can watch in Jaeger

BANNER
