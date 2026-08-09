# ADR 0003 — Cloud Native Buildpacks for the app plane

- **Status:** proposed
- **Date:** 2026-08-09

## Context

The `App` kind promises the Heroku experience: point at a git repo, get a running HTTPS
service. Between those two points sits a build, and the build is where this product is won or
lost. It runs untrusted tenant source code, inside our cluster, on shared nodes.

Three properties matter, in this order:

1. **Zero configuration for common stacks.** If a tenant has to write a Dockerfile, we have
   not shipped a PaaS — we have shipped Kubernetes with extra steps.
2. **Safety.** Tenant code must not need a privileged pod or a mounted Docker socket.
3. **Patchability.** When a CVE lands in a base image, we must be able to fix every affected
   tenant image without asking anyone to redeploy. This is the difference between a platform
   and a build script, and it is the obligation that comes with hosting other people's code.

[Cloud Native Buildpacks](https://buildpacks.io/) address all three. Buildpacks detect the
language and framework from source, produce an OCI image with no Dockerfile, run unprivileged,
and — critically — support **rebase**: because buildpacks keep app layers separate from the
runtime base, a patched base image can be swapped underneath an existing app image without a
rebuild. CNB graduated in the CNCF in 2026; it is a settled foundation, not a bet.

This is the layer that most Kubernetes-based platforms leave to the customer, so there is
comparatively little prior art to copy — which is precisely why it is the differentiator.

## Decision

**Cloud Native Buildpacks are the default build path.**

- `spec.build.kind: buildpack` (the default) runs the CNB lifecycle in an unprivileged pod.
- `spec.build.kind: dockerfile` runs rootless BuildKit, for tenants who need full control.
- `spec.build.kind: none` accepts a pre-built image reference and skips building entirely.
- Each app gets a build cache PVC.
- Images are pushed to the in-cluster registry (Zot or Harbor, backed by SeaweedFS) and
  **referenced by digest, never by tag**, everywhere downstream.

**Open sub-decision, to be settled by a spike before phase 4 implementation:** whether to run
the lifecycle via [kpack](https://github.com/buildpacks-community/kpack) or via our own thin
`Build` controller.

The recommendation is **kpack**, because it already implements automatic rebase when a builder
or stack image updates — that is property 3 above, and it is the hardest of the three to build
well. The case against is that kpack brings its own CRDs, controllers, and upgrade cadence for
something we could approximate with a Job.

Either way, **tenants only ever see `Build`**. If kpack is adopted, it lives in `packages/system/`
and its CRDs are an implementation detail behind our own. That wrapping is what makes this
sub-decision reversible.

## Consequences

**Good**

- A tenant with an ordinary Go, Node, Python, Ruby, or Java repo deploys with no build
  configuration at all.
- No privileged builds and no Docker socket anywhere in the cluster.
- Base-image CVEs are fixed by rebase across the whole fleet, without tenant action.
- Reproducible, digest-pinned images make rollback exact rather than approximate.

**Bad**

- Buildpacks are slower than a tuned Dockerfile on a cold cache, and the cache PVC is per-app
  state we now have to size, garbage-collect, and account for in quota.
- Unusual stacks fall off the zero-config path and land on the Dockerfile route, which is a
  worse experience — the buildpack catalog we ship defines where that cliff sits, and it needs
  an owner.
- The in-cluster registry becomes critical infrastructure: if it is down, nothing deploys and
  nothing scales up from zero on a cold image pull. It needs the same HA and backup treatment
  as etcd.
- Build capacity is a shared, abusable resource. Tenant builds need their own quota
  (concurrent builds, build minutes) and their own node pool or at minimum strict limits,
  or one tenant's CI storm becomes everyone's outage.

## Alternatives rejected

**Dockerfile-only, via BuildKit.** Simplest to build and gives tenants total control, but
fails property 1 outright and property 3 almost entirely — there is no rebase, so every base
image CVE becomes a fleet-wide rebuild campaign. Kept as an explicit opt-in mode rather than
the default.

**Knative Serving for build and scale-to-zero.** A coherent, well-trodden package, but it
brings a second networking stack (its own ingress and activator path) alongside Cilium Gateway
API, which is exactly the consolidation [ADR 0002](0002-cilium-single-dataplane.md) avoids.
KEDA's HTTP add-on delivers the scale-to-zero behaviour we actually need without a parallel
dataplane.

**Tekton pipelines as the build engine.** Powerful and flexible, but it is a CI system, and we
would be exposing CI concepts to tenants who asked for a PaaS. If tenants later want custom
pipelines, Tekton is the right thing to add *behind* `Build` — not as the tenant-facing model.

**Build outside the cluster (hosted CI).** Removes the untrusted-code-on-our-nodes problem
entirely, which is genuinely attractive. Rejected because it makes the platform depend on a
third party for its core loop, and the git-push-to-URL latency becomes someone else's SLA.
