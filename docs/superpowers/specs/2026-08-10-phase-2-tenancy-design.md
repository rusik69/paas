# Phase 2, part 1 — the `Tenant` tree, quotas, and the resolver

- **Status:** proposed
- **Date:** 2026-08-10
- **Covers:** roadmap phase 2, bullets 1, 2 and 5 — the namespace tree with its 63-character
  guard, `ResourceQuota`/`LimitRange` from `spec.plan`, and module resolution up the ancestor
  chain
- **Defers:** `CiliumNetworkPolicy` isolation and the OIDC/RBAC work, each to its own spec

## Decided by ADR 0004, not here

[ADR 0004](../../adr/0004-tenant-hierarchy-and-inheritance.md) settles the shape, and this spec
implements it rather than revisiting it:

- Tenants form a tree; a `Tenant` reconciles to a namespace named from its path.
- Modules are enabled at a node and inherited by descendants. `enabled: false` on a child means
  "use my parent's", not "denied".
- **Isolation does not inherit.** Every tenant gets its own quota and its own default-deny
  policy regardless of depth.
- Re-parenting is unsupported — it would rename the namespace and move every object.
- The ancestor walk lives in exactly one place, `pkg/tenancy`, because reimplementing it per
  controller produces subtle disagreements about where a workload's metrics go.

## A restructure this forces

`api/v1alpha1` currently holds the `platform.paas.io` group. `Tenant` is in `core.paas.io`, and
controller-gen takes one `+groupName` per Go package, so a second group needs a second package.

`api/` therefore becomes one package per group:

```
api/platform/v1alpha1/   platform.paas.io — Platform, PackageSource, Package
api/core/v1alpha1/       core.paas.io     — Tenant
api/apps/v1alpha1/       apps.paas.io     — phase 3 onward
```

architecture.md §10, go-guidelines and AGENTS.md all describe a flat `api/v1alpha1/` holding
every kind. That is not implementable with one group marker per package, so those documents are
corrected as part of this work. Doing it now costs three import rewrites; doing it when
`apps.paas.io` arrives costs the same rewrite plus whatever has been built on the wrong shape.

## The `Tenant` API

Namespaced, not cluster-scoped: a `Tenant` lives in its parent's namespace, and that is how the
tree is expressed. Root tenants live in `tenant-root`.

```yaml
apiVersion: core.paas.io/v1alpha1
kind: Tenant
metadata: {name: acme, namespace: tenant-root}
spec:
  plan: business                  # drives quota and limits
  isolation: shared               # shared | dedicated-nodes
  host: acme.paas.example         # optional until phase 5 routing
  modules:
    monitoring: {enabled: true}
    seaweedfs:  {enabled: false}  # resolves to the nearest ancestor that has one
  admins: [alice@acme.com]
status:
  namespace: tenant-acme          # what the path resolved to
  observedGeneration: 3
  conditions: []                  # Ready, Degraded
```

`spec.plan` and `spec.isolation` are enums. `spec.modules` is a map of named modules to
`{enabled: bool}` rather than a list, so a child overriding one module cannot accidentally
replace the whole set under server-side apply.

**Immutability.** `spec.isolation` is immutable via CEL: changing it would move every workload
between node pools, which is a migration. The tenant's identity — its name and its namespace —
is already immutable because Kubernetes does not permit renaming either, so no rule is needed
and none is written.

## `pkg/tenancy`

The only place the tree is interpreted. Public because a tenant writing their own operator
needs to answer the same questions.

- `NamespaceFor(path []string) (string, error)` — joins with the `tenant-` prefix, rejects over
  63 characters rather than truncating, and rejects segments that are not DNS labels.
- `PathOf(t *Tenant) []string` — derives the path from the chain of namespaces.
- `Resolve(ctx, reader, tenant, module) (*Tenant, bool, error)` — the ancestor walk, returning
  the nearest ancestor with the module enabled.

The walk is deliberately a plain function over a `client.Reader` rather than a caching service.
ADR 0004 calls for caching and invalidation, and that is real, but a cache whose invalidation
is wrong is worse than an uncached walk at phase-2 depths — a tree three deep is three Gets.
Caching arrives with a measurement showing it is needed.

## The reconciler

`Tenant` → Namespace, `ResourceQuota`, `LimitRange`. Server-side apply under the field manager
`paas-operator/tenant`, frozen from its first commit.

Plans are a table in code, not configuration: phase 2 has three plans and inventing a
`Plan` CRD before anyone has asked for a fourth is the speculative abstraction the guidelines
forbid. It becomes a CRD when a customer needs a bespoke one.

| plan | cpu | memory | pods |
|---|---|---|---|
| `trial` | 2 | 4Gi | 20 |
| `business` | 8 | 16Gi | 100 |
| `enterprise` | 32 | 64Gi | 500 |

Quota and limits are applied at every depth, per ADR 0004: a child is not trusted because its
parent is.

**Deletion.** The namespace is owner-referenced to the `Tenant`, so deleting a tenant reclaims
it. Descendant `Tenant` objects live inside that namespace and go with it, which cascades the
tree for free. ADR 0004 requires an e2e proving full garbage collection, and that test is part
of this work rather than deferred.

## Testing

**Unit** — `pkg/tenancy` is pure and gets exhaustive table tests: name derivation at the
63-character boundary, non-label segments, empty paths, and the ancestor walk over a fake tree
including "no ancestor has it".

**envtest** — the reconciler: a root tenant produces its namespace with the right quota; a child
produces a path-derived name; a name over 63 characters is rejected with a `Degraded` condition
naming the length rather than a truncated namespace appearing; changing `spec.plan` updates the
quota; deleting a parent removes the child's namespace too.

**e2e** — deferred to the isolation spec, where the phase-2 done-when lives: two nested
tenants, the child inheriting the parent's monitoring, and the negative network tests.

`pkg/tenancy` and `internal/controller/tenant` join `COVERED_PACKAGES`.

## Out of scope

- `CiliumNetworkPolicy` and the isolation tests — next spec, and where the done-when is proven.
- OIDC, RBAC bindings, and the generated kubeconfig Secret. The roadmap says "Keycloak or Dex"
  and that choice is the user's; nothing here depends on it.
- Per-tenant Flux `Kustomization`/`HelmRepository` sharding.
- `TenantNamespace`, `Quota` and `AccessGrant` kinds from the group table — nothing needs them
  until tenants can grant access to each other.
- The actual module implementations. Resolution answers "which ancestor's monitoring serves
  this tenant"; installing a monitoring stack is phase-3 packaging work.

## Risks

**A namespace is not a security boundary on its own.** Everything in this spec is naming,
quota, and structure; the isolation that makes a tenant a tenant is the next spec. Until it
lands, a `Tenant` object is an organisational convenience and should not be described as
isolation to anyone.

**The 63-character guard rejects late.** Validation at admission would be better than a
`Degraded` condition, but a CEL rule cannot see the parent chain, and a webhook is more
machinery than phase 2 needs. The reconciler rejecting loudly, with the length in the message,
is the compromise — and it is why the test asserts no namespace is created rather than only
that the condition appears.
