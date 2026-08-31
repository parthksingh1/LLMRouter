# 5. Guardrails as a sidecar, failing open by default

**Status:** Accepted

## Context

Prompts need screening for PII, injection attempts and pasted credentials. The gateway is Go.
The detection ecosystem — Presidio, Detoxify, every injection classifier worth using — is
Python. And this runs on every single request.

Two decisions: where the code lives, and what happens when it is unavailable.

## Decision, part 1: a sidecar

A separate FastAPI service, called over HTTP.

A Go library would be faster by one loopback hop, and would mean reimplementing every detector
in Go and abandoning the ecosystem the moment a real classifier is wanted. The rule set that
ships here *could* be Go; the trained model that should replace it could not.

The separation buys three things beyond language choice:

- **Independent scaling.** Screening is CPU-bound, the gateway is network-bound. They saturate
  differently and should scale differently.
- **Independent deployment.** A detector rule can ship without redeploying the request path.
- **A blast radius.** A detector that leaks memory or hangs takes down a sidecar, not the
  gateway.

The measured cost of the hop: p99 **1.78 ms** in-process, **7.66 ms** through the ASGI stack,
against a 12 ms budget. The hop is affordable, and the budget is enforced in CI rather than
asserted in a README.

## Decision, part 2: fail open by default

When the sidecar is unreachable the request proceeds unscreened. `GUARDRAILS_FAIL_MODE=closed`
inverts it.

This is the decision most worth arguing with, so here is the argument.

**Fail closed** means a guardrails outage is a total gateway outage. Every LLM request in the
product stops. The blast radius of a sidecar restart becomes the blast radius of the whole
platform, and the component with the least operational maturity gains veto power over
everything else.

**Fail open** means a screening gap. For a window measured in seconds, prompts reach providers
unscreened. Nobody is attacked by the outage itself; the risk is that an attack coincides with
it.

For a general developer platform, fail open is right: the availability loss is certain and
immediate, the security loss is probabilistic and bounded. For a regulated deployment, where
sending unscreened data is itself the violation, fail closed is right — hence the flag rather
than a hard-coded answer.

What makes this defensible rather than lazy is that the failure is never silent:

- `llmrouter_guardrail_fail_open_total` increments, and an alert fires on any non-zero rate.
- A WARN log line names the tenant.
- The span carries `llmrouter.guardrail_failed_open=true`, so it is searchable in Jaeger.
- `ScreenResult.FailedOpen` reaches the analytics event.

An unscreened request is a security event even when allowing it was the correct trade, and it
has to be findable afterwards.

## Decision, part 3: a rule set, not a model

The injection detector is curated regexes. It catches the known surface forms and will not catch
a novel or obfuscated attack. It is a speed bump and a signal source, not a boundary.

The reason is `make demo`: a trained classifier is a model artefact needing download, versioning
and a GPU budget. The `scan()` interface does not change when one is swapped in — only what sits
behind it.

The module docstring, the README and `docs/owasp-llm-mapping.md` all state this limitation,
because the characteristic failure mode of a security control is people trusting it more than it
deserves.

## Consequences

Injection screening is weak. PII and secret detection are genuinely useful — regex plus
checksums is the right tool for structured identifiers, and Luhn and IBAN mod-97 are what make
those patterns usable rather than noise.

Toxicity ships **off**. A keyword filter has a high false-positive rate on exactly the security
and incident traffic a developer platform carries, and a demo that blocks legitimate prompts
gets the whole service switched off — losing the PII and secret detection that does work.

Findings never carry the matched text, only offsets and a truncated hash. Findings travel to the
gateway, into its logs and into the analytics event; a finding carrying the SSN would copy it to
three more places, which is precisely what this service exists to prevent.

## Revisit when

A trained injection classifier is available, or when the p99 budget is missed — at which point
the choice is optimising the detectors or moving them in-process and accepting the ecosystem
cost.
