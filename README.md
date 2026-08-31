# LLMRouter

An OpenAI-compatible LLM gateway that sits between applications and five providers, adding
policy routing, mid-stream failover, semantic caching, guardrails, per-tenant budgets and cost
attribution.

**It runs entirely offline.** No API keys, no network egress, no cloud account. Provider calls
are served by a deterministic mock harness that samples real latency distributions, so the
timings you measure are the timings the architecture produces.

```bash
git clone <this repo> && cd LLMRouter
make demo          # Windows without GNU make: .\make.ps1 demo
```

Then:

```bash
curl -H "Authorization: Bearer demo-tenant-a" \
     -H "Content-Type: application/json" \
     -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}' \
     http://localhost:8080/v1/chat/completions
```

| | |
|---|---|
| Gateway | http://localhost:8080/v1 |
| Cost dashboard | http://localhost:8501 |
| Grafana | http://localhost:3000 (`admin` / `admin`) |
| Jaeger | http://localhost:16686 |
| ClickHouse | http://localhost:8123/play |

---

## Measured results

Every number below is read from `benchmarks/results/*.json` by `benchmarks/report.py`. **None of
it is typed by hand.** Reproduce it with `make bench` — offline mode needs only Python 3.11, no
Docker.

<!-- BENCHMARK TABLE START -->
| What | Measured | Produced by |
|---|---|---|
| Cost reduction from policy routing | **52.0%** | `make bench-eval` |
| Quality drift that reduction cost | **1.5 pp** (100.0% → 98.5%) | `make bench-eval` |
| Difficulty classifier accuracy | **80.0%** | `make bench-eval` |
| Cache hit rate on seeded traffic | **39.6%** | `make bench-cache` |
| Cache false-hit rate | **0.00%** of 5,405 near misses | `make bench-cache` |
| Latency, uncached → cached (p50) | **2,471 ms → 0.8 ms** | `make bench-cache` |
| Calibrated similarity threshold | **0.9962** (bge-small, AUC 0.843) | `make bench-cache` |
| Guardrails p99 (budget 12 ms) | **1.78 ms** | `make bench-guardrails` |
| Stream completion at a 5% mid-stream failure rate | **100.00%** of 10,000 streams | `make bench-failover` |
| Streams recovered by failover | **489** (4.9%), 16 restarted | `make bench-failover` |
| Runaway workloads found from spend alone | **3 tenants**, precision 1.00, recall 1.00 | `make bench-attribution` |
<!-- BENCHMARK TABLE END -->

### What these numbers do and do not mean

They are honest measurements of a **seeded workload**, and the workload is a modelling choice
you can inspect and disagree with:

- **Cost and quality** come from a 200-case eval set where a model "answers correctly" if its
  measured quality clears the case's difficulty threshold. That is a step function, not a judge.
  It faithfully measures *the cost of the routing decision* — how often routing down loses an
  answer — and it is not a claim about real model capability.
- **The cost saving depends heavily on the difficulty mix** (about 35% easy / 49% medium / 16%
  hard). `benchmarks/results/eval.json` includes a sensitivity sweep showing what the headline
  numbers do across the two cost/quality dials, so the chosen operating point is a measurement
  rather than a preference.
- **Cache latency is measured asymmetrically** and the results file says so: the cached path is
  real wall clock, the uncached path is sampled from the configured provider latency
  distribution. Sleeping for real would make a 50,000-request benchmark take a day.
- **The failover benchmark drives the real code** — the real runner, the real continuation
  logic, the real stall detection — with the clock accelerated. It is the one benchmark that is
  not bit-reproducible: goroutine interleaving varies with the worker count, so repeated runs
  land between about 99.97% and 100% completion. Both are the same result; treat the last
  decimal as noise rather than signal.

If you want to argue with a number, argue with the seed generator. That is the point of shipping
it.

---

## What is actually interesting here

Three places where measuring something changed the design. These are the parts worth reading.

### 1. A bi-encoder cannot do semantic caching safely on its own

The obvious design is: embed the prompt, find the nearest neighbour, serve it above a similarity
threshold. We built that and measured it against 500 paraphrases and 500 hard negatives:

| Negative kind | mean similarity | max |
|---|---|---|
| Different question | 0.552 | 0.827 |
| Same question, different subject | 0.828 | 0.934 |
| **One word changed, meaning inverted** | **0.934** | **0.998** |
| *Paraphrase (should hit)* | *0.958* | *0.997* |

"Should I **increase** the timeout" scores higher against "should I **decrease** the timeout"
than most genuine paraphrases do. At a 0.5% false-hit budget, similarity alone admitted **0.2%**
of paraphrases — a cache that hits one time in five hundred.

So the cache admits in two tiers: an exact tier (content-token equality after conservative
normalisation) that is certain and carries most of the value, and a semantic tier guarded by
that same rule so a minimal pair can score 0.998 and still be refused. The calibrator reports
the semantic tier as *not worthwhile* on the adversarial set and says so in the results file
rather than quietly recommending a threshold that loses money.

→ [ADR-0002](docs/adr/0002-cache-threshold-calibration.md)

### 2. Failing over after the client has already read half an answer

A provider can die when the status is long since 200 and forty tokens are on the wire. The
gateway accumulates every delta as it forwards it, detects both resets *and stalls* (an upstream
that goes quiet is as dead as one that errors, and the socket stays open so a plain read loop
waits forever), then sends the accumulated text to a fallback as a trailing assistant message so
it continues rather than restarts.

The failover metadata rides on a **named** SSE event, which OpenAI SDKs skip — so a vanilla
client sees one uninterrupted stream. Token usage is summed across attempts so a failed-over
request is billed once and completely.

```
event: failover
data: {"from":"anthropic","to":"openai","mode":"continue","tokens_preserved":21}
```

Where the fallback cannot continue, generation restarts and the event says `restarted: true` —
because the client's transcript really does contain a discontinuity, and that answer is not
written to the cache.

→ [ADR-0003](docs/adr/0003-failover-semantics.md) · watch it live with `make failover-demo`

### 3. A sustained runaway hides from a naive anomaly detector

The first version of the cost anomaly detector scored each day against the whole month's median
and found **none** of the three planted runaway workloads. A real runaway is not a spike: it
starts when a bad deploy ships and persists for weeks, so those elevated days *become* the
baseline. A large enough anomaly hides itself.

It now scores each day against that tenant's own trailing 14-day median, and requires both
statistical significance and a material (≥2×) increase — significance alone flagged four tenants
whose spend had moved 20%. Precision and recall are now both 1.00.

The same module powers the benchmark and the dashboard, so the committed result and the page a
human looks at cannot disagree.

---

## Architecture

```
Client ──▶ Gateway (Go) ──▶ Router ──▶ Provider adapters ×5 (mock | live)
             │                            │
             ├──▶ Guardrails (FastAPI)    └──▶ Stream runner (mid-stream failover)
             ├──▶ Semantic cache (Redis + Qdrant + embedder sidecar)
             ├──▶ Budgets (Redis Lua, atomic)
             └──▶ Events ──▶ ClickHouse ──▶ Streamlit dashboard
                    └──────▶ OTel Collector ──▶ Jaeger / Prometheus / Grafana
```

Hexagonal: `internal/domain` imports nothing, `internal/app` declares the ports it needs, and
every adapter implements one. The mock providers satisfy the same `Provider` interface as the
live ones, which is why the offline demo exercises the real router, the real failover state
machine and the real cache — only the bytes on the wire are simulated.

Full sequence diagrams for the cached hit, the cache miss, the mid-stream failover and the
guardrail block: **[ARCHITECTURE.md](ARCHITECTURE.md)**.

### Every dependency degrades a feature, not the service

| Down | Effect |
|---|---|
| Guardrails | Requests proceed unscreened, counted and alerted (`fail_mode=closed` refuses instead) |
| Redis | No cache, no budget enforcement; requests still served |
| Qdrant / embedder | Semantic tier off; the exact tier still works |
| ClickHouse | Events dropped with a counter; no user impact |
| One provider | Breaker opens, router avoids it, in-flight streams fail over |
| All providers | `/readyz` fails; cache hits still served |

---

## API

Wire-compatible with the OpenAI SDKs — point `base_url` at the gateway and change nothing else.

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="demo-tenant-a")

# "auto" lets the policy engine choose. "auto:cheap", "auto:fast" and "auto:best"
# force a specific policy; a concrete model id pins the request.
response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Explain consistent hashing"}],
)

print(response.model)                    # the model actually chosen
print(response.llmrouter["cost_usd"])    # what it cost
print(response.llmrouter["difficulty"])  # why it was chosen
```

Endpoints: `POST /v1/chat/completions` (streaming and not), `POST /v1/embeddings`,
`GET /v1/models`, `GET /healthz`, `GET /readyz`, `GET /metrics`.

Working examples for the Python and Node SDKs are in [`examples/`](examples/).

### Routing policies

Declared in [`config/policies.yaml`](config/policies.yaml); adding one is configuration, not code.

| Policy | Chooses |
|---|---|
| `quality_tiered` | Classifies prompt difficulty, then the cheapest model clearing that bucket's quality floor |
| `cost_optimized` | Cheapest model above a quality floor and under a latency ceiling |
| `latency_optimized` | Fastest healthy provider by *observed* TTFB |
| `weighted_round_robin` | Deterministic interleaving for A/B tests — exact ratios over any window |
| `canary` | A percentage to a new model, sticky by tenant + prompt |

---

## Repository layout

```
services/gateway/      Go. The hot path.
  internal/domain/       entities; imports nothing
  internal/app/          use cases; declares its ports
  internal/router/       policy engine, difficulty classifier
  internal/stream/       mid-stream failover state machine
  internal/providers/    registry, breakers, mock harness, 5 live adapters
  internal/cache/        two-tier semantic cache
  internal/{budget,guardrails,events,telemetry,http}/
services/guardrails/   Python. PII, injection, secrets, toxicity.
services/embedder/     Python. bge-small-en-v1.5 baked into the image.
services/dashboard/    Streamlit. Cost attribution.
benchmarks/            Every number in this README.
seed/                  Deterministic generators. `make verify-determinism` proves it.
infra/                 ClickHouse, OTel, Prometheus, Grafana + reference Helm/Terraform
docs/adr/              Eight decision records
```

---

## Quality gates

```bash
make test        # Go (race detector) + Python
make lint        # golangci-lint, gofumpt, ruff, mypy --strict
make bench       # every number in this README
make size-check  # repo under 50 MB, no file over 5 MB
```

| Gate | Floor | Current |
|---|---|---|
| Go coverage on `internal/` | 65% | 67.2% |
| Python coverage on guardrails | 80% | 98% |
| Guardrails p99 | 12 ms | 1.78 ms |
| Repo size | 50 MB | ~4 MB |

Three gates exist specifically to stop this project lying to you:

- **`scripts/check_results_provenance.sh`** rejects any benchmark result missing its provenance
  block — a pre-commit hook that catches hand-edited numbers.
- **`make verify-determinism`** regenerates every seed artifact and fails if a byte changed.
- **Cross-language parity tests** assert the Go and Python implementations of the hash embedder
  and the cache normalisation agree, so a calibration cannot drift from the cache that runs.

---

## Honest limitations

- **Prompt-injection detection is a rule set, not a model.** It catches known surface forms and
  is not adversarially robust. Anyone who reads the rules can write around them. A trained
  classifier drops in behind the same interface.
- **PII detection has no NER**, so a person's name in free text is missed. Structured
  identifiers — emails, cards, IBANs, SSNs — are caught reliably, with checksums.
- **Tenant auth is a static bearer token** with no rotation, expiry or revocation. The
  `TenantStore` interface is the seam for OIDC or mTLS. ([ADR-0008](docs/adr/0008-secrets-and-tenancy.md))
- **The analytics sink drops on failure** rather than blocking a user request. Kafka is the
  correct fix at scale. ([ADR-0004](docs/adr/0004-clickhouse-vs-postgres.md))
- **The offline benchmark simulator duplicates the routing logic** in Python. It reads the same
  YAML and CI fails on divergence, but it is a real maintenance cost.
  ([ADR-0007](docs/adr/0007-offline-benchmark-simulator.md))
- **Helm and Terraform are reference material** and are never applied by CI.

---

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — sequence diagrams and failure modes
- [docs/adr/](docs/adr/) — eight decision records, each with what it costs
- [docs/owasp-llm-mapping.md](docs/owasp-llm-mapping.md) — what is and is not addressed
- [docs/demo-script.md](docs/demo-script.md) — a five-minute walkthrough
- [CONTRIBUTING.md](CONTRIBUTING.md) · [SECURITY.md](SECURITY.md)

MIT licensed.
