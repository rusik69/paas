# Architecture

> Status: proposed. Reviewed decisions live in [adr/](adr/). Build order lives in
> [roadmap.md](roadmap.md).

## 1. What we are building

A multi-tenant cloud platform running on our own bare metal. Tenants buy three products:

1. **Managed data services** — Postgres, Kafka, Redis, ClickHouse, S3 buckets, ordered as API objects.
2. **App deployment** — Heroku-style: push a repo, get a built image, an HTTPS URL, autoscaling, logs.
3. **VMs / IaaS** — KubeVirt virtual machines, disks, virtual networks.

Two constraints shape everything below:

- **Bare metal, full stack ownership.** We own it from the NIC up: Talos → Kubernetes →
  storage → network. No managed control plane to hide behind.
- **Namespace-per-tenant, hierarchical.** Soft isolation, dense packing, nested tenants
  that inherit shared infrastructure from their parents.

Product (2) is the differentiator. Managed services and VMs on Kubernetes are well-trodden
ground with mature building blocks; a build-and-deploy plane that turns a git push into a
running HTTPS service is where most of the net-new design in this document goes.

## 2. Product surface: what a tenant sees

Everything is a Kubernetes object in the tenant's namespace, reachable via `kubectl`, the
dashboard, or `paasctl`. Three API groups:

| Group | Kinds | Backed by |
|---|---|---|
| `apps.paas.io/v1alpha1` | `App`, `Build`, `Domain`, `Postgres`, `Kafka`, `Redis`, `ClickHouse`, `Bucket`, `VirtualMachine`, `Disk` | Helm releases + upstream operators |
| `core.paas.io/v1alpha1` | `Tenant`, `TenantNamespace`, `Quota`, `AccessGrant` | our controllers |
| `platform.paas.io/v1alpha1` | `Platform`, `Package`, `PackageSource`, `ServiceClass` | operator-only, cluster-scoped |

Tenant-facing kinds are deliberately narrow. Each chart's `values.schema.json` defines the
*entire* writable surface — no image overrides, no arbitrary pod spec. That schema is
simultaneously the security boundary, the API validation, and the dashboard form definition.

## 3. Layers

```
L5  Dashboard · paasctl · public REST/OIDC gateway · billing portal
    ─────────────────────────────────────────────────────────────
L4  paas-apiserver (imperative verbs)   paas-controller (reconcile)
    logs/exec/console/restart/backup    Tenant, App, Build, ServiceClass
    ─────────────────────────────────────────────────────────────
L3  Managed services   App plane            VM plane
    CloudNativePG      Build → Registry     KubeVirt + CDI
    Strimzi, Valkey    → Deployment         Disks, VM images
    ClickHouse, S3     → HTTPRoute
    ─────────────────────────────────────────────────────────────
L2  Flux (source/helm controllers, sharded) · cert-manager · KEDA
    Prometheus/Grafana/Loki · Velero · Keycloak/Dex
    ─────────────────────────────────────────────────────────────
L1  Cilium (CNI, eBPF, Gateway API, BGP) · Piraeus/LINSTOR (DRBD)
    SeaweedFS (S3) · Kube-OVN (VM VPCs, phase 6) · Harbor/Zot registry
    ─────────────────────────────────────────────────────────────
L0  Talos Linux · 3+ control-plane nodes · etcd on NVMe · VIP
```

### Load-bearing decisions

Five choices carry most of the risk. Each has an ADR stating the alternatives that were
rejected and why.

| Decision | ADR |
|---|---|
| **Generated CRDs** are the store; HelmRelease is derived output. Imperative verbs move to a separate, small aggregated API. | [0001](adr/0001-generated-crds-over-aggregated-api.md) |
| **Cilium is the only dataplane** — CNI, service IPs via BGP, policy, Gateway API — until VM VPCs require otherwise. | [0002](adr/0002-cilium-single-dataplane.md) |
| **Cloud Native Buildpacks** are the default build path for the `App` plane. | [0003](adr/0003-buildpacks-app-plane.md) |
| **Tenants nest**, and modules enabled at a parent are inherited by descendants. | [0004](adr/0004-tenant-hierarchy-and-inheritance.md) |
| **Metering is first-class**, not bolted on — it shapes quota, admission, and the tenant model. | §9 |

The rest of the stack is deliberately conventional: Talos, Flux, KubeVirt, LINSTOR/DRBD,
SeaweedFS, and umbrella-chart packaging are all well-trodden choices for this problem, and
being creative there would spend risk budget on ground that is already solved.

## 4. Tenancy

A `Tenant` is a namespace plus an opinionated bundle, and tenants nest.

```yaml
apiVersion: core.paas.io/v1alpha1
kind: Tenant
metadata: {name: acme, namespace: tenant-root}
spec:
  plan: business                 # drives quota + feature gates
  isolation: shared              # shared | dedicated-nodes
  host: acme.paas.example        # wildcard domain for this tenant's apps
  modules:                       # "extras" enabled here, inherited by children
    ingress:    {enabled: true}
    monitoring: {enabled: true}
    seaweedfs:  {enabled: false} # -> resolves to the nearest ancestor that has one
  admins: [alice@acme.com]
```

Reconciling it produces:

- Namespace `tenant-acme`. A child `beta` underneath becomes `tenant-acme-beta` — names are
  path-derived and capped at 63 characters.
- `ResourceQuota` + `LimitRange` derived from `spec.plan`.
- `CiliumNetworkPolicy`: **default-deny** across namespaces, plus no pod→apiserver and no
  pod→tenant-etcd. Opt in per workload with explicit labels
  (`policy.paas.io/allow-to-apiserver: "true"`).
- A Flux `Kustomization` + `HelmRepository` scoped to the namespace, **sharded** by a hash of
  the tenant name across N helm-controller replicas. A single Flux shard is the known
  scaling bottleneck in this architecture; shard from day one.
- RBAC: `tenant-admin` / `tenant-viewer` Roles bound to OIDC groups, plus a generated
  kubeconfig Secret backed by a bound ServiceAccount token for CI use.
- Optional per-tenant ingress controller, monitoring stack, etcd, and SeaweedFS.

Module inheritance is the point of the hierarchy: one SeaweedFS cluster enabled at a parent
serves `Bucket` claims from every descendant, and a child without monitoring ships its
metrics to its parent's stack. See [ADR 0004](adr/0004-tenant-hierarchy-and-inheritance.md).

Hard multi-tenancy is explicitly not a goal at this isolation level. `isolation:
dedicated-nodes` (taints + affinity + a dedicated pool) is the compliance escape hatch.
Per-tenant control planes via Kamaji are a later phase.

## 5. ServiceClass → generated CRD → HelmRelease

The mechanism that lets us ship a new managed service by adding a Helm chart, with no Go code
and no recompile.

```yaml
apiVersion: platform.paas.io/v1alpha1
kind: ServiceClass          # cluster-scoped, ships with the platform package
metadata: {name: postgres}
spec:
  kind: Postgres            # -> apps.paas.io/v1alpha1, Kind: Postgres
  plural: postgreses
  chart: {repo: oci://registry.paas.io/apps, name: postgres, version: 1.4.2}
  schemaFrom: values.schema.json     # becomes the CRD's structural schema
  statusFrom:
    - {path: .status.conditions, from: helmrelease}
    - {path: .status.primary,    from: postgresql.cnpg.io/Cluster, jsonPath: .status.currentPrimary}
  ui: {icon: postgres, category: databases}
```

`paas-controller` reconciles each `ServiceClass` into:

1. A real `CustomResourceDefinition` whose OpenAPI structural schema is the chart's
   `values.schema.json`.
2. A dynamic reconciler for that kind: a tenant's `Postgres` CR becomes a `HelmRelease` in the
   same namespace with `values` taken from the CR spec, owner-referenced to the CR.
3. Status propagation back from the HelmRelease and from whatever operator CR the chart
   created, so `kubectl get postgres` shows something real.
4. A dashboard catalog entry rendered from the same JSON schema.

Charts use one of two patterns behind that interface:

- **Operator-based** — the chart emits `postgresql.cnpg.io/Cluster` and CloudNativePG does the work.
- **HelmRelease-based** — the chart emits a nested `HelmRelease` pointing at a vendored upstream chart.

Package taxonomy, four OCI-published chart families:

| Family | Contents |
|---|---|
| `core` | installer, platform config, Flux |
| `system` | operators and shared machinery: CNPG, Strimzi, KubeVirt, Cilium, Piraeus, registry |
| `apps` | what tenants order directly |
| `extra` | tenant-level enablers switched on per tenant and inherited by children |

**Known risk.** The tenant CR and the derived HelmRelease both hold values. The CR is
authoritative; the HelmRelease is machine-owned, written with server-side apply under a
dedicated field manager, and any hand edit is overwritten on next reconcile. Editing derived
HelmReleases is unsupported and should be blocked by RBAC.

## 6. Control plane components

| Binary | Responsibility |
|---|---|
| `paas-operator` | Bootstrap only. Installs and updates CRDs from embedded manifests on every start (this is what kills the stale-CRD problem), installs Flux, reconciles `Platform` → `PackageSource`/`Package`. Two-stage OCI repositories so migrations run before component upgrades. |
| `paas-controller` | The workhorse. Reconcilers for `Tenant`, `ServiceClass`→CRD, the dynamic per-kind → HelmRelease loop, `App`, `Build`, `Domain`, and Grafana dashboards. |
| `paas-apiserver` | Aggregated API serving imperative subresources only: tenant-scoped `pods/logs` proxy, `App/restart`, `VirtualMachine/console` (VNC over websocket), `Postgres/backup` and `/restore`, `Build/logs`. |
| `paas-usage` | Scrapes Prometheus and KubeVirt, writes hourly per-tenant rollups to Postgres, exposes the billing API. |
| `paasctl` | Tenant-facing CLI plus operator package tooling (`show`, `diff`, `apply`, dependency graph). |
| `dashboard` | Web UI. Forms generated from `values.schema.json`, catalog from `ServiceClass`. |

**Bootstrap flow.** Installer renders Talos machine configs → `talosctl bootstrap` → apply one
installer manifest → `paas-operator` pulls the platform OCI artifact → Flux takes over. The
platform version is pinned in a single `Platform` CR; an upgrade is one field change, rolled
across clusters in rings.

## 7. App plane

```yaml
apiVersion: apps.paas.io/v1alpha1
kind: App
metadata: {name: web, namespace: tenant-acme}
spec:
  source:
    git: {url: https://github.com/acme/web, ref: main, path: .}
    # or: image: {ref: ghcr.io/acme/web:v1}
  build: {kind: buildpack}        # buildpack | dockerfile | none
  runtime:
    command: ["./bin/server"]
    env: [{name: LOG_LEVEL, value: info}]
    envFrom: [{secretRef: {name: app-secrets}}]
    resources: {cpu: 500m, memory: 512Mi}
  scale: {min: 1, max: 10, targetRPS: 100}   # min: 0 -> scale-to-zero
  domains: [web.acme.paas.example, www.acme.com]
  attachments: [{kind: Postgres, name: maindb, as: DATABASE_URL}]
```

Pipeline:

1. **Trigger** — a GitHub/GitLab App webhook, or `paasctl deploy`, creates a `Build`.
2. **Build** — the Cloud Native Buildpacks lifecycle runs in an unprivileged pod for the
   zero-config path; rootless BuildKit handles `dockerfile`. Per-app cache PVC. Result is
   pushed to the in-cluster registry (Zot or Harbor) backed by SeaweedFS.
3. **Release** — the controller renders a Deployment, Service, `HTTPRoute`, and a KEDA
   `ScaledObject`. Rolling by default, blue/green behind a flag. The image is pinned by
   digest, never by tag.
4. **Route** — Cilium Gateway API. Wildcard `*.acme.paas.example` certificate via cert-manager
   DNS-01. For a custom domain the tenant creates a `Domain`; we verify a CNAME/TXT record and
   then issue via HTTP-01.
5. **Attachments** — binding a `Postgres` injects a connection-string Secret into the app;
   credential rotation updates the Secret and triggers a rollout.
6. **Scale to zero** — KEDA's HTTP add-on, not Knative. Knative would bring a second
   networking stack for no benefit we need.

Preview environments (branch or PR → ephemeral child `App` on a subdomain, garbage-collected
on merge) are a later add-on, but the `App` schema is designed to need no change for them.

See [ADR 0003](adr/0003-buildpacks-app-plane.md).

## 8. Storage, network, VMs

**Storage.** Piraeus/LINSTOR with DRBD provides replicated block storage — `replicated-2` and
`replicated-3` StorageClasses — and this is what databases and VM disks sit on. DRBD requires
a **Talos system extension**; bake it into the installer image or the first PVC fails.
SeaweedFS backs the `Bucket` kind and the registry. `local-path` exists for scratch only and
must be labelled non-durable in the catalog.

**Network.** Cilium is the single dataplane: eBPF CNI, kube-proxy replacement, Gateway API
implementation, `CiliumNetworkPolicy` for tenant isolation, and a BGP control plane
advertising service IPs to top-of-rack switches — which is why there is no MetalLB. Per-tenant
egress bytes come from Hubble flow metrics and feed billing directly. See
[ADR 0002](adr/0002-cilium-single-dataplane.md).

**VMs.** KubeVirt plus CDI. `VirtualMachine` and `Disk` are ordinary `ServiceClass`-generated
kinds. Live migration needs RWX or DRBD dual-primary — verify this early, because it
constrains the storage layout. The VNC console is a websocket subresource on `paas-apiserver`.
Real per-tenant L2/VPC networking (isolated subnets, floating IPs, tenant routers) is where
Kube-OVN comes back, and not before — it doubles the network surface area.

## 9. Identity, metering, operations

**Identity.** Keycloak, or Dex fronting an upstream IdP, as the OIDC provider that the
Kubernetes API server trusts. Tenant membership maps to OIDC groups, and the `Tenant`
reconciler creates the RBAC bindings. The dashboard and `paasctl` both authenticate via OIDC;
no long-lived static kubeconfigs for humans.

**Metering and plans.** `paas-usage` collects per-namespace CPU-seconds, memory GB-hours, PVC
GB-hours, VM-hours, S3 stored bytes and request counts, egress bytes, and build minutes, then
writes hourly rollups to Postgres that become invoice lines. Enforcement is two layers:
`ResourceQuota` derived from `spec.plan` (hard limits), plus a validating webhook that rejects
creating a kind the plan does not include (feature gating). The overage policy — throttle,
block, or bill — is a product decision that must be settled before the webhook is written.

**Observability.** Prometheus, Grafana, and Loki, with per-tenant instances at the tenant
level federating up to the platform stack. Grafana dashboards ship as code and are reconciled
by `paas-controller`. Platform SLOs cover API latency, reconcile lag, and build queue depth.

**Backup and DR.** Velero with namespace-scoped schedules per tenant, plus CNPG's native WAL
archiving to SeaweedFS and off-site S3. Restore is tenant-facing via the `Postgres/restore`
subresource. Etcd snapshots go off-cluster. Restore is tested in CI — backup that has never
been restored is not backup.

## 10. Repository layout

```
api/<group>/v1alpha1/ one package per API group: core, apps, platform
cmd/                  paas-operator, paas-controller, paas-apiserver, paas-usage, paasctl
internal/controller/  reconcilers, one package per kind
internal/dynamic/     ServiceClass -> CRD generator + dynamic HelmRelease reconciler
pkg/                  shared libs (schema, naming, tenancy resolution)
packages/
  core/               installer, platform, flux
  system/             cnpg, strimzi, kubevirt, cilium, piraeus, seaweedfs, keda, registry
  apps/               postgres, kafka, redis, clickhouse, bucket, vm, app
  extra/              monitoring, ingress, seaweedfs-tenant, gateway
dashboard/            web UI
dashboards/           Grafana JSON
test/e2e/             //go:build e2e — all e2e assertions live here, in Go
docs/                 this document + ADRs
hack/                 e2e.sh (3 KVM guests + Talos), dev scripts
```

Every package is an umbrella chart with `charts/` (vendored upstream), `patches/`,
`values.schema.json`, and a `Makefile` exposing `update` / `image` / `show` / `diff` / `apply`.
Never fork an upstream chart — vendor it and patch it, so upstream updates stay mechanical.

## 11. Verification

Full strategy in [testing.md](testing.md); coding conventions in
[go-guidelines.md](go-guidelines.md). In outline:

| Tier | Covers | Gate |
|---|---|---|
| Unit | Name derivation, schema conversion, module resolution, quota and usage arithmetic | every PR |
| Integration (`envtest`) | Every reconciler against a real API server, including SSA conflict and drift behaviour | every PR |
| Chart | `helm template` golden files, `values.schema.json` accept/reject, schema back-compat | every PR |
| E2E | Three KVM guests → Talos → platform → tenant → Postgres → App → HTTPS → teardown | merge queue |
| Isolation | Negative security assertions: cross-tenant traffic, apiserver reach, privilege escalation | merge queue |
| Upgrade | N-1 → N with an allowlist of expected restarts, plus rollback | merge queue |
| Restore | Backup → restore → row counts and checksums | nightly |
| Scale/soak | 200 tenants, Flux shard distribution, controller memory over 24 h | nightly |

Three of these are load-bearing and easy to let rot: **tenant deletion garbage collection**
(asserted resource type by resource type, not by trusting namespace deletion), **the negative
isolation tests** (which must assert the specific denial, or they keep passing after the
feature disappears), and **the restore drill** (backup that has never been restored is not
backup).

E2E assertions are Go tests behind `//go:build e2e`; `hack/e2e.sh` provisions infrastructure
only. Bash provisions, Go asserts.

## 12. Open questions

- **Hardware baseline** — node count, NVMe layout, NIC count and bonding, ToR BGP
  availability. Both the DRBD replication factor and whether live migration is viable at all
  depend on the answer.
- **kpack versus our own build controller** — see [ADR 0003](adr/0003-buildpacks-app-plane.md);
  needs a spike before phase 5.
- **Overage policy** — throttle, block, or bill. Changes the admission webhook design.
- **Regions and availability zones** — single cluster now, or a fleet from day one? Affects
  `Platform` and the tenant identifier namespace. Recommendation: single cluster, but never
  encode "one cluster" into tenant naming.

## References

- [Kubernetes API aggregation layer](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/)
- [Structural schemas for CustomResourceDefinitions](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#specifying-a-structural-schema)
- [Server-Side Apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
- [Cloud Native Buildpacks](https://buildpacks.io/)
- [Talos Linux system extensions](https://www.talos.dev/latest/talos-guides/configuration/system-extensions/)
