# Security Policy

## Scope and posture

LLMRouter sits on the request path between applications and LLM providers. It therefore
handles tenant API keys, prompt content (which routinely contains PII), and provider
credentials. The threat model and the controls that address it:

| Threat | Control | Where |
|---|---|---|
| Prompt injection reaching a provider or a downstream tool | Input screening, OWASP `LLM01` detectors | `services/guardrails/app/detectors/prompt_injection.py` |
| PII leaving the perimeter in a prompt | Redaction by default, `block` available per policy (`LLM06`) | `services/guardrails/app/detectors/pii.py` |
| Secrets pasted into prompts | High-entropy and vendor-prefix detectors (`LLM06`) | `services/guardrails/app/detectors/secrets.py` |
| Cross-tenant cache poisoning or leakage | Cache keys are namespaced by `(tenant_id, model_family, system_prompt_hash)`; enforced by an explicit isolation test | `services/gateway/internal/cache` |
| Runaway spend / denial-of-wallet | Redis token bucket per tenant per day, atomic via Lua; 429 with `Retry-After` at 100% | `services/gateway/internal/budget` |
| Credential leakage through logs | Redacting log middleware; `Authorization` and every `*_API_KEY` are never serialised | `services/gateway/internal/http/middleware` |
| Committed secrets | `gitleaks` in pre-commit and in CI | `.gitleaks.toml` |

## What this project is not

This is a portfolio-grade reference implementation. Before production use you would need, at
minimum: real authentication (mTLS or OIDC rather than static bearer tokens), a secret manager
in place of `config/tenants.yaml`, a trained prompt-injection classifier rather than the
demo rule set, and per-tenant rate limiting at the edge.

## Reporting a vulnerability

Open a GitHub security advisory, or email the maintainer. Please do not open a public issue
for anything exploitable. Expect an acknowledgement within 5 working days.

## Demo credentials

Every credential in this repository (`demo-tenant-a`, Grafana `admin/admin`, the blank
ClickHouse password) exists only for the offline demo stack and is deliberately obvious.
None of them grant access to anything real.
