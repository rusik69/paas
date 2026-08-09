# Phase 1, part 2 — Flux bootstrap and the `Platform` reconciler

- **Status:** proposed
- **Date:** 2026-08-09
- **Covers:** roadmap phase 1, bullets 2 and 3 — Flux bootstrap from the operator, and the
  `Platform` → `PackageSource`/`Package` reconcilers with two-stage ordering
- **Follows:** [part 1](2026-08-09-phase-1-api-and-crd-bootstrap-design.md), which shipped the
  types and the CRD applier

## Decisions taken before this spec

Two were settled directly:

**Flux is the rollout engine**, as architecture.md §6 and ADR 0001 assume. The operator does
not apply charts itself. This means Flux bootstrap has to land first, which is why both
bullets are in one spec.

**Rollback is symmetric.** The reconciler converges on whatever `spec.version` says, in either
direction, with no downgrade guard and no acknowledgement field. The phase-1 done-when reads
exactly that way. The cost is named under Risks.

## The shape

```
Platform (spec.version: v1.4.2)
  │  operator pulls oci://…/platform:v1.4.2, reads packages.yaml
  ├── PackageSource  "platform"          → Flux OCIRepository (digest-pinned)
  ├── Package "cnpg-migrate" stage=migration  → Flux HelmRelease
  └── Package "cnpg"         stage=component  → Flux HelmRelease (dependsOn the migration)
```

Three reconcilers, each with one job:

| Reconciler | Reads | Writes |
|---|---|---|
| `Platform` | the platform OCI artifact | one `PackageSource`, N `Package`s |
| `PackageSource` | its own spec | one Flux `OCIRepository` |
| `Package` | its own spec | one Flux `HelmRelease` |

Splitting it this way keeps the OCI-pulling code in exactly one reconciler, and makes the
other two pure translations that envtest can cover without a registry.

### Where the package list comes from

The platform artifact carries a `packages.yaml`:

```yaml
packages:
  - name: cnpg-migrate
    chart: cnpg-migrations
    version: "1.27.0"
    stage: migration
  - name: cnpg
    chart: cnpg
    version: "1.27.0"
    stage: component
```

This is what makes "one field change rolls out a whole platform version" true. The alternative
— listing packages in `Platform.spec` — would mean an upgrade edits N fields, and the done-when
would be false by construction.

### Two-stage ordering

`Package.spec.stage` drives it, and Flux enforces it: every `component` HelmRelease is written
with `dependsOn` naming every `migration` HelmRelease in the same release, plus `wait: true`.
Flux will not reconcile a dependent until its dependencies report Ready.

The ordering is therefore not ours to get right at runtime — it is expressed once, declaratively,
and enforced by a controller that already does this correctly.

## Flux bootstrap

The operator installs `source-controller` and `helm-controller` only. Not kustomize-controller,
not notification-controller, not image automation: each one is more attack surface and more
CRDs for a capability nothing in the roadmap uses yet.

Manifests are vendored under `internal/flux/manifests/`, generated from the pinned
`FLUX_VERSION` by a `make vendor-flux` target and committed, then embedded and applied with the
same server-side-apply path `internal/crd` already uses. Vendored rather than fetched at
runtime, for the reason phase 0 embeds its CRDs: a binary should install the versions it was
built against, and an operator that reaches the network on startup fails in a new way.

**Sharding.** Both controllers run with `--watch-label-selector=sharding.fluxcd.io/key notin
(…)` per the Flux sharding convention, so a later phase can move tenant workloads onto their own
shard without moving the platform's. Phase 1 sets the default shard only; the flag is present so
the retrofit is a value change rather than a redeploy of everything.

## Reconciler conventions

From go-guidelines, restated only where this spec makes a choice:

- Level-triggered and idempotent. Every write is server-side apply under the field manager
  `paas-operator/platform`, frozen from its first commit and distinct from `paas-operator/crd`.
- `ctx` threaded everywhere; never `context.Background()` in a reconciler.
- Status carries `observedGeneration` and conditions `Available`, `Progressing`, `Degraded`.
- Owner references from `Platform` to the `PackageSource` and `Package`s it creates, so
  deleting the `Platform` garbage-collects the tree. Cluster-scoped owners of cluster-scoped
  objects, which is legal; a namespaced owner would not be.
- A `Package` that disappears from `packages.yaml` between versions is deleted, not orphaned.
  This is the part of "rolls out a complete platform version" that a naive apply-only
  reconciler gets wrong, and the upgrade test asserts it.

## Testing

**envtest** covers everything except the registry:

- `Platform` with a stubbed artifact fetcher produces the expected `PackageSource` and
  `Package` set, with `component` packages depending on `migration` ones.
- Changing `spec.version` to a release with a different package set adds, updates and
  **removes** to match — the removal case is the one that matters.
- Rolling `spec.version` back reaches the same state as before the upgrade. Asserted by
  comparing the full object set, not by spot-checking a field.
- A `Package` whose `HelmRelease` reports failure surfaces as `Degraded` on the `Platform`,
  with the underlying message.
- Deleting the `Platform` removes everything it created.

The artifact fetcher is an interface with an OCI implementation and a test fake. The fake is
for the *fetch*, never for the apply: object writing is always tested against the real
apiserver, per go-guidelines.

**e2e** covers the registry path for real: push a platform artifact to the phase-0 in-cluster
registry, point a `Platform` at it, assert the components roll out; change the version; assert
rollback. That is the phase-1 done-when, so it lives in `test/e2e` and runs on the Talos guests.

**Coverage.** `internal/controller/platform` joins `COVERED_PACKAGES`.

## Out of scope

- The `packages/**` → OCI publishing pipeline. This spec consumes artifacts; producing them is
  the remaining phase-1 bullet. Until it exists, tests build fixture artifacts by hand.
- Any `ServiceClass` or tenant-facing kind. Phase 3.
- Multi-cluster or ring-based rollout. Architecture mentions rings; nothing in phase 1 needs
  more than one cluster.

## Risks

**Symmetric rollback can fail at apply time.** A chart that added a non-nullable field, or a
migration that is not reversible, will fail on the way down with a HelmRelease error rather
than a clear "this version cannot be rolled back". Accepted deliberately: the done-when asks
for rollback to work, and guarding it properly needs per-package downgrade metadata that does
not exist yet. The mitigation is that the failure is visible — `Degraded` with the chart's own
message — rather than silent.

**Vendored Flux manifests drift from the pinned CLI version.** `make vendor-flux` regenerates
them, and the CI generated-files gate already fails when a committed generated file disagrees
with its source, so drift breaks the build rather than the cluster.

**The two-stage guarantee is only as good as `wait: true`.** A HelmRelease that reports Ready
before its migration Job has finished would let a component start early. The migration charts
must gate readiness on Job completion; that is a property of the charts, and the e2e test is
what would catch it being wrong.
