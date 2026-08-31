# Architecture decision records

Each record states a decision, what it costs, and what would make us revisit it. The ones worth
reading first are those where the measurement changed the design:

- **[0002](0002-cache-threshold-calibration.md)** — measurement showed a bi-encoder cannot do
  semantic caching safely on its own, which produced the two-tier design.
- **[0003](0003-failover-semantics.md)** — what "continue the answer" actually means when the
  fallback is a different model.
- **[0007](0007-offline-benchmark-simulator.md)** — the one deliberate duplication in the
  repository, and the machinery that keeps it honest.

| # | Decision | Status |
|---|---|---|
| [0001](0001-provider-abstraction.md) | One `Provider` port for five vendors and a mock | Accepted |
| [0002](0002-cache-threshold-calibration.md) | Two-tier cache admission, threshold calibrated | Accepted |
| [0003](0003-failover-semantics.md) | Mid-stream failover via assistant-prefix continuation | Accepted |
| [0004](0004-clickhouse-vs-postgres.md) | ClickHouse for request events | Accepted |
| [0005](0005-guardrail-sidecar-vs-library.md) | Guardrails as a sidecar, failing open by default | Accepted |
| [0006](0006-compose-vs-kubernetes-for-the-demo.md) | Compose for the demo, Helm for production | Accepted |
| [0007](0007-offline-benchmark-simulator.md) | An offline routing simulator for benchmarks | Accepted |
| [0008](0008-secrets-and-tenancy.md) | Static bearer tokens, and why that is not enough | Accepted |
