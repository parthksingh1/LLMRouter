# Contributing

## Getting set up

```bash
make setup          # python venv + pinned dev deps
pre-commit install  # format, lint, gitleaks, size check
make demo           # the whole offline stack
```

A local Go toolchain is optional: `make test-go`, `make lint-go` and `make fmt` all run inside
containers. Install Go 1.23+ if you want a faster inner loop.

## Ground rules

**Architecture.** The gateway is hexagonal. `internal/domain` holds entities and has no
dependencies. `internal/app` holds use cases and defines the ports it needs (`Provider`,
`SemanticCache`, `Guardrails`, `Budget`, `EventSink`). Everything under `internal/providers`,
`internal/cache`, `internal/guardrails`, `internal/budget` and `internal/events` is an adapter
implementing one of those ports. Adapters never import each other, and nothing imports
`internal/http` except `cmd/`.

**Go.** `context.Context` is the first argument of anything that can block. Wrap errors with
`fmt.Errorf("doing thing: %w", err)`. No global mutable state, no `panic` outside `main`.
Tests are table-driven. `gofumpt` and `golangci-lint` are enforced.

**Python.** Fully typed; `mypy --strict` must pass on `services/guardrails/app`. Pydantic v2
models at every boundary. FastAPI dependency injection rather than module-level singletons.
No bare `except:`.

**Configuration.** Every knob is an environment variable, documented in `.env.example`. Config
is read once at startup into a typed struct and injected downward -- never read `os.Getenv`
from inside a handler.

**Secrets.** Never log a credential. `gitleaks` runs in pre-commit and CI. `.env` is ignored.

## Tests

| Gate | Command | Floor |
|---|---|---|
| Go unit, race detector | `make test-go` | 65% on `internal/` |
| Python unit | `make test-py` | 80% on `services/guardrails/app` |
| Lint | `make lint` | clean |
| Wire compatibility | `make test-integration` | OpenAI Python + Node SDKs pass |
| Repo size | `make size-check` | < 50 MB total, < 5 MB per file |

## Benchmarks and honesty

Numbers in `README.md` are rendered from `benchmarks/results/*.json`, and those files are
produced only by `make bench`. **Never edit a results file by hand.** If a change moves a
number, re-run the benchmark and commit the new JSON along with the change that moved it. If a
seeded number needs to land somewhere specific, adjust the *seed generator*, never the report.

## Commits and PRs

Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, `perf:`).
Work on a feature branch. PRs use `.github/pull_request_template.md`; the checklist is not
decorative -- CI enforces most of it.
