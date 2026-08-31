#!/usr/bin/env bash
# `make failover-demo`
#
# Restarts the gateway with mid-stream failure injection armed on Anthropic, opens a stream,
# and shows the synthetic `event: failover` arriving without the client connection breaking.
set -euo pipefail

cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
GATEWAY=${GATEWAY:-http://localhost:8080}

step() { printf '\n\033[1;36m==>\033[0m \033[1m%s\033[0m\n' "$1"; }

step "Arming mid-stream failure injection on the anthropic mock"
MOCK_ANTHROPIC_FAIL_MIDSTREAM=true \
MOCK_MIDSTREAM_FAIL_AFTER_TOKENS=24 \
  $COMPOSE up -d --no-deps gateway
bash scripts/wait_for.sh gateway "$GATEWAY/readyz" 90

step "Streaming a request that will be routed to anthropic and then broken mid-generation"
echo "  (watch for 'event: failover' -- the SSE connection itself never closes)"
echo

curl -sS -N \
  -H "Authorization: Bearer demo-tenant-a" \
  -H "Content-Type: application/json" \
  -H "x-llmrouter-policy: quality_tiered" \
  -d '{
        "model": "claude-3-5-sonnet",
        "stream": true,
        "messages": [
          {"role": "user", "content": "Explain the trade-offs of optimistic concurrency control, step by step."}
        ]
      }' \
  "$GATEWAY/v1/chat/completions" \
  | sed -u -e 's/^event: failover$/\x1b[1;33mevent: failover\x1b[0m/'

step "Disarming injection and restoring the gateway"
MOCK_ANTHROPIC_FAIL_MIDSTREAM=false $COMPOSE up -d --no-deps gateway
bash scripts/wait_for.sh gateway "$GATEWAY/readyz" 90

cat <<'BANNER'

What just happened:

  1. The router picked anthropic/claude-3-5-sonnet for a "hard" prompt.
  2. The mock provider killed the upstream stream after 24 tokens.
  3. The gateway detected the break WITHOUT closing your SSE connection, replayed the
     accumulated assistant tokens to the fallback provider as an assistant prefix, and
     continued generation from there.
  4. A synthetic `event: failover` carried the metadata; every `data:` frame stayed
     OpenAI-compatible, so a vanilla SDK sees one uninterrupted stream.
  5. Token accounting was preserved across the seam for billing.

  See the trace:  http://localhost:16686  -> service "llmrouter-gateway" -> tag failover=true
  See the metric: http://localhost:9091/graph?g0.expr=llmrouter_failover_total

BANNER
