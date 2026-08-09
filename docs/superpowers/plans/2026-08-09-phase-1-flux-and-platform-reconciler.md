# Flux Bootstrap and Platform Reconciler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** `paas-operator` installs Flux, and one `Platform` version change rolls out a whole platform release through it — including rolling back.

**Architecture:** Three reconcilers. `Platform` pulls the platform OCI artifact, reads `packages.yaml`, and writes one `PackageSource` plus N `Package`s. `PackageSource` → Flux `OCIRepository`. `Package` → Flux `HelmRelease`, with `component` stages depending on `migration` stages.

**Tech Stack:** Go 1.25 (toolchain 1.26.5), `k8s.io/*` v0.34.3, controller-runtime v0.22.5, Flux API modules pinned to the controllers the pinned CLI deploys.

## Global Constraints

Everything from part 1's plan still applies. Additions:

- **Flux Go API modules must match the controllers `flux` v2.7.2 installs**: `source-controller/api v1.7.2`, `helm-controller/api v1.4.2`. These are the newest lines still on `k8s.io/apimachinery v0.34`; v1.8/v1.9 and v1.5/v1.6 require v0.35 and v0.36 and would drag the whole module graph two minor versions past the 1.34 cluster.
- **Field manager `paas-operator/platform`**, frozen from its first commit, distinct from `paas-operator/crd`.
- Reconcilers are level-triggered, use server-side apply, and never `context.Background()`.
- `internal/controller/platform` joins `COVERED_PACKAGES` when it lands.

---

### Task 1: Generic applier and Flux bootstrap

**Files:**
- Create: `internal/kube/apply.go`, `internal/kube/apply_test.go`
- Create: `internal/flux/flux.go`, `internal/flux/flux_test.go`, `internal/flux/bootstrap_integration_test.go`
- Create (generated): `internal/flux/manifests/flux.yaml`
- Modify: `Makefile` (add `vendor-flux`), `hack/versions.sh` (note the API-module coupling)

**Interfaces:**
- Produces: `kube.ApplyAll(ctx, c client.Client, objs []*unstructured.Unstructured, fieldManager string) error`, and `flux.Bootstrap(ctx, c client.Client) error`.

- [ ] **Step 1: `make vendor-flux`**

```make
.PHONY: vendor-flux
vendor-flux: ## Regenerate the vendored Flux manifests from the pinned CLI
	@command -v flux >/dev/null || { echo "flux not installed: run 'make deps-install'"; exit 1; }
	flux install --export \
		--components=source-controller,helm-controller \
		--namespace=flux-system >internal/flux/manifests/flux.yaml
```

Run it, commit the output.

- [ ] **Step 2: Generic applier, test first**

`internal/kube/apply.go` decodes nothing — it takes already-decoded unstructured objects and server-side applies each. Test with envtest: applying twice is a no-op, and a foreign edit to an owned field is corrected.

- [ ] **Step 3: `internal/flux`**

Embeds `manifests/flux.yaml`, splits it on `---`, decodes each doc to `*unstructured.Unstructured`, and applies via `kube.ApplyAll` with field manager `paas-operator/flux`.

Unit test: the vendored manifest parses, and contains Deployments named `source-controller` and `helm-controller` and no others (guards a `--components` flag someone widens by accident).

Integration test: bootstrap into envtest, assert the Flux CRDs reach Established and both Deployments exist.

- [ ] **Step 4: Commit**

---

### Task 2: `PackageSource` → `OCIRepository`

**Files:**
- Create: `internal/controller/packagesource/reconciler.go` and its integration test.

**Interfaces:**
- Produces: `packagesource.Reconciler` with `SetupWithManager(mgr) error`.

- [ ] **Step 1: Integration test first**

Create a `PackageSource`; assert an `OCIRepository` appears in `flux-system` with the same `url`, the interval from spec, `insecure` propagated, and an owner reference back to the `PackageSource`. Then change the interval and assert the `OCIRepository` follows — the level-triggered claim.

- [ ] **Step 2: Implement, then commit**

---

### Task 3: `Package` → `HelmRelease`

**Files:**
- Create: `internal/controller/pkg/reconciler.go` and its integration test.

- [ ] **Step 1: Integration test first**

A `component` `Package` produces a `HelmRelease` whose `dependsOn` names every `migration` `Package` of the same platform, with `wait: true`. A `migration` `Package` produces one with no `dependsOn`. Changing `spec.version` updates the `HelmRelease` chart version.

- [ ] **Step 2: Implement, then commit**

---

### Task 4: `Platform` → `PackageSource` + `Package`s

**Files:**
- Create: `internal/controller/platform/reconciler.go`, `internal/controller/platform/artifact.go` (the fetcher interface plus its OCI implementation), and integration tests.

**Interfaces:**
- Produces: `platform.Fetcher` — `Fetch(ctx, registry, version string) (*platform.Release, error)` where `Release` carries the parsed `packages.yaml` and the resolved digest.

- [ ] **Step 1: Integration tests first, against a fake fetcher**

- One `Platform` produces one `PackageSource` and one `Package` per entry.
- Changing `spec.version` to a release with a different set **adds, updates and removes** to match. The removal case is the one that matters.
- Rolling the version back reproduces the earlier object set exactly — compare the whole set, not a field.
- Deleting the `Platform` garbage-collects its children.
- A `Package` reporting failure surfaces as `Degraded` on the `Platform`, carrying the message.

- [ ] **Step 2: Implement, then commit**

---

### Task 5: Wire the manager

**Files:**
- Modify: `cmd/paas-operator/main.go`, `internal/crd/apply.go` (Install also bootstraps Flux and starts the manager).

- [ ] **Step 1: Start a controller-runtime manager after CRD install and Flux bootstrap, register all three reconcilers, block until the context is cancelled. Commit.**

---

## Self-Review

Spec coverage: Flux bootstrap (1), the three reconcilers (2, 3, 4), two-stage ordering (3), the prune case (4), rollback symmetry (4), manager wiring (5). The `packages/**` publishing pipeline stays out of scope, so Task 4's tests build fixture releases by hand via the fake fetcher — matching the spec.
