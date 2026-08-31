# 4. ClickHouse for request events

**Status:** Accepted

## Context

Every request produces one analytics row: tenant, model, provider, tokens, cost, latency,
cached, guardrail outcome. At a modest 100 requests/second that is ~260 million rows a year, and
the queries against it are all the same shape:

> sum a metric over a time range, grouped by one or two low-cardinality dimensions

"Spend per tenant per day." "Cache hit rate per hour." "Token volume by model this month."

## Decision

ClickHouse, with `MergeTree` for raw events and `AggregatingMergeTree` rollups maintained by
materialised views.

Postgres would work at the low end and stop working somewhere in the tens of millions of rows —
not because it is slow, but because it is row-oriented. `SELECT sum(cost_usd) ... GROUP BY
tenant_id` has to read every column of every row to sum one of them. A column store reads one
column. On this schema that is roughly a 20x difference in bytes touched, and the gap grows with
the number of columns.

The write pattern reinforces it: append-only, never updated, batched. That is exactly what
`MergeTree` is built for, and exactly what makes Postgres pay for MVCC machinery this workload
never uses.

Details that matter more than the engine choice:

- **`ORDER BY (tenant_id, ts, request_id)`.** The primary index is sparse over this key, so a
  per-tenant query reads a fraction of the table instead of scanning it. Every attribution query
  filters or groups by tenant.
- **`LowCardinality` on the dimensions, and deliberately not on `request_id`.** Dictionary
  encoding is a large win for a column with a few hundred distinct values and a loss for one
  that is unique per row.
- **`PARTITION BY toYYYYMM(ts)`.** Monthly keeps the part count sane while still letting "last
  30 days" skip whole partitions. Daily would be over-partitioning at this volume.
- **Rollups as materialised views.** The dashboard reads thousands of pre-aggregated rows rather
  than scanning hundreds of millions. `AggregateFunction` state columns merge partial aggregates
  correctly across parts, which a `SummingMergeTree` cannot do for distinct counts.

## Consequences

Another stateful service to run, and AWS has no managed ClickHouse — the Terraform module puts
it on EC2 and says so rather than pretending the problem is not there.

The bigger cost is that the sink loses data on failure. `internal/events` batches and drops when
the queue is full or an insert fails, because the alternative is blocking a user's request on an
analytics write. **This is the weakest link in the pipeline and it is deliberate.** The correct
fix at scale is to write to Kafka and let a consumer own delivery; that is a second piece of
infrastructure and a real design decision rather than an oversight.

ClickHouse also has no transactions, so an event could in principle be double-counted after a
retry. Since the sink does not retry, that does not arise today — but it is a constraint on any
future retry logic.

## Alternatives considered

**Postgres with monthly partitions and a nightly rollup job.** Fine to a few million rows a
month, one less service to run, and a rollup job is a cron that can fail silently at 3am.
Rejected on the growth curve rather than on today's volume.

**TimescaleDB.** Postgres ergonomics with columnar compression, and a genuinely reasonable
middle ground. ClickHouse wins on raw aggregate throughput and on the fact that essentially the
entire query workload here is aggregates.

**Ship to a vendor (Datadog, Honeycomb).** Excellent products, and the per-event cost is exactly
the thing this project exists to measure. Paying per event to discover that you are spending too
much per event has an irony to it.

## Revisit when

Event volume exceeds what a single node handles comfortably — roughly a billion rows — at which
point the choice is ClickHouse Cloud or a cluster. Or when losing events on a failed insert
stops being acceptable, at which point Kafka goes in front of it.
