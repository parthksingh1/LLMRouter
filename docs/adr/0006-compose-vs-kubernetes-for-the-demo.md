# 6. Docker Compose for the demo, Helm for production

**Status:** Accepted

## Context

The project has two audiences with incompatible needs. Someone evaluating it wants to see it
working in five minutes on a laptop. Someone deploying it wants rolling updates, autoscaling,
pod disruption budgets, network policy and per-pod IAM.

## Decision

Ship both, make Compose the documented path, and label the Kubernetes material reference-only —
in `infra/README.md`'s first line and in a table showing exactly which files `make demo` reads.

Compose wins the demo on the things that actually decide whether someone runs it:

- **No cluster.** `kind` or `minikube` is a prerequisite, a download and a class of failure —
  image pull, storage class, a LoadBalancer with no cloud provider behind it — that has nothing
  to do with what the project demonstrates.
- **No registry.** Compose builds and runs from the local daemon. Kubernetes needs images
  somewhere it can pull from, which means a registry or `kind load`, and one more step to get
  wrong.
- **Speed.** Ten containers start in about a minute. A cluster plus a Helm install is several.
- **Legibility.** `docker-compose.yml` is one file a reader can hold in their head. The
  equivalent is a chart, a values file and a dozen templates.

Kubernetes wins production on everything Compose cannot do at all: `maxUnavailable: 0` rollouts,
an HPA on in-flight requests, PDBs, NetworkPolicy, IRSA.

## Consequences

**The duplication is real.** Each service's environment variables are declared in
`docker-compose.yml` and again in `values.yaml`, and they can drift — a knob added to one and
forgotten in the other works locally and fails in the cluster.

The mitigation is that `.env.example` is the single documented list of every knob, and both
files are written against it. That is a convention, not a mechanism, and conventions decay.

A larger project would generate both from one source, or use the same manifests everywhere via
`kompose` or Tilt. At this size that machinery would cost more than the drift it prevents. The
honest position is that this is a known trade with a known failure mode, not that it is free.

**The Helm chart is not exercised by CI.** It is linted and templated, never applied to a real
cluster. It could be wrong in ways lint does not catch. Labelling it reference-only is what
makes that acceptable; presenting it as production-ready would not be.

## Alternatives considered

**Kubernetes only, via `kind`.** One deployment story, no drift. Rejected: it makes the
five-minute demo a twenty-minute demo with more ways to fail, and the first impression of a
portfolio project is the demo working.

**Compose only.** No Kubernetes material at all. Rejected because production concerns —
graceful SSE draining across rollouts, autoscaling on the right signal — are a large part of
what makes this project interesting, and Compose cannot express them.

**Compose plus a plain manifest directory.** No Helm, just YAML. Simpler, and loses the
templating that makes a chart configurable per environment, which is most of the point of
shipping one.

## Revisit when

Someone needs to deploy this for real. At that point the chart stops being reference material,
CI grows a `kind` job that actually applies it, and the drift between the two files needs a
mechanism rather than a convention.
