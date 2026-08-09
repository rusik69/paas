# Roadmap

Build order for the architecture in [architecture.md](architecture.md). Phases 3 and 4 are
the product; 0–2 exist to make them possible.

Each phase's "done when" is a test that runs in CI, not a judgement call. The tiers referenced
below are defined in [testing.md](testing.md).

## Development and test environment

**Everything through phase 4 is developed and tested on local KVM virtual machines running
Talos** — three libvirt/KVM guests driven by `hack/e2e.sh`, one control plane and two workers,
which is the minimum that exercises DRBD replication and pod anti-affinity honestly. No
physical hardware is required until phase 5, where live migration and BGP peering need real
NICs and a real top-of-rack switch.

Talos boots from its `metal` ISO or qcow2 image; machine configuration is delivered to each
guest by the installer templates. Nested virtualisation must be enabled on the host, because
phase 5 runs KubeVirt inside these guests — check `/sys/module/kvm_intel/parameters/nested`
(or `kvm_amd`) before assuming it works.

Prerequisites on the dev machine — **none of these are currently installed**, so phase 0
starts by provisioning them:

```
go  git  kubectl  helm  talosctl  flux
qemu-kvm  libvirt-daemon-system  virt-install  virsh
docker (or podman)
```

## Phases

### Phase 0 — Foundation

Talos install flow, three-node cluster, Cilium, Piraeus/DRBD, in-cluster registry, and the
e2e harness itself.

- Render Talos machine configs from an installer template; `talosctl bootstrap`.
- Build the Talos installer image **with the DRBD system extension baked in** — without it
  the first PVC fails and the failure mode is opaque.
- Cilium with kube-proxy replacement and Gateway API enabled.
- Piraeus operator, `replicated-2` and `replicated-3` StorageClasses.
- `hack/e2e.sh` up/down, idempotent, runnable in CI.

**Done when:** `hack/e2e.sh` brings up a cluster from nothing and binds a `replicated-3` PVC
that survives killing the node holding the primary replica.

### Phase 1 — Platform core

`paas-operator` and the packaging/delivery machinery.

- `api/v1alpha1` scaffolding; CRDs embedded in the operator binary and applied on every start.
- Flux bootstrap from the operator (source-controller + helm-controller, sharded).
- `Platform`, `PackageSource`, `Package` reconcilers; two-stage OCI repositories so
  migrations land before component upgrades.
- Chart publishing pipeline: `packages/**` → OCI artifacts in the registry.

**Done when:** changing the version field on one `Platform` CR rolls out a complete platform
version, and rolling it back works.

### Phase 2 — Tenancy

`Tenant` reconciler and the isolation story.

- Namespace tree with path-derived names, 63-character guard.
- `ResourceQuota` + `LimitRange` from `spec.plan`.
- `CiliumNetworkPolicy` default-deny, with the label-based opt-ins.
- OIDC provider (Keycloak or Dex), group→RBAC binding, generated kubeconfig Secret.
- Module enable/inherit resolution up the ancestor chain.

**Done when:** two nested tenants exist, the child inherits its parent's monitoring, and a
negative network test proves cross-tenant traffic and pod→apiserver access both fail.

### Phase 3 — Managed services

The `ServiceClass` machinery and the first three catalog entries.

- `ServiceClass` → `CustomResourceDefinition` generator driven by `values.schema.json`.
- Dynamic per-kind reconciler: tenant CR → server-side-applied HelmRelease.
- Status propagation from the HelmRelease and from the underlying operator CR.
- Charts: `postgres` (CloudNativePG), `redis` (Valkey), `bucket` (SeaweedFS).

**Done when:** `kubectl apply -f postgres.yaml` in a tenant namespace yields a running HA
CNPG cluster, `kubectl get postgres` reports a real primary, and deleting the CR reclaims
everything.

### Phase 4 — App plane

The Heroku layer. This is the differentiator; it gets the most care.

- `App`, `Build`, `Domain` CRDs and reconcilers.
- Build execution: Cloud Native Buildpacks lifecycle, rootless BuildKit for the Dockerfile
  path, per-app cache PVC, push by digest to the in-cluster registry.
- Resolve the kpack-vs-own-controller question from
  [ADR 0003](adr/0003-buildpacks-app-plane.md) with a spike **before** writing the reconciler.
- Release: Deployment + Service + `HTTPRoute` + KEDA `ScaledObject`, digest-pinned.
- Routing: Cilium Gateway API, cert-manager wildcard via DNS-01, custom domains via
  CNAME/TXT verification then HTTP-01.
- Attachments: `Postgres` → injected connection-string Secret, rotation triggers rollout.

**Done when:** a git push produces an HTTPS URL serving the app, with a database attached via
`attachments`, and scale-to-zero followed by a cold request works.

### Phase 5 — VMs

First phase that needs real hardware.

- KubeVirt + CDI; `VirtualMachine` and `Disk` as `ServiceClass` entries.
- VNC console websocket subresource on `paas-apiserver`.
- Live migration — validate the RWX / DRBD dual-primary requirement early.
- Kube-OVN for tenant VPCs: isolated subnets, floating IPs, tenant routers.

**Done when:** a VM boots, live-migrates between nodes without dropping a ping, and its
console is reachable through the dashboard.

### Phase 6 — Commercial

- `paas-usage` collectors and hourly rollups; egress from Hubble flow metrics.
- Plan-enforcement validating webhook; settle the overage policy first.
- Dashboard and billing portal.
- Velero schedules, CNPG WAL archiving, and a restore drill wired into CI.

**Done when:** an invoice line matches independently measured usage, and the scheduled
restore drill passes.

## Test coverage by phase

The suite accretes; nothing is retired. Each phase adds its tier and keeps every earlier one
green.

| Phase | Test work delivered with it |
|---|---|
| 0 | `hack/e2e.sh` up/down, idempotent and aggressive about leaked libvirt domains; CI job skeleton; `make test` wired with `-race` |
| 1 | envtest harness and `setup-envtest` pinning; `Platform`/`Package` reconciler tests; chart golden-file rendering |
| 2 | Tenant reconciler tests; **the isolation suite** — its first version ships with the isolation it tests, never after |
| 3 | Schema-conversion unit tests and fuzz corpus; SSA drift and conflict tests; per-chart schema accept/reject tables |
| 4 | Full e2e journey including build, routing, attachment, scale-to-zero; fixture apps in Go and Node so buildpack detection is exercised |
| 5 | Live-migration and node-failure tests on real hardware; VM console smoke test |
| 6 | Restore drills in CI; usage-arithmetic unit tests; scale/soak at 200 tenants |

Two rules about ordering that are easy to violate under deadline pressure:

- **The isolation tests ship in phase 2, with the isolation.** Writing them later means
  writing tests that pass against whatever was built, rather than against what was intended.
- **The upgrade test starts at phase 1**, as soon as there are two platform versions to move
  between. It is nearly impossible to retrofit once several releases exist with no proven
  migration path.

## Sequencing notes

- **Do not build phase 5 before phase 4.** VMs are the most seductive and least
  differentiating piece of the platform.
- Shard Flux in phase 1, not later. Retrofitting sharding across live tenants is painful.
- Metering (phase 6) shapes the tenant model, so keep the per-namespace attribution labels
  correct from phase 2 onward even though nothing reads them yet.
- Keep `make test` under ten seconds from phase 0. The moment unit tests need a cluster, the
  tier boundary has been violated and the feedback loop is gone for good.
