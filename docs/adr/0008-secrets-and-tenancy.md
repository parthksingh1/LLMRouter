# 8. Static bearer tokens for tenants, and why that is not enough

**Status:** Accepted

## Context

Every request needs to be attributed to a tenant: to route by that tenant's policy, enforce its
budget, scope its cache namespace, and bill it. The gateway also holds the upstream provider API
keys, which makes it the highest-value target in the deployment.

## Decision

**Tenant authentication is a static bearer token**, matched against a registry loaded from
`config/tenants.yaml` at start-up. `Authorization: Bearer demo-tenant-a`.

This is what the OpenAI SDKs already send, which matters: wire compatibility means an
application points its base URL at the gateway and changes nothing else. Any other scheme —
mTLS, OIDC, signed requests — breaks that and turns a five-minute integration into a project.

Two details are done properly even though the scheme is simple:

- **Constant-time comparison.** Keys are looked up by SHA-256 digest and then confirmed with
  `subtle.ConstantTimeCompare`. A plain map lookup on the raw key is not timing-safe. The
  practical risk is small; the cost of doing it right is four lines.
- **The raw key never leaves the config layer.** `TenantSpec.Domain()` converts to a
  `domain.Tenant` carrying only a hash, so a tenant can be logged or put on a span safely. A
  test asserts the key does not appear in the domain entity.

**Provider keys come from the environment, never from a file in the repository.** `PROVIDER_MODE
=live` refuses to start if any configured provider's key is unset — failing at boot rather than
on the first request that needs it.

**The Helm chart cannot create the provider-key Secret.** It requires an existing one and fails
to render otherwise. A chart that can create a Secret invites `--set apiKey=...`, which puts the
key into shell history, CI logs and `helm get values` forever.

**Logs are redacted at the handler.** The slog `ReplaceAttr` hook drops any attribute named
`authorization`, `api_key`, `password`, `secret` or `token`, so a careless log line cannot leak
a credential even if someone adds one.

## What this is not

Stated plainly, because a portfolio project claiming production-grade auth would be the
dishonest part:

- **No key rotation.** Changing a key means editing YAML and restarting. There is no overlap
  window, so rotation is a small outage for that tenant.
- **No expiry, no scopes.** A key is all-or-nothing for its tenant and lives forever.
- **No revocation propagation.** Every replica loads the registry at start-up, so revoking a key
  means a rolling restart.
- **Keys are in a config file.** Fine for a demo where they are deliberately obvious
  (`demo-tenant-a`). Not fine anywhere real.
- **No per-request authorisation beyond the model allowlist.** A tenant that can call the gateway
  can call it for anything its allowlist permits.

A real deployment replaces this with OIDC or mTLS at the edge, a secret manager holding tenant
records, and a control plane that can rotate and revoke without a restart. The `TenantStore`
interface is the seam: `Lookup(apiKey) (Tenant, bool)` can be backed by anything.

## Cache isolation

The one tenancy guarantee that *is* structural. Cache keys are namespaced by
`(tenant_id, model_family, system_prompt_hash)`, and the namespace is part of the storage key
rather than a filter applied after retrieval. Tenant B's key cannot name tenant A's entry, so
there is no code path in which the wrong tenant's answer can be read — not merely no path that
does. `TestCrossTenantIsolation` pins it, including asserting that every stored key carries its
tenant.

The Qdrant filter is applied server-side for the same reason: filtering client-side would fetch
the wrong tenant's vectors before discarding them, which is both slower and a leak waiting for a
refactor to expose.

## Revisit when

This is deployed for anyone other than its author. Rotation without a restart is the first thing
to fix, because it is the one whose absence causes an outage rather than merely a risk.
