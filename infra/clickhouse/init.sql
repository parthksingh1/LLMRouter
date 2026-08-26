-- LLMRouter analytics schema.
--
-- One row per logical request. Everything the dashboard and the attribution benchmark report is
-- derived from this table, so it is the contract: services/gateway/internal/events writes it,
-- services/dashboard reads it, and seed/generate_traffic.py fills it with synthetic history.
--
-- Why ClickHouse rather than Postgres: the queries are all "sum a metric over a time range,
-- grouped by a low-cardinality dimension" across hundreds of millions of rows. That is the one
-- shape a column store is dramatically better at, and the write pattern -- append-only, never
-- updated, batched -- is exactly what its MergeTree engine wants. Argued in
-- docs/adr/0004-clickhouse-vs-postgres.md.

CREATE DATABASE IF NOT EXISTS llmrouter;

-- ---------------------------------------------------------------------------------------------
-- Raw events
-- ---------------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS llmrouter.events
(
    ts                  DateTime64(3, 'UTC'),
    request_id          String,

    -- LowCardinality is a dictionary encoding. These columns have at most a few hundred
    -- distinct values across billions of rows, so it cuts both storage and group-by cost
    -- substantially. request_id is deliberately NOT LowCardinality: it is unique per row, and
    -- dictionary-encoding a unique column makes everything worse.
    tenant_id           LowCardinality(String),
    policy              LowCardinality(String),
    model               LowCardinality(String),
    provider            LowCardinality(String),

    prompt_tokens       UInt32,
    completion_tokens   UInt32,

    cached              UInt8,
    cache_similarity    Float32,

    guardrail_blocked   UInt8,
    guardrail_findings  UInt8,
    failover_count      UInt8,

    status_code         UInt16,
    latency_ms          UInt32,
    cost_usd            Float64,

    -- Materialised rather than computed at query time: total tokens is in almost every query,
    -- and ClickHouse stores it as a real column so the sum is a single pass.
    total_tokens        UInt32 MATERIALIZED prompt_tokens + completion_tokens
)
ENGINE = MergeTree
-- Partitioning by month keeps the part count small while still letting a "last 30 days" query
-- skip whole partitions. Daily partitions would be over-partitioning at this volume.
PARTITION BY toYYYYMM(ts)
-- Ordered by tenant first because every attribution query filters or groups by tenant. The
-- primary index is a sparse index over this key, so a per-tenant query reads a fraction of the
-- table rather than scanning it.
ORDER BY (tenant_id, ts, request_id)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;


-- ---------------------------------------------------------------------------------------------
-- Rollups
--
-- Materialised views maintain these incrementally as rows arrive, so the dashboard reads
-- thousands of pre-aggregated rows instead of scanning hundreds of millions. The AggregatingMergeTree
-- state columns (sumState, uniqState) merge partial aggregates across parts correctly, which a
-- plain SummingMergeTree cannot do for distinct counts.
-- ---------------------------------------------------------------------------------------------

-- Per tenant per day: what the cost dashboard and the anomaly detector read.
CREATE TABLE IF NOT EXISTS llmrouter.tenant_daily
(
    day                 Date,
    tenant_id           LowCardinality(String),
    requests            AggregateFunction(sum, UInt64),
    prompt_tokens       AggregateFunction(sum, UInt64),
    completion_tokens   AggregateFunction(sum, UInt64),
    cost_usd            AggregateFunction(sum, Float64),
    cached_requests     AggregateFunction(sum, UInt64),
    blocked_requests    AggregateFunction(sum, UInt64),
    failovers           AggregateFunction(sum, UInt64),
    latency_ms          AggregateFunction(quantiles(0.5, 0.95, 0.99), UInt32),
    models_used         AggregateFunction(uniq, String)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (tenant_id, day);

CREATE MATERIALIZED VIEW IF NOT EXISTS llmrouter.mv_tenant_daily
TO llmrouter.tenant_daily
AS SELECT
    toDate(ts)                                      AS day,
    tenant_id,
    sumState(toUInt64(1))                           AS requests,
    sumState(toUInt64(prompt_tokens))               AS prompt_tokens,
    sumState(toUInt64(completion_tokens))           AS completion_tokens,
    sumState(cost_usd)                              AS cost_usd,
    sumState(toUInt64(cached))                      AS cached_requests,
    sumState(toUInt64(guardrail_blocked))           AS blocked_requests,
    sumState(toUInt64(failover_count))              AS failovers,
    quantilesState(0.5, 0.95, 0.99)(latency_ms)     AS latency_ms,
    uniqState(model)                                AS models_used
FROM llmrouter.events
GROUP BY day, tenant_id;


-- Per model per day: the model-mix chart, and how spend splits across providers.
CREATE TABLE IF NOT EXISTS llmrouter.model_daily
(
    day                 Date,
    provider            LowCardinality(String),
    model               LowCardinality(String),
    requests            AggregateFunction(sum, UInt64),
    total_tokens        AggregateFunction(sum, UInt64),
    cost_usd            AggregateFunction(sum, Float64),
    latency_ms          AggregateFunction(quantiles(0.5, 0.95, 0.99), UInt32)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (day, provider, model);

CREATE MATERIALIZED VIEW IF NOT EXISTS llmrouter.mv_model_daily
TO llmrouter.model_daily
AS SELECT
    toDate(ts)                                      AS day,
    provider,
    model,
    sumState(toUInt64(1))                           AS requests,
    sumState(toUInt64(prompt_tokens + completion_tokens)) AS total_tokens,
    sumState(cost_usd)                              AS cost_usd,
    quantilesState(0.5, 0.95, 0.99)(latency_ms)     AS latency_ms
FROM llmrouter.events
GROUP BY day, provider, model;


-- Cache effectiveness by hour: the hit-rate-over-time chart.
--
-- Hourly rather than daily because a cache regression shows up as a step change within a day,
-- and a daily average would smear it across 24 hours of good data.
CREATE TABLE IF NOT EXISTS llmrouter.cache_hourly
(
    hour                DateTime('UTC'),
    tenant_id           LowCardinality(String),
    lookups             AggregateFunction(sum, UInt64),
    hits                AggregateFunction(sum, UInt64),
    cost_saved_usd      AggregateFunction(sum, Float64),
    mean_similarity     AggregateFunction(avg, Float32)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (tenant_id, hour);

CREATE MATERIALIZED VIEW IF NOT EXISTS llmrouter.mv_cache_hourly
TO llmrouter.cache_hourly
AS SELECT
    toStartOfHour(ts)                               AS hour,
    tenant_id,
    sumState(toUInt64(1))                           AS lookups,
    sumState(toUInt64(cached))                      AS hits,
    -- A cache hit costs nothing, so the saving is what the request WOULD have cost. The
    -- gateway writes that counterfactual into cost_usd on cached rows before zeroing the
    -- billed amount, which is the only way to answer "what is the cache worth".
    sumState(if(cached = 1, cost_usd, 0))           AS cost_saved_usd,
    avgState(cache_similarity)                      AS mean_similarity
FROM llmrouter.events
GROUP BY hour, tenant_id;


-- ---------------------------------------------------------------------------------------------
-- Convenience views
--
-- The -Merge functions are how AggregateFunction columns are read. Wrapping them in views means
-- the dashboard and the benchmark write ordinary SQL and cannot get the merge wrong.
-- ---------------------------------------------------------------------------------------------

CREATE VIEW IF NOT EXISTS llmrouter.v_tenant_daily AS
SELECT
    day,
    tenant_id,
    sumMerge(requests)                              AS requests,
    sumMerge(prompt_tokens)                         AS prompt_tokens,
    sumMerge(completion_tokens)                     AS completion_tokens,
    sumMerge(cost_usd)                              AS cost_usd,
    sumMerge(cached_requests)                       AS cached_requests,
    sumMerge(blocked_requests)                      AS blocked_requests,
    sumMerge(failovers)                             AS failovers,
    quantilesMerge(0.5, 0.95, 0.99)(latency_ms)     AS latency_quantiles,
    uniqMerge(models_used)                          AS models_used
FROM llmrouter.tenant_daily
GROUP BY day, tenant_id;

CREATE VIEW IF NOT EXISTS llmrouter.v_model_daily AS
SELECT
    day,
    provider,
    model,
    sumMerge(requests)                              AS requests,
    sumMerge(total_tokens)                          AS total_tokens,
    sumMerge(cost_usd)                              AS cost_usd,
    quantilesMerge(0.5, 0.95, 0.99)(latency_ms)     AS latency_quantiles
FROM llmrouter.model_daily
GROUP BY day, provider, model;

CREATE VIEW IF NOT EXISTS llmrouter.v_cache_hourly AS
SELECT
    hour,
    tenant_id,
    sumMerge(lookups)                               AS lookups,
    sumMerge(hits)                                  AS hits,
    sumMerge(hits) / nullIf(sumMerge(lookups), 0)   AS hit_rate,
    sumMerge(cost_saved_usd)                        AS cost_saved_usd,
    avgMerge(mean_similarity)                       AS mean_similarity
FROM llmrouter.cache_hourly
GROUP BY hour, tenant_id;


-- Monthly spend share per tenant: what the runaway-workload claim is computed from.
--
-- Kept as a view rather than another materialised view because it is read a handful of times a
-- day by a human, not on every page load, and a view cannot drift from its source.
CREATE VIEW IF NOT EXISTS llmrouter.v_tenant_month_share AS
SELECT
    toStartOfMonth(day)                             AS month,
    tenant_id,
    sum(cost_usd)                                   AS cost_usd,
    sum(requests)                                   AS requests,
    cost_usd / sum(cost_usd) OVER (PARTITION BY month) AS share_of_month
FROM llmrouter.v_tenant_daily
GROUP BY month, tenant_id, day, cost_usd, requests;
