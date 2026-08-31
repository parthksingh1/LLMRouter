# OWASP Top 10 for LLM Applications — what LLMRouter does and does not address

Every guardrails finding carries an OWASP category, and `GET /v1/policies` publishes the mapping
so it can be checked rather than believed. This document is the long form, including the
categories a gateway is the wrong place to address — those are more useful to a reviewer than
the ones it covers.

Reference: [OWASP Top 10 for LLM Applications](https://owasp.org/www-project-top-10-for-large-language-model-applications/).

| Category | Addressed | Where | Notes |
|---|---|---|---|
| **LLM01 Prompt Injection** | Partially | `detectors/prompt_injection.py` | Rule set, not a model. Catches known surface forms; **not adversarially robust**. |
| **LLM02 Insecure Output Handling** | Partially | `detectors/toxicity.py`, output screening | Output screening is opt-in. Escaping is the caller's job — see below. |
| **LLM03 Training Data Poisoning** | No | — | A gateway never touches training data. |
| **LLM04 Model Denial of Service** | Yes | `internal/budget`, request caps | Per-tenant token buckets, body size cap, request timeouts. |
| **LLM05 Supply Chain** | Partially | CI | Pinned dependencies, gitleaks, image scanning. Not model provenance. |
| **LLM06 Sensitive Information Disclosure** | Yes | `detectors/pii.py`, `detectors/secrets.py` | The strongest coverage here. |
| **LLM07 Insecure Plugin Design** | No | — | LLMRouter does not execute tools; it forwards tool definitions. |
| **LLM08 Excessive Agency** | Partially | `prompt_injection:exfiltration` | Detects instructions to send data outbound. |
| **LLM09 Overreliance** | Partially | routing metadata | Every response reports which model served it and at what cost. |
| **LLM10 Model Theft** | No | — | Provider-side concern. |

---

## LLM01 — Prompt Injection

**What is implemented.** A curated rule set covering the recurring surface forms:

| Rule group | Catches | Default action |
|---|---|---|
| `instruction_override` | "ignore all previous instructions", "disregard the above" | block (high) |
| `role_reassignment` | "you are now DAN", "enter developer mode", "pretend you have no restrictions" | block (high) |
| `system_prompt_extraction` | "repeat your system prompt", "show the initial instructions" | block (high) |
| `delimiter_injection` | `### system:`, `<|im_start|>`, `[INST]` | flag (medium) / block (high) |
| `encoded_payload` | "base64 decode this", long base64 runs | flag (medium/low) |
| `refusal_suppression` | "do not refuse", "without any disclaimer" | flag (medium) |

**What is not.** This is a rule set, and anyone who knows the rules exist can write around them.
It will not catch a novel phrasing, a non-English attack, or an obfuscated payload. It is a
speed bump and a signal source, not a boundary.

**What a production deployment should do instead.** Replace the rule set with a trained
classifier behind the same `scan()` interface — the service contract does not change. A rule set
ships here because a classifier is a model artefact needing download, versioning and a GPU
budget, which would make `make demo` depend on all three. Argued in
[ADR-0005](adr/0005-guardrail-sidecar-vs-library.md).

**Defence in depth matters more than the detector.** Injection is not solvable by input
filtering. The controls that actually contain it are architectural: least-privilege tool access,
human confirmation for consequential actions, and treating model output as untrusted input.
LLMRouter provides the last of these by making output screening available.

---

## LLM02 — Insecure Output Handling

Output screening (`POST /v1/screen/output`) runs the same detectors over the completion, because
the risks are symmetric: a model can echo back the PII it was given, or emit markup a downstream
renderer will execute.

It is **off by default** (`GUARDRAILS_SCREEN_OUTPUT=false`) because it doubles the guardrails
latency cost and not every deployment wants to pay it.

**The gateway cannot fix insecure output handling.** Escaping is contextual — what is safe in
JSON is not safe in HTML, and neither is safe in a shell. A gateway does not know where the
output is going, so it cannot escape correctly. Treat every completion as untrusted user input
at the point of use.

---

## LLM04 — Model Denial of Service

The one category a gateway is genuinely the right place for, because it is the only component
that sees all of a tenant's traffic:

- **Per-tenant token budgets**, enforced with an atomic Redis Lua script (`internal/budget`), so
  concurrent requests cannot race past the limit.
- **Warning at 80%, refusal at 100%**, returned as a `429` with `Retry-After` so SDK retry logic
  behaves.
- **Request body cap** of 8 MiB and per-request timeouts.
- **Circuit breakers** per provider, so one struggling upstream cannot consume every worker.

Denial-of-wallet is the variant that matters commercially, and the attribution pipeline is what
makes it visible: `benchmarks/results/attribution.json` finds the runaway workloads.

---

## LLM06 — Sensitive Information Disclosure

The strongest coverage, and the reason the service exists.

**PII** (`detectors/pii.py`) — emails, phone numbers, SSNs, payment cards, IBANs and public IP
addresses. Cards are Luhn-checked and IBANs mod-97-checked, which is what makes the patterns
usable rather than noise. Redacted by default; `strict` blocks instead.

*Known gap:* there is no NER model, so a person's name in free text is not detected. Stated here
rather than papered over: an operator can compensate for a known gap, not for an unknown one. A
production deployment would add Presidio or an equivalent.

**Secrets** (`detectors/secrets.py`) — vendor-prefix patterns (OpenAI, Anthropic, AWS, GitHub,
GitLab, Slack, Stripe, Google, JWTs, private key blocks, connection strings) plus a
Shannon-entropy check for unprefixed credentials. The entropy check excludes UUIDs, hex digests
and single-character-class strings, without which it fires on every request id in every
debugging prompt and gets switched off within a day.

Secrets **block** rather than redact: forwarding a prompt with the credential removed still
forwards the surrounding context, and the user needs to know it was rejected so they can rotate.

**Findings never carry the matched text.** They carry offsets and a truncated SHA-256 of the
span. Findings travel to the gateway, into its logs and into the analytics event; a finding that
carried the SSN would copy it to three more places. Tested explicitly.

---

## LLM08 — Excessive Agency

The `exfiltration` rules detect instructions telling the model to send data outbound — a URL
POST, a webhook, or a markdown image whose URL interpolates conversation content (the classic
zero-click exfiltration via a rendered image).

Detection is all a gateway can offer. Genuine mitigation is scoping the model's tools, which
happens in the application.

---

## Categories deliberately not addressed

**LLM03 Training Data Poisoning** and **LLM10 Model Theft** are properties of model training and
hosting. A gateway sits in front of an already-trained model.

**LLM05 Supply Chain** is partially addressed for *this* project's own supply chain — pinned
dependencies, `gitleaks` in pre-commit and CI, and non-root minimal images — but LLMRouter has
no view into a provider's model provenance.

**LLM07 Insecure Plugin Design** belongs to whatever executes tools. LLMRouter forwards tool
definitions and tool calls without executing anything, which is deliberate: a gateway that
executed tools would be a far larger attack surface.

**LLM09 Overreliance** is a product concern, but the gateway helps by making the answer's
provenance visible — every response reports the serving provider, the resolved model, whether it
came from cache, and its cost, so an application can surface "this was answered by a small
model" to its users.

---

## Verifying the mapping

```bash
curl -s http://localhost:8000/v1/policies | jq .owasp_coverage
```

The mapping is generated from `app/policies/owasp.py`, so it cannot drift from the code. The
per-category behaviour is pinned by tests in `services/guardrails/tests/`.
