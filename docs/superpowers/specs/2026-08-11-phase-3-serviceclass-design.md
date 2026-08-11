# Phase 3, part 1 — ServiceClass, the generated CRD, and the HelmRelease behind it

- **Status:** implemented and proven by `TestService_PostgresBecomesReadyAndReportsItsPrimary`,
  `TestService_OffSchemaFieldIsRejectedWithItsOwnMessage` and
  `TestService_DeleteReclaimsEverything`, 2026-08-11
- **Date:** 2026-08-11
- **Covers:** the machinery of roadmap phase 3, proven with one catalog entry — `postgres`.
  `redis` and `bucket` are deliberately out of scope; see [Scope](#scope).
- **Depends on:** the phase 1 packaging machinery (`Platform`, `Package`, the OCI fetcher and
  `hack/publish.sh`), and the phase 2 `Tenant` reconciler that owns tenant RBAC.
- **Decides nothing that [ADR 0001](../../adr/0001-generated-crds-over-aggregated-api.md)
  already decided.** That ADR chose generated CRDs over an aggregated API server. This is how.

## Scope

The roadmap lists four bullets and three charts. This spec covers the four bullets and
**`postgres` only**.

`redis` and `bucket` become follow-on specs, and what they are allowed to add is a chart and a
`ServiceClass` — nothing in Go. That constraint is not a convenience. It is the property phase 3
exists to establish, and the only way to know it holds is to add the second service without
touching the machinery. Building all three at once would let a postgres-shaped assumption hide
inside the generator, because nothing would be pushing back on it.

## The load-bearing idea

**A tenant CR's `.spec` is the chart's Helm values, verbatim.**

Everything else follows. One `values.schema.json` becomes, with no mapping layer and nothing to
keep in sync:

- the generated CRD's OpenAPI structural schema,
- the API server's validation of what a tenant may write,
- the security boundary — what a tenant may *not* write,
- and, in phase 4, the dashboard form.

The alternative — a hand-written mapping from CR fields to values — reintroduces the per-service
Go code the whole design exists to avoid, and gives four places for the same fact to drift.

## Components

Four new pieces, following the existing one-package-per-kind layout under
`internal/controller/`.

### `api/platform/v1alpha1/serviceclass_types.go`

Cluster-scoped, so a platform version pins the catalog.

**How it gets there:** a `catalog` package, whose chart templates one `ServiceClass` per entry
and contains nothing else. It rides the existing `Platform` → `Package` → `HelmRelease` path
with no new delivery mechanism, and because `Platform` prunes, an entry dropped from a release
is removed from the cluster. Adding a service is therefore two files — a chart under
`packages/apps/`, and a template in `catalog` — and no Go.

`spec.chart` names a chart in the same in-cluster registry the platform publishes to, resolved
the same way `Package` resolves one. It carries no repository field: a catalog entry pointing at
an arbitrary registry would put an unreviewed `values.schema.json` in the position of being the
security boundary.

```yaml
apiVersion: platform.paas.io/v1alpha1
kind: ServiceClass
metadata: {name: postgres}
spec:
  kind: Postgres            # -> apps.paas.io/v1alpha1, Kind: Postgres
  plural: postgreses
  chart: {name: postgres, version: "0.1.0"}
  statusFrom:
    - path: .status.primary
      from: {apiVersion: postgresql.cnpg.io/v1, kind: Cluster}
      jsonPath: .status.currentPrimary
  ui: {icon: postgres, category: databases}
```

**`schemaFrom` from architecture.md §5 is dropped.** The chart's `values.schema.json` is always
the schema. A knob whose only correct setting is the default is a knob that will eventually be
set wrong.

### `internal/controller/serviceclass/`

Reconciles a `ServiceClass` into a `CustomResourceDefinition`:

1. fetch the chart through the existing OCI fetcher,
2. read `values.schema.json` out of it,
3. convert it to a structural schema, **or refuse** (see below),
4. server-side apply the CRD — group `apps.paas.io`, version `v1alpha1`, namespaced, status
   subresource on, printer columns for Ready, the first `statusFrom` path, and Age,
5. wait for `Established`,
6. ask the engine to run a controller for that GVK.

Status carries the usual conditions plus the chart version the CRD was generated from, so
"which schema is live" is answerable without fetching anything.

### `internal/controller/engine/`

A map of GVK to a running controller and its cancel func, with a mutex. `Start` is idempotent
— a `ServiceClass` reconciles repeatedly and must not accumulate controllers. `Stop` cancels the
context and then removes the informer from the shared cache, in that order, so nothing is
delivering to a controller that has gone.

Each controller it starts watches three things: the generated kind itself, the `HelmRelease` it
owns, and one informer per distinct GVK named in `statusFrom` — mapped back to the owning CR by
the labels in [Chart contract](#chart-contract). The third is why status propagation needs no
polling and no requeue timer: a CNPG `Cluster` electing a primary wakes the `Postgres` that
asked for it.

This is the piece with no upstream equivalent in controller-runtime, which wants its controllers
built before the manager starts. It is also the piece phases 4 and 5 reuse, since both add kinds
that do not exist at compile time.

### `internal/controller/service/`

One generic reconciler, instantiated per GVK, working in `unstructured`:

- renders the CR into a `HelmRelease` in the **same namespace**, owner-referenced to the CR,
  server-side applied under field manager `paas-operator/service`,
- values are `.spec` verbatim,
- copies status back: the HelmRelease's `Ready` condition, plus each `statusFrom` entry read by
  JSONPath from the named underlying object.

Same-namespace works because Flux is installed with `--watch-all-namespaces=true`; this was
verified rather than assumed, and it is the one line whose change would break the design
silently.

### `packages/apps/postgres/`

The tenant-facing chart, wrapping CloudNativePG. Its `values.schema.json` is written by us, not
inherited from upstream — it is the security boundary, and it exposes instances, storage size,
storage class and a resource envelope, and nothing that would let a tenant escape the namespace.

## What ownership gives us for free

**Reclaim.** Deleting a `Postgres` garbage-collects the `HelmRelease`, which uninstalls the
release, which deletes the CNPG `Cluster`, which deletes its PVCs. No finalizer, no cleanup
code. *This chain is assumed at its last link and must be verified early* — see Risks.

**RBAC.** `tenant-admin` and `tenant-viewer` get `apiGroups: ["apps.paas.io"], resources: ["*"]`.
Adding a `ServiceClass` therefore needs no RBAC change at all, and no re-reconcile of every
tenant. Tenants are never granted `HelmRelease` write, so the derived object cannot be used to
reach round the schema.

Nothing else gates a tenant creating a service in this phase. Plan-based feature gating belongs
to the validating webhook architecture.md §9 assigns it, alongside metering; `ResourceQuota`
already bounds what actually gets consumed.

## Chart contract

Every chart in the catalog labels every object it creates:

```
paas.io/service-name:      {{ .Release.Name }}
paas.io/service-namespace: {{ .Release.Namespace }}
```

That is what lets a status watch on an arbitrary underlying kind — a CNPG `Cluster` we did not
create and do not own — map back to the CR whose status it belongs in, without inventing owner
references across a boundary Helm controls. It is also what the tenant policy allowing ingress
from `paas-system` selects on, so the platform's operators reach the instances they provision
and nothing else the tenant runs (architecture.md §4).

**The release name is `<cr-name>-<lowercased kind>`,** not the CR's name. Two generated kinds
can carry the same CR name in one namespace, and both would otherwise render the same
`HelmRelease`: the API server refuses the second controller owner reference, and helm-controller,
seeing the chart name flip between reconciles, upgrades the release to the other chart — which
deletes the first kind's underlying object and every PVC owner-referenced to it. A tenant loses
a database by naming a cache after it. So `paas.io/service-name` carries the release name, and
`service.Reconciler` composes and decomposes it in the three places that must agree: the
`HelmRelease` it renders, the label selector `readStatusFrom` lists by, and the suffix
`byServiceLabels` strips to get back to the CR. Helm caps a release name at 53 characters, so
the CR name is bounded by that minus the kind; a longer one is refused by name rather than left
to fail inside helm-controller.

**A chart whose workload needs the API server must opt itself in.** Phase 2's tenant default-deny
policy blocks pod→apiserver unless the pod carries `policy.paas.io/allow-to-apiserver: "true"`
(architecture.md §4), and the catalog is not exempt from it — a service's own workload is a
tenant workload. The chart, not the platform, has to set the label, typically by threading it
through the wrapped operator's own pod-template metadata (CNPG's `spec.inheritedMetadata`, for
example). Nothing else notices a chart that omits this: the release installs, the underlying
object appears, and the workload it starts hangs forever unable to reach the control plane. See
[Findings](#findings) for how `postgres` was caught missing it.

## Failing closed on the schema

Kubernetes structural schemas are a subset of JSON Schema. `$ref`, `definitions` and
`patternProperties` are not allowed; `oneOf`, `anyOf` and `allOf` may not carry `type`,
`additionalProperties`, `default` or `nullable` inside them.

**A schema that cannot be represented faithfully produces no CRD.** The condition names the
offending JSON path and the reason it is unrepresentable.

The temptation is to drop what will not convert and generate the rest. That is the one thing
this must never do. Because `.spec` is the values, a dropped constraint is not a missing
validation — it is an unvalidated field flowing straight into Helm values. A generator that
degrades quietly turns the security boundary into a hole and reports success.

For the same reason `x-kubernetes-preserve-unknown-fields` is never set anywhere in a generated
CRD. That flag hands the entire boundary away in one line.

## A deleted ServiceClass does not delete its CRD

`Platform` prunes what a release drops. If pruning a `ServiceClass` deleted its CRD, a platform
upgrade that dropped a service would delete every tenant's objects of that kind — and with them,
through the ownership chain above, their data.

So the engine stops the controller and the CRD is left, marked orphaned in the `ServiceClass`
status and in a metric. An orphaned CRD whose objects nothing reconciles is a bad day that
someone notices. Silent data destruction during an upgrade is a catastrophe nobody notices until
it is irreversible. Removing the CRD stays a deliberate, separate act.

## Errors

| Failure | Response |
|---|---|
| Chart fetch fails | Condition, backoff, requeue. **Never tear down a working CRD over a registry blip.** |
| Schema not structural | Condition naming the path. Terminal for that pinned chart version — retrying cannot fix a schema that is already published. |
| CRD never `Established` | Condition, requeue. The engine does not start a controller for a kind the API server has not accepted. |
| Underlying object missing for a `statusFrom` | That status path is absent, not an error. A release that has not created its `Cluster` yet is early, not broken. |

## What proves it

Across the three tiers in [testing.md](../../testing.md).

**Unit**, no cluster, inside the ten-second budget:

- JSON Schema to structural schema, and **every rejection case** — `$ref`, `definitions`,
  `patternProperties`, a typed `oneOf`. These are the tests that matter; a generator is only as
  good as what it refuses, and per non-negotiable #9 the error paths are the point rather than
  the happy one.
- Values extraction from a CR spec, and JSONPath status reads including the absent case.

**envtest:**

- `ServiceClass` to an `Established` CRD.
- The engine starting a controller, and stopping it without leaking an informer.
- The service reconciler producing a `HelmRelease` with the right owner reference and field
  manager, and overwriting drift on the next pass — the mitigation ADR 0001 promises for its one
  acknowledged cost.

**e2e**, on the real Talos guests:

- The done-when: apply a `Postgres` in a tenant namespace, an HA CNPG cluster runs,
  `kubectl get postgres` reports the real primary, and deleting the CR reclaims the PVCs.
- A negative asserting the **specific** API-server validation message for an off-schema field,
  per non-negotiable #6. A test that accepts any error would keep passing if the CRD were
  generated with `x-kubernetes-preserve-unknown-fields`, which is precisely the regression worth
  catching.

New packages join `COVERED_PACKAGES`.

## Findings

**PVC reclaim is verified, not assumed (confirmed 2026-08-11).** The last link in the ownership
chain — CNPG deleting PVCs when its `Cluster` is deleted — was checked against a running
cluster rather than left as a claim. A CNPG-created PVC carries `ownerReferences` naming its
`Cluster`; deleting the `Cluster` removed the PVC within 35 seconds; no `PersistentVolume` was
left orphaned; and the `replicated-3` `StorageClass` sets `reclaimPolicy: Delete`, so nothing
downstream of the PVC is retained either. `TestService_DeleteReclaimsEverything` asserts this on
the cluster and keeps asserting it going forward.

**The schema boundary was proven live, in both directions.** An undeclared `spec.image` is
rejected by strict decoding, by name, before it ever reaches the API server's storage layer.
With that decoding bypassed — simulating a client willing to send whatever it likes — the field
is instead **pruned server-side**: the stored `spec` came back as exactly `{"instances": 1}`,
never `{"instances": 1, "image": ...}`. An undeclared field cannot reach Helm values by any
path this test found. Separately, an out-of-range value is refused with the API server's own
message, `spec.instances in body should be less than or equal to 3`, not a generic error.
`TestService_OffSchemaFieldIsRejectedWithItsOwnMessage` covers both directions in one test with
three subtests. This is the [load-bearing idea](#the-load-bearing-idea) demonstrated rather than
argued: the generated schema is where a tenant's write is actually stopped, not merely where the
design says it should be.

**A chart whose workload needs the API server must opt in, and nothing told chart authors so.**
`postgres` never set `policy.paas.io/allow-to-apiserver` on CNPG's instance-manager pods, so
phase 2's tenant default-deny policy blocked them from the control plane and `initdb` failed
forever with no condition pointing at the cause — the release had installed and the `Cluster`
object existed; only the pod it started was stuck. Fixed by setting the label through CNPG's
`spec.inheritedMetadata`. This is a property of the catalog contract, not of `postgres`
specifically, which is why [Chart contract](#chart-contract) above now states it as a
requirement rather than leaving it as something `postgres` happened to need.

## Risks

**Schema fidelity is the whole security boundary.** Everything above about failing closed exists
because of this. It is first because it is the only risk here whose failure mode is silent and
exploitable rather than noisy and annoying.

**The engine can leak informers.** A stopped kind whose informer stays in the shared cache keeps
watching a resource that may no longer exist, and the cost is invisible until there are enough of
them. Ordering — cancel, then remove — is asserted in envtest.

**The operator now creates CRDs cluster-wide.** It already applies its own on start, so the
grant exists and this widens rather than opens the blast radius. Worth stating because a bug in
the generator is now a bug that can write cluster-scoped API surface.

**A JVM-sized surprise, again.** Phase 2 discovered late that a component's real resource
appetite broke a node. CNPG clusters are tenant workloads on two 3 GiB workers; the e2e fixture
should ask for an instance count and storage size the dev cluster can actually host, and say so
in the chart's defaults.
