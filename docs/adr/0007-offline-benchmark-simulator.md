# 7. An offline routing simulator for the benchmarks

**Status:** Accepted

## Context

The benchmarks have to reproduce on a reviewer's machine. If reproducing a number requires
Docker, a running stack and ten minutes of warm-up, most readers will take the README on faith —
which is the opposite of what a claims-with-evidence project is for.

But the routing logic lives in Go, in the gateway. Measuring cost savings means routing 200
cases, and routing requires a router.

## Decision

`benchmarks/common/simulator.py` reimplements the routing decision in Python, and every
benchmark runs in one of two modes:

- `MODE=offline` — the simulator. Needs only Python. The default, and what the committed results
  were produced with.
- `MODE=gateway` — drives the live stack over HTTP. What CI's integration job runs.

**This is the one place in the repository where logic is deliberately written twice, and that is
a real risk.** A simulator that drifts from the router turns an honest benchmark into a
flattering one, silently, in the direction nobody checks.

Three things keep it honest:

**1. It reads the same configuration.** Weights, thresholds, quality floors, the safety margin,
the confidence band and every price come from `config/policies.yaml` and
`config/providers.yaml` — the same files the Go router parses. Only the ~60 lines of arithmetic
that combine them exist twice. Change a price and both move together.

**2. `benchmarks/compare_modes.py` fails the build on divergence.** CI's integration job runs
the eval in both modes and asserts the cost saving agrees within a small tolerance. The
simulator cannot drift far without someone finding out.

**3. Every results file records its mode.** A reader always knows whether a number came from a
model of the router or the router.

The simulator implements only `cost_optimized`, `latency_optimized` and `quality_tiered`. It
raises `NotImplementedError` for `weighted_round_robin` and `canary` rather than approximating
them: both are stateful, and a simulator that guessed at their behaviour would produce a number
that looks like a measurement and is not.

## Consequences

`make bench` runs on any machine with Python 3.11 and no other dependency. That is the point:
a reviewer can check a claim without believing anything first.

The maintenance burden is real. A change to `internal/router` that is not mirrored here shows up
as a `compare_modes` failure — which is the mechanism working — but it is still two places to
edit. This decision would not survive the routing logic getting much more complex.

The divergence check is only as good as its tolerance, and it currently compares one aggregate
(cost saving) rather than per-case decisions. A simulator that made compensating errors could
pass. Comparing per-case model choices would be stricter and is the obvious improvement.

## Alternatives considered

**Compile the Go router to WASM and call it from Python.** No duplication at all, and genuinely
appealing. It adds a build step, a toolchain and a binary artefact to the benchmark path — which
is exactly the friction this decision exists to remove.

**Gateway mode only.** No duplication, and no reproducibility without Docker. Rejected: it makes
every number take a reviewer's trust rather than their machine.

**A thin Go binary that exposes routing decisions over stdin/stdout.** The middle ground: no
reimplementation, and Python drives it. It still needs a Go toolchain or a prebuilt binary at
benchmark time. This is the strongest alternative and the one to take if the simulator ever
becomes a maintenance problem.

## Revisit when

`compare_modes.py` fails for a reason that is not an obvious oversight, or when the routing
logic grows past what sixty lines of Python can mirror. Either is the signal to switch to the
subprocess approach.
