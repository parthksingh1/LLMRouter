# infra/ — reference implementations

**None of the Kubernetes or cloud material here is required to run the demo.** `make demo` uses
`docker-compose.yml` at the repository root and, from this directory, only the files marked
below.

| Path | Used by `make demo` | What it is |
|---|---|---|
| `clickhouse/init.sql` | **yes** | Schema and rollups, applied on container start |
| `otel-collector-config.yaml` | **yes** | Collector pipelines |
| `prometheus/` | **yes** | Scrape config and alert rules |
| `grafana/` | **yes** | Datasources and the generated dashboard |
| `helm/` | no | Kubernetes deployment, reference only |
| `terraform/` | no | AWS infrastructure, reference only |

## Why Compose for the demo and Helm for production

They optimise for different things, and making one file do both makes it bad at both.

The demo has to start on a laptop in under five minutes with one command, no cluster, no
registry and no cloud account. Compose does that. A `kind` cluster plus a Helm install would add
several minutes, a registry step, and a whole class of failure — image pull, storage class, a
LoadBalancer with no cloud provider behind it — that has nothing to do with what the project is
demonstrating.

Production needs rolling updates, autoscaling, pod disruption budgets, network policy and
per-pod IAM. Compose has none of those.

The duplication is real and worth naming rather than hiding: the environment variables each
service takes are declared in both `docker-compose.yml` and `values.yaml`, and they can drift.
The mitigation is that `.env.example` is the single documented list and both are written against
it. A larger project would generate both from one source; at this size that machinery would cost
more than the drift it prevents. Argued in
[ADR-0006](../docs/adr/0006-compose-vs-kubernetes-for-the-demo.md).

## Helm chart

```bash
helm lint infra/helm/llmrouter
helm template llmrouter infra/helm/llmrouter --set gateway.existingSecret=llmrouter-provider-keys
```

Details worth reading rather than skimming:

- **The chart cannot create the provider-key Secret.** It requires one to exist and fails to
  render otherwise. A chart that can create a Secret invites `--set apiKey=...`, which puts the
  key into shell history, CI logs, and `helm get values` forever.
- **Liveness probes `/healthz`; readiness probes `/readyz`.** `/healthz` deliberately does not
  consult providers. A liveness check that fails during an upstream outage restarts every pod at
  exactly the moment the failover logic is doing its job.
- **`terminationGracePeriodSeconds: 120`, `maxUnavailable: 0`.** SSE streams run for minutes. A
  pod being replaced still has to finish what it is serving, or every deploy truncates
  somebody's answer mid-sentence.
- **The HPA scales on in-flight requests, not CPU.** The gateway spends most of its life blocked
  on an upstream, so CPU stays low while concurrency — the thing that actually saturates it —
  climbs. CPU-based autoscaling would not react until far too late.
- **`preStop: sleep 5`.** Removes the endpoint from every kube-proxy before the process stops
  accepting, so a rollout does not drop the requests arriving in that gap.
- **NetworkPolicy ships with open egress on 443, and says so.** That is the first rule to narrow
  before anyone calls this production-ready: the gateway holds every tenant's API keys and sees
  every prompt, so unrestricted egress from that pod is the worst thing in the deployment.

## Terraform

```bash
cd infra/terraform
terraform init
terraform plan -var environment=dev
```

**This provisions billable resources** — EKS, ElastiCache, NAT gateways, EC2 — and takes about
twenty minutes. Do not run it to look at the demo.

Decisions a reviewer should push on, each argued in the comments rather than asserted:

- **Two AZs, not three.** The gateway is stateless and the stateful services are managed, so a
  third AZ buys availability this architecture cannot use while tripling cross-AZ transfer —
  which, for a service moving large payloads, is a real line on the bill rather than a rounding
  error.
- **One NAT gateway by default.** A single point of failure for egress, and the right trade at
  this size. `var.nat_per_az` flips it when it stops being right.
- **ClickHouse on EC2.** AWS has no managed ClickHouse. The alternatives are ClickHouse Cloud
  (excellent, but a second vendor and a second bill) or self-hosting. The module takes the honest
  starting point and makes the upgrade path explicit rather than pretending the problem is not
  there.
- **Redis with automatic failover, not a single node.** The budget counters must survive a node
  loss: losing them mid-day resets every tenant's spend to zero, which is a billing incident
  rather than a cache miss.
- **Secret values are not managed by Terraform.** The resource creates the container; something
  with narrower permissions fills it. A secret in Terraform is a secret in state, and state is a
  file that gets copied.
