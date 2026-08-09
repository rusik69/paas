# ADR 0004 — Nested tenants with module inheritance

- **Status:** proposed
- **Date:** 2026-08-09

## Context

Every tenant needs supporting infrastructure that is not the thing they bought: an ingress
controller, a monitoring stack, object storage behind their `Bucket` claims, sometimes an
etcd. A Prometheus, a Grafana, an ingress controller, and a SeaweedFS cluster per tenant is
several gigabytes of memory and a dozen pods before the tenant has deployed anything at all.
At any interesting tenant count that is the dominant cost of the platform, and it is spent on
overhead rather than on workload.

Real customers are also not flat. A company wants separate environments (`prod`, `staging`,
`dev`), or separate teams, each isolated from the others but administered together and billed
together. Modelling that as unrelated top-level tenants loses the relationship and duplicates
the overhead per environment.

The answer that fits both problems at once is hierarchical tenants: a tenant is a namespace,
tenants nest, a parent enables shared infrastructure once, and children inherit it. A child
without its own monitoring sends its metrics to the parent's stack, and the overhead is paid
per subtree rather than per tenant.

## Decision

**Tenants form a tree. Modules are enabled at a node and inherited by its descendants.**

- A `Tenant` reconciles to a namespace whose name is derived from its path: a child `beta`
  under `acme` becomes `tenant-acme-beta`. Names are capped at 63 characters and the
  reconciler rejects anything longer rather than truncating.
- `spec.modules` enables tenant-level infrastructure — `ingress`, `monitoring`, `seaweedfs`,
  `etcd`.
- **Resolution walks up the ancestor chain.** A workload's effective module is the one from
  the nearest ancestor that has it enabled. `enabled: false` on a child is not a denial; it
  means "use my parent's".
- Isolation does **not** inherit. Every tenant namespace gets its own default-deny
  `CiliumNetworkPolicy` and its own `ResourceQuota` regardless of depth. A child is not
  trusted because its parent is.
- Administration flows down: a parent's `tenant-admin` can administer descendants. It does not
  flow up.
- Billing rolls up to the nearest node that has a plan attached.

## Consequences

**Good**

- One SeaweedFS cluster serves `Bucket` claims across an entire subtree; one Prometheus covers
  an org's environments. Overhead is paid per subtree, not per tenant, which is what makes
  dense multi-tenancy economically viable.
- The tree maps directly onto how customers actually think — org → environment → team — so
  `prod` and `staging` can be genuinely isolated without doubling fixed cost.
- Creating a child tenant is cheap, so tenants create them freely, which is the behaviour we
  want.
- Delegated administration falls out of the structure instead of needing a separate
  permissions model.

**Bad**

- **Resolution is a distributed lookup, and that is the main hazard.** Every controller that
  needs "which monitoring stack serves this namespace" must walk ancestors. This belongs in
  exactly one place — a `pkg/tenancy` resolver — and must be cached and invalidated on parent
  changes. Reimplementing the walk per controller will produce subtle disagreements about
  where a workload's metrics go.
- **A shared module is a shared blast radius.** A parent's Prometheus falling over blinds
  every descendant. Shared infrastructure needs per-child resource limits (metric cardinality
  caps, storage quotas) or one noisy child degrades its siblings.
- Path-derived names hit the 63-character ceiling at depth. Enforcing it at admission is
  mandatory; discovering it at namespace-creation time is a bad failure.
- **Moving a tenant between parents is not supported.** It would change the namespace name,
  and therefore every object's location. Treat re-parenting as a migration, and say so in the
  docs before someone asks.
- Deletion must cascade correctly. Deleting a parent has to reclaim descendants and their
  managed services, and the e2e test asserting full garbage collection is not optional.

## Alternatives rejected

**Flat tenants, no hierarchy.** Much simpler resolution — no ancestor walk, no shared blast
radius. Rejected on economics: per-tenant monitoring and object storage put a floor under cost
per tenant that makes small tenants unprofitable, and it forces customers to model
environments as unrelated accounts.

**Hierarchical Namespace Controller (HNC).** Solves namespace trees and policy propagation as
a general upstream mechanism. Rejected because our hierarchy carries platform semantics —
module resolution, plan and billing rollup, quota derivation — that we would end up
implementing on top of HNC anyway, leaving two overlapping sources of truth about the tree.

**Control plane per tenant (Kamaji, vcluster).** Strong isolation, tenants can install their
own CRDs and operators, and no ancestor-walk problem. Rejected for now on cost and operational
surface: it is a per-tenant control plane to run, upgrade, back up, and monitor. It remains
the right answer for a future dedicated tier and for the Kubernetes-as-a-Service product, and
nothing in this ADR precludes adding it.

**Namespace-per-tenant with dedicated node pools for everyone.** Stronger compute isolation,
but it defeats dense packing and leaves capacity stranded per tenant. Kept as the
`isolation: dedicated-nodes` option for compliance-sensitive customers rather than the
default.
