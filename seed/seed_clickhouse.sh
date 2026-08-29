#!/usr/bin/env bash
# Generate synthetic history and load it into ClickHouse.
#
# Run by `make demo`. Idempotent: it truncates the events table first, so running it twice does
# not double every number on the dashboard.
set -euo pipefail

cd "$(dirname "$0")/.."

CLICKHOUSE_URL=${CLICKHOUSE_URL:-http://localhost:8123}
DATABASE=${CLICKHOUSE_DB:-llmrouter}
DAYS=${SEED_DAYS:-30}
TARGET=${SEED_TARGET_EVENTS:-200000}
SEED=${SEED:-1337}
EVENTS=seed/out/events.tsv

PY=${PY:-python}
if [ -x ".venv/Scripts/python.exe" ]; then PY=".venv/Scripts/python.exe"
elif [ -x ".venv/bin/python" ]; then PY=".venv/bin/python"
fi

query() {
  curl -sS --fail-with-body --max-time 300 \
    --data-binary @- "${CLICKHOUSE_URL}/?database=${DATABASE}" <<< "$1"
}

echo "  waiting for clickhouse at ${CLICKHOUSE_URL}"
for _ in $(seq 1 60); do
  if curl -fsS -o /dev/null --max-time 2 "${CLICKHOUSE_URL}/ping" 2>/dev/null; then break; fi
  sleep 2
done

# The schema normally arrives via the container's docker-entrypoint-initdb.d, but applying it
# again is harmless (every statement is IF NOT EXISTS) and makes this script usable against a
# ClickHouse that was started some other way.
echo "  applying schema"
curl -sS --fail-with-body --max-time 60 --data-binary @infra/clickhouse/init.sql \
  "${CLICKHOUSE_URL}/" > /dev/null

if [ ! -f "$EVENTS" ]; then
  echo "  generating ${TARGET} events across ${DAYS} days (seed ${SEED})"
  "$PY" seed/generate_traffic.py --days "$DAYS" --target "$TARGET" --seed "$SEED" --out "$EVENTS"
else
  echo "  reusing $EVENTS ($(du -h "$EVENTS" | cut -f1)); delete it to regenerate"
fi

echo "  truncating existing events"
query "TRUNCATE TABLE IF EXISTS ${DATABASE}.events"
# The materialised views write to their own tables, so those need clearing too or the rollups
# would keep the previous run's aggregates and every chart would be double-counted.
for table in tenant_daily model_daily cache_hourly; do
  query "TRUNCATE TABLE IF EXISTS ${DATABASE}.${table}"
done

echo "  loading events"
COLUMNS="ts,request_id,tenant_id,policy,model,provider,prompt_tokens,completion_tokens,cached,cache_similarity,guardrail_blocked,guardrail_findings,failover_count,status_code,latency_ms,cost_usd"
curl -sS --fail-with-body --max-time 600 \
  --data-binary "@${EVENTS}" \
  "${CLICKHOUSE_URL}/?database=${DATABASE}&query=INSERT%20INTO%20events%20(${COLUMNS//,/%2C})%20FORMAT%20TabSeparated" \
  > /dev/null

# The materialised views only fire on INSERT, and the rollup tables were just truncated, so they
# are repopulated from the raw events here. Without this the dashboard would be empty.
echo "  rebuilding rollups"
query "INSERT INTO ${DATABASE}.tenant_daily SELECT toDate(ts) AS day, tenant_id,
         sumState(toUInt64(1)), sumState(toUInt64(prompt_tokens)), sumState(toUInt64(completion_tokens)),
         sumState(cost_usd), sumState(toUInt64(cached)), sumState(toUInt64(guardrail_blocked)),
         sumState(toUInt64(failover_count)), quantilesState(0.5, 0.95, 0.99)(latency_ms), uniqState(model)
       FROM ${DATABASE}.events GROUP BY day, tenant_id"

query "INSERT INTO ${DATABASE}.model_daily SELECT toDate(ts) AS day, provider, model,
         sumState(toUInt64(1)), sumState(toUInt64(prompt_tokens + completion_tokens)),
         sumState(cost_usd), quantilesState(0.5, 0.95, 0.99)(latency_ms)
       FROM ${DATABASE}.events GROUP BY day, provider, model"

query "INSERT INTO ${DATABASE}.cache_hourly SELECT toStartOfHour(ts) AS hour, tenant_id,
         sumState(toUInt64(1)), sumState(toUInt64(cached)),
         sumState(if(cached = 1, cost_usd, 0)), avgState(cache_similarity)
       FROM ${DATABASE}.events GROUP BY hour, tenant_id"

ROWS=$(query "SELECT count() FROM ${DATABASE}.events" | tr -d '[:space:]')
SPEND=$(query "SELECT round(sum(cost_usd), 2) FROM ${DATABASE}.events WHERE cached = 0" | tr -d '[:space:]')
echo "  loaded ${ROWS} events, \$${SPEND} of billed spend"
