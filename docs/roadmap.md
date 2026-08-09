# Roadmap

Build order for the architecture in [architecture.md](architecture.md). Phases 3 to 5 are
the product; 0–2 exist to make them possible.

Each phase's "done when" is a test that runs in CI, not a judgement call. The tiers referenced
below are defined in [testing.md](testing.md).

## Development and test environment

**Everything through phase 5 is developed and tested on local KVM virtual machines running
Talos** — three libvirt/KVM guests driven by `hack/e2e.sh`, one control plane and two workers,
which is the minimum that exercises DRBD replication and pod anti-affinity honestly. No
physical hardware is required until phase 6, where live migration and BGP peering need real
NICs and a real top-of-rack switch.

Talos boots from its `metal` ISO or qcow2 image; machine configuration is delivered to each
guest by the installer templates. Nested virtualisation must be enabled on the host, because
phase 6 runs KubeVirt inside these guests — check `/sys/module/kvm_intel/parameters/nested`
(or `kvm_amd`) before assuming it works.

Prerequisites on the dev machine — **none of these are currently installed**, so phase 0
starts by provisioning them:

```
go  git  kubectl  helm  talosctl  flux
qemu-system-x86  libvirt-daemon-system  virt-install  virsh
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
- In-cluster OCI registry (Zot) on a `replicated-3` PVC, reachable from every node
  through a `machine.registries` mirror. Not SeaweedFS-backed: SeaweedFS arrives with
  the `Bucket` kind in phase 3 and phase 0 must not depend on it.
- `hack/e2e.sh` up/down, idempotent, runnable in CI.
- `AGENTS.md` — the conventions and traps a coding agent must know, pointing at
  these documents rather than restating them. It ships in phase 0 because an
  agent that learns the wrong conventions in phase 1 encodes them everywhere,
  and every later phase is written on top of that.
- **GitHub Actions.** `unit` (vet, e2e-compiles, tests, coverage floor),
  `lint` (golangci-lint, shellcheck, shfmt, actionlint), `vuln`, `tooling`,
  a nightly `versions` check, and `e2e` on a self-hosted runner with `/dev/kvm`.
- **`hack/deps.sh install` is proven by CI**, not by trust. The `tooling` job
  runs it on a clean hosted runner, asserts every pinned tool is present at its
  pinned version, then runs it a second time to prove the re-run path is a
  no-op. Otherwise the installer is exercised once per developer, on a machine
  nobody can reproduce, and rots unwatched.

**Done when:** `hack/e2e.sh` brings up a cluster from nothing and binds a `replicated-3` PVC
that survives killing the node holding the primary replica, and an image pushed to the
in-cluster registry is pullable by a node through the Talos mirror — on real Talos guests,
asserted by the suite itself, with `make cover` and every CI job green.

### Phase 1 — Platform core

`paas-operator` and the packaging/delivery machinery.

- `api/v1alpha1` scaffolding; CRDs embedded in the operator binary and applied on every start.
  *(Landed: the three types, controller-gen wiring, the embedded manifests, the applier and
  the envtest tier. The reconcilers below are outstanding.)*
- Flux bootstrap from the operator (source-controller + helm-controller, sharded).
  *(Landed: vendored manifests, embedded and applied on start. Sharding flags outstanding.)*
- `Platform`, `PackageSource`, `Package` reconcilers; two-stage OCI repositories so
  migrations land before component upgrades.
  *(Landed: all three reconcilers and the manager that runs them. `Platform` applies and
  prunes, so an upgrade removes what a release drops and a rollback reproduces the earlier
  state exactly. The OCI fetcher is implemented and tested against a real registry; it has
  nothing to pull until the publishing pipeline below exists.)*
- Chart publishing pipeline: `packages/**` → OCI artifacts in the registry.
  *(Landed: `hack/publish.sh` and `make publish`. Charts go to `<registry>/charts`, the release
  manifest to `<registry>:<version>`, and a test pushes through the script and reads it back
  with the operator's own fetcher.)*

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

### Phase 4 — Dashboard

The web UI. It sits here because it is the first phase with something worth rendering: the
`ServiceClass` catalog and the `values.schema.json` that generates its forms both arrive in
phase 3, and building the UI before them means hand-writing forms that the generator would
have produced.

- Read-only first: tenant tree, service catalog from `ServiceClass`, object status from
  conditions. A UI that can only observe is useful on day one and cannot corrupt anything.
- Forms generated from `values.schema.json`, never hand-written per service. A service added
  by adding a chart must appear in the UI by the same act, or the phase-3 property is lost.
- OIDC login against the phase-2 provider, with the tenant's own kubeconfig identity. The UI
  holds no privileges of its own — every call is made as the logged-in user, so RBAC and the
  isolation tests cover the UI for free.
- Write paths last: create, edit and delete for the kinds the catalog exposes.

**Done when:** a tenant user logs in, sees only their own namespaces, creates a `Postgres`
from a generated form, and watches it reach Ready — and a second tenant's objects are absent
from the API responses, not merely hidden in the client.

### Phase 5 — App plane

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

### Phase 6 — VMs

First phase that needs real hardware.

- KubeVirt + CDI; `VirtualMachine` and `Disk` as `ServiceClass` entries.
- VNC console websocket subresource on `paas-apiserver`.
- Live migration — validate the RWX / DRBD dual-primary requirement early.
- Kube-OVN for tenant VPCs: isolated subnets, floating IPs, tenant routers.

**Done when:** a VM boots, live-migrates between nodes without dropping a ping, and its
console is reachable through the dashboard.

### Phase 7 — Commercial

- `paas-usage` collectors and hourly rollups; egress from Hubble flow metrics.
- Plan-enforcement validating webhook; settle the overage policy first.
- Billing portal, on the phase-4 dashboard.
- Velero schedules, CNPG WAL archiving, and a restore drill wired into CI.

**Done when:** an invoice line matches independently measured usage, and the scheduled
restore drill passes.

## Test coverage by phase

The suite accretes; nothing is retired. Each phase adds its tier and keeps every earlier one
green.

| Phase | Test work delivered with it |
|---|---|
| 0 | `hack/e2e.sh` up/down, idempotent and aggressive about leaked libvirt domains; the full CI job set; `make test` wired with `-race`; `make cover` floor; `deps.sh install` proven on a clean runner; the suite asserts its own cluster is really Talos |
| 1 | envtest harness and `setup-envtest` pinning; `Platform`/`Package` reconciler tests; chart golden-file rendering |
| 2 | Tenant reconciler tests; **the isolation suite** — its first version ships with the isolation it tests, never after |
| 3 | Schema-conversion unit tests and fuzz corpus; SSA drift and conflict tests; per-chart schema accept/reject tables |
| 4 | Playwright journey against a real cluster: login, generated form, object reaches Ready; a cross-tenant read asserted absent from the API response, not hidden in the client |
| 5 | Full e2e journey including build, routing, attachment, scale-to-zero; fixture apps in Go and Node so buildpack detection is exercised |
| 6 | Live-migration and node-failure tests on real hardware; VM console smoke test |
| 7 | Restore drills in CI; usage-arithmetic unit tests; scale/soak at 200 tenants |

Three standing rules, from phase 0 onward:

- **Coverage is comprehensive, and CI enforces a per-package floor.** `make cover` fails when
  a package named in `COVERED_PACKAGES` is below `COVERAGE_MIN`, which ratchets up as phases
  land and is never lowered to make a red build green. Per package rather than global,
  because a global number is satisfied by covering whatever is cheapest. The floor is a backstop against carelessness, not evidence of good tests: coverage
  measures which lines ran, never whether anything was asserted about them. Every reconciler
  gets envtest coverage of its error and conflict paths, not only its happy path, and every
  bug fixed gets the test that would have caught it.
- **E2E runs on real Talos guests. Always.** Never kind, never minikube, never a mocked API
  server. Substituting one would make the suite green while testing none of what it exists to
  prove — the DRBD system extension, DRBD replication, unclean node loss, Cilium as the only
  dataplane. `TestCluster_NodesAreRealTalosGuests` asserts this from inside the suite, so a
  substituted cluster fails rather than quietly passes.
- **Every gate is a CI job.** A check that only runs when someone remembers is not a gate. If
  a rule in this document matters, it has a job in `.github/workflows/ci.yml`.

Two rules about ordering that are easy to violate under deadline pressure:

- **The isolation tests ship in phase 2, with the isolation.** Writing them later means
  writing tests that pass against whatever was built, rather than against what was intended.
- **The upgrade test starts at phase 1**, as soon as there are two platform versions to move
  between. It is nearly impossible to retrofit once several releases exist with no proven
  migration path.

## Sequencing notes

- **Do not build phase 6 before phase 5.** VMs are the most seductive and least
  differentiating piece of the platform.
- Shard Flux in phase 1, not later. Retrofitting sharding across live tenants is painful.
- Metering (phase 7) shapes the tenant model, so keep the per-namespace attribution labels
  correct from phase 2 onward even though nothing reads them yet.
- Keep `make test` under ten seconds from phase 0. The moment unit tests need a cluster, the
  tier boundary has been violated and the feedback loop is gone for good.
