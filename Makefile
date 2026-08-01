# LLMRouter -- developer entrypoints.
#
# Everything on the happy path is offline: no API keys, no network egress.
# Windows users without GNU make: use ./make.ps1 <target>, which mirrors these rules.

SHELL          := /bin/bash
.DEFAULT_GOAL  := help
.SHELLFLAGS    := -eu -o pipefail -c

COMPOSE        ?= docker compose
PY             ?= python
VENV           ?= .venv
# bench mode: offline (pure python, no docker) | gateway (drives the live stack)
MODE           ?= offline
SEED           ?= 1337
GO_IMAGE       ?= golang:1.23-alpine
GATEWAY_DIR    := services/gateway
RESULTS        := benchmarks/results

# Run a go command inside a container so a local Go toolchain is optional.
GO_RUN = $(COMPOSE) run --rm --no-deps -T gateway-tools

.PHONY: help
help: ## Show this help
	@echo "LLMRouter targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Demo
# ---------------------------------------------------------------------------

.PHONY: demo
demo: ## Bring up the full stack, seed 30 days of traffic, run a load burst, print URLs
	@bash scripts/demo.sh

.PHONY: up
up: ## Start the stack in the background
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop the stack and remove volumes
	$(COMPOSE) down -v --remove-orphans

.PHONY: logs
logs: ## Tail gateway + guardrails logs
	$(COMPOSE) logs -f gateway guardrails

.PHONY: ps
ps: ## Show container status
	$(COMPOSE) ps

.PHONY: failover-demo
failover-demo: ## Force a mid-stream provider failure and stream the recovery live
	@bash scripts/failover_demo.sh

# ---------------------------------------------------------------------------
# Seed data (deterministic; SEED pins every generator)
# ---------------------------------------------------------------------------

.PHONY: seed
seed: seed-eval seed-pairs seed-fixtures ## Regenerate every committed seed artifact

.PHONY: seed-eval
seed-eval: ## Generate the 200-case eval set
	$(PY) seed/generate_eval_set.py --seed $(SEED) --out benchmarks/eval/dataset.jsonl

.PHONY: seed-pairs
seed-pairs: ## Generate 500 positive / 500 hard-negative cache pairs
	$(PY) seed/generate_cache_pairs.py --seed $(SEED) --out benchmarks/cache/pairs.jsonl

.PHONY: seed-fixtures
seed-fixtures: ## Generate deterministic mock-provider responses
	$(PY) seed/generate_fixtures.py --seed $(SEED) --out seed/fixtures/responses.jsonl

.PHONY: seed-clickhouse
seed-clickhouse: ## Load ~30 days / ~200k synthetic events into ClickHouse
	@bash seed/seed_clickhouse.sh

.PHONY: verify-determinism
verify-determinism: ## Regenerate seeds and fail if anything changed
	@bash scripts/verify_determinism.sh

# ---------------------------------------------------------------------------
# Benchmarks -- every resume number comes from here
# ---------------------------------------------------------------------------

.PHONY: bench
bench: bench-eval bench-cache bench-guardrails bench-failover bench-attribution report ## Run every benchmark and print the summary table

.PHONY: bench-eval
bench-eval: ## Cost reduction and quality drift over the 200-case eval set
	$(PY) benchmarks/eval/run_eval.py --mode $(MODE) --out $(RESULTS)/eval.json

.PHONY: bench-cache
bench-cache: ## Calibrate the similarity threshold, then measure hit rate and latency
	$(PY) benchmarks/cache/calibrate.py --out $(RESULTS)/cache_calibration.json \
	     --plot docs/diagrams/cache_roc.png
	$(PY) benchmarks/cache/run_cache_bench.py --mode $(MODE) --out $(RESULTS)/cache_bench.json

.PHONY: bench-guardrails
bench-guardrails: ## p99 latency of the input screening pipeline (fails above budget)
	$(PY) benchmarks/guardrails/bench_guardrails.py --out $(RESULTS)/guardrails_latency.json

.PHONY: bench-failover
bench-failover: ## 10,000 streams at a 5% mid-stream failure rate
	$(PY) benchmarks/failover/run_failover.py --mode $(MODE) --out $(RESULTS)/failover.json

.PHONY: bench-attribution
bench-attribution: ## Find runaway workloads in the seeded month of spend
	$(PY) benchmarks/attribution/run_attribution.py --mode $(MODE) --out $(RESULTS)/attribution.json

.PHONY: report
report: ## Print the claim -> measured-number table from benchmarks/results/*.json
	$(PY) benchmarks/report.py

.PHONY: load-test
load-test: ## k6 smoke test against the running gateway
	k6 run --summary-export=$(RESULTS)/k6_summary.json benchmarks/load/k6-smoke.js

# ---------------------------------------------------------------------------
# Tests and quality gates
# ---------------------------------------------------------------------------

.PHONY: test
test: test-go test-py ## Run all unit tests

.PHONY: test-go
test-go: ## Go unit tests with the race detector
	docker run --rm -v "$(PWD)/$(GATEWAY_DIR)":/src -w /src $(GO_IMAGE) \
	  sh -c "apk add --no-cache git gcc musl-dev >/dev/null && go test -race -covermode=atomic -coverprofile=coverage.out ./..."

.PHONY: cover-go
cover-go: ## Enforce the 65% coverage floor on internal/
	@bash scripts/check_go_coverage.sh 65

.PHONY: test-py
test-py: ## Python tests for guardrails, seed and benchmark code
	$(PY) -m pytest -q services/guardrails/tests benchmarks/tests seed/tests

.PHONY: cover-py
cover-py: ## Enforce the 80% coverage floor on the guardrails service
	$(PY) -m pytest -q --cov=services/guardrails/app --cov-fail-under=80 services/guardrails/tests

.PHONY: test-integration
test-integration: ## Wire-compatibility tests against the live stack via the OpenAI SDKs
	$(PY) -m pytest -q tests/integration

.PHONY: lint
lint: lint-go lint-py ## Lint everything

.PHONY: lint-go
lint-go:
	docker run --rm -v "$(PWD)/$(GATEWAY_DIR)":/src -w /src golangci/golangci-lint:v1.62-alpine golangci-lint run

.PHONY: lint-py
lint-py:
	$(PY) -m ruff check services benchmarks seed
	$(PY) -m ruff format --check services benchmarks seed
	$(PY) -m mypy --strict services/guardrails/app

.PHONY: fmt
fmt: ## Format Go and Python sources
	docker run --rm -v "$(PWD)/$(GATEWAY_DIR)":/src -w /src $(GO_IMAGE) \
	  sh -c "go run mvdan.cc/gofumpt@v0.7.0 -l -w ."
	$(PY) -m ruff format services benchmarks seed

.PHONY: size-check
size-check: ## Fail if the repo exceeds 50 MB or any file exceeds 5 MB
	@bash scripts/size_check.sh

.PHONY: secrets-scan
secrets-scan: ## gitleaks scan of the working tree
	docker run --rm -v "$(PWD)":/repo zricethezav/gitleaks:latest detect --source=/repo --config=/repo/.gitleaks.toml --no-git -v

.PHONY: ci
ci: lint test cover-go cover-py size-check bench ## What CI runs

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------

.PHONY: setup
setup: ## Create the Python venv and install dev dependencies
	$(PY) -m venv $(VENV)
	$(VENV)/bin/pip install -U pip
	$(VENV)/bin/pip install -r requirements-dev.txt

.PHONY: clean
clean: ## Remove generated, non-committed artifacts
	rm -rf .pytest_cache .ruff_cache .mypy_cache **/__pycache__ $(GATEWAY_DIR)/coverage.out
