## What and why

<!-- One paragraph. What changes, and what problem it solves. -->

## Checklist

- [ ] Conventional Commit title (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, `perf:`)
- [ ] `make lint` passes
- [ ] `make test` passes; new behaviour has table-driven tests
- [ ] Coverage floors hold (Go 65% on `internal/`, Python 80% on guardrails)
- [ ] `make size-check` passes (no file > 5 MB, repo < 50 MB)
- [ ] No secrets committed (`make secrets-scan`)
- [ ] `.env.example` updated if a config knob was added
- [ ] An ADR added under `docs/adr/` if this changes an architectural decision

## Benchmark impact

- [ ] This change cannot move any number in `benchmarks/results/`
- [ ] ...or: `make bench` was re-run and the updated JSON is in this PR

<!-- If numbers moved, paste the before/after here. -->

## How to verify

<!-- Exact commands a reviewer can run. -->
