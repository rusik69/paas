# ADR 0001 — Generated CRDs as the store, not an aggregated API server

- **Status:** proposed
- **Date:** 2026-08-09

## Context

Tenants order managed services as typed Kubernetes objects (`Postgres`, `Kafka`, `Bucket`).
We want to add a new service by adding a Helm chart, without writing Go types or recompiling
a binary. Something has to turn a typed, tenant-facing object into a Helm release.

The established approach for this problem is an
[aggregated API server](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/):
a custom server built on `k8s.io/apiserver` registers an API group via an `APIService`, and
translates each request into an operation on the underlying Flux `HelmRelease`. The
HelmRelease becomes the storage — the custom server keeps no etcd of its own — and types are
defined dynamically from chart metadata, so a new chart really does mean a new API kind with
no recompile.

That design buys real things: exactly one copy of the state, and a natural home for imperative
verbs and custom subresources that CRDs cannot express.

It also costs real things. A storage-backed aggregated API server is one of the more demanding
pieces of Kubernetes extension machinery: implementing `rest.Storage` correctly across
list/watch/patch semantics, resource versions and consistent watch bookmarks, field and label
selectors, table conversion for `kubectl get`, dry-run, RBAC delegation and
SubjectAccessReview, serving certificates and CA rotation, and highly-available deployment —
because when it is down, `kubectl get postgres` fails cluster-wide. It is also the single most
load-bearing component in the system: every read and every write on every tenant object flows
through it.

## Decision

**Generated `CustomResourceDefinition`s are the source of truth. The HelmRelease is derived
output.**

A cluster-scoped `ServiceClass` names a chart and points at its `values.schema.json`.
`paas-controller` reconciles it into a real CRD whose OpenAPI structural schema *is* that
JSON schema, plus a dynamic reconciler that renders each CR into a server-side-applied
`HelmRelease` in the same namespace, owner-referenced to the CR, and copies status back.

This keeps the "new chart, no recompile" property — the CRD is generated at runtime from the
chart's schema — while letting the real API server do the storage.

**Imperative verbs move to a separate, small aggregated API server** (`paas-apiserver`)
serving only subresources: tenant-scoped `pods/logs`, `App/restart`,
`VirtualMachine/console`, `Postgres/backup` and `/restore`, `Build/logs`. It holds no state
and is not on the CRUD path.

## Consequences

**Good**

- `kubectl get/describe/explain/edit`, watch, label and field selectors, `--dry-run=server`,
  server-side apply, and RBAC all work because they are the upstream implementations, not
  ours.
- Admission webhooks and OPA/Kyverno policies apply to tenant objects natively.
- One writable source of truth per object. The HelmRelease is an implementation detail.
- The blast radius of `paas-apiserver` going down is "no VM console, no log tailing" rather
  than "the platform API is gone".
- Substantially less code to write and, more importantly, to keep correct across Kubernetes
  releases.

**Bad**

- **State is duplicated**: the CR spec and the HelmRelease values hold the same data. This is
  the main cost of the decision. Mitigation: the HelmRelease is machine-owned, written with
  server-side apply under a dedicated field manager, so drift is overwritten on the next
  reconcile; RBAC denies tenants write access to derived HelmReleases; and reconcile-drift is
  a monitored metric, not a silent condition.
- etcd holds a copy of every tenant object. At our target density this is small, but it is
  not free and should be watched.
- CRD schema changes require a CRD update, so chart `values.schema.json` changes must be
  backward-compatible or versioned. Removing a field from a schema is a breaking API change
  and must be treated as one.
- Two components now serve API traffic instead of one, which is more surface to secure even
  though each is simpler.

## Alternatives rejected

**Full aggregated API server with HelmRelease as the store.** Rejected for the implementation
and operational burden described above, not because it is wrong — it is a good fit for a team
that already has that expertise and wants exactly one copy of the state. Revisit if we find
ourselves fighting the state-duplication problem more than expected.

**Hand-written Go types with generated deepcopy, one per service.** Simple and type-safe, but
every new managed service becomes a code change, a release, and a rollout. That directly
contradicts the goal of shipping a service by adding a chart.

**No typed layer — tenants write HelmReleases directly.** Rejected outright. It exposes
arbitrary chart values, which destroys the security boundary that `values.schema.json`
provides, and gives the dashboard nothing to render a catalog from.
