# Testing strategy

> Companion to [architecture.md](architecture.md). Coding conventions live in
> [go-guidelines.md](go-guidelines.md).

## Principles

1. **Test the contract, not the implementation.** Assert on the objects a reconciler
   produces, never on how many times it looped or in what order it wrote.
2. **Reconcilers are level-triggered, so tests must be too.** Every controller test
   reconciles at least twice and asserts the second pass is a no-op. Idempotency is not a
   nice property here, it is the correctness condition.
3. **No sleeps, ever.** Poll with a deadline, use a fake clock, or use `testing/synctest`.
   A `time.Sleep` in a test is a flake with a delay fuse.
4. **A flaky test is a broken test.** Quarantine it in the same PR that notices it, file the
   issue, fix it within the sprint. Never add a retry to make CI green — retries convert a
   real race into a rare production incident.
5. **Untested restore is not backup.** The same applies to upgrade paths and tenant deletion.
   These are asserted in CI, not by hand.
6. **Fast feedback is a feature.** Unit tests run in under 10 seconds for the whole tree.
   If they don't, something belongs in a higher tier.

## The pyramid

| Tier | What it covers | Harness | Runtime | Gate |
|---|---|---|---|---|
| **Unit** | Pure logic: name derivation, schema conversion, module resolution, quota math, usage rollups | `go test`, table-driven | < 10 s | every PR |
| **Integration** | Reconciler behaviour against a real API server | `envtest` | 2–5 min | every PR |
| **Chart** | `helm template` golden files, `values.schema.json` accept/reject, schema back-compat | `go test` + `helm` | < 1 min | every PR |
| **E2E** | The full journey on real Talos VMs | 3 KVM guests + `//go:build e2e` Go tests | 30–45 min | every PR (merge queue) |
| **Isolation** | Negative security assertions | e2e cluster | included in e2e | every PR |
| **Upgrade** | N-1 → N with no unexpected disruption | e2e cluster | 20 min | every PR |
| **Restore** | Backup → restore → row counts | e2e cluster | 15 min | nightly |
| **Scale/soak** | 200 tenants, Flux shard behaviour, controller memory | dedicated cluster | hours | nightly + pre-release |
| **Fuzz** | Schema conversion, name derivation, values marshalling | `go test -fuzz` | continuous | nightly |

## Unit tests

Pure functions, no cluster, no network, no filesystem beyond `testdata/`. These are where the
subtle platform bugs actually live:

- **Namespace path derivation** — `acme` → `tenant-acme`, `acme/beta` → `tenant-acme-beta`;
  the 63-character boundary at exactly 62, 63, and 64; invalid DNS labels; unicode; empty
  segments. Truncation is never correct here, so assert the error.
- **`values.schema.json` → CRD structural schema** — every JSON Schema construct we accept,
  and explicit rejection of the ones Kubernetes structural schemas forbid (`oneOf` at the
  root, `additionalProperties` combined with `properties` in the wrong shape, missing `type`).
  This conversion is the security boundary from [ADR 0001](adr/0001-generated-crds-over-aggregated-api.md);
  a permissive bug here lets a tenant set fields we never intended to expose.
- **Module inheritance resolution** — the ancestor walk from
  [ADR 0004](adr/0004-tenant-hierarchy-and-inheritance.md). Enabled at self, at parent, at
  grandparent, nowhere; `enabled: false` meaning "inherit" rather than "deny"; a cycle in the
  parent chain must error rather than hang.
- **Quota derivation** from plan, including plan downgrade below current usage.
- **Usage rollups** — hour-boundary arithmetic, counter resets when a pod restarts, gauge vs.
  counter handling, DST and leap seconds. Billing arithmetic that is wrong by 3% is a support
  queue.
- **Digest pinning** — a tag must never survive into a rendered Deployment.

Style: table-driven with `t.Run` subtests and `t.Parallel()`, `go-cmp` for comparisons, no
assertion DSL. See [go-guidelines.md](go-guidelines.md#testing).

## Integration tests (envtest)

`envtest` runs a real `kube-apiserver` and `etcd` with no kubelet — real validation, real
admission, real server-side apply, no scheduling. This is the right tier for every reconciler.

**Use envtest, not the fake client, for anything touching server-side apply.** The
controller-runtime fake client has gained SSA support via a field-managed object tracker, but
our entire HelmRelease derivation depends on field-manager ownership and conflict semantics
([ADR 0001](adr/0001-generated-crds-over-aggregated-api.md)) — the exact behaviour a fake is
most likely to approximate. The fake client is fine for a reconciler that only reads.

What each controller must prove:

**`Tenant`**
- Creates namespace, `ResourceQuota`, `LimitRange`, `CiliumNetworkPolicy`, RBAC, Flux
  `Kustomization`, and the kubeconfig Secret.
- Reconciling twice changes nothing (no-op second pass, no spurious `resourceVersion` bumps).
- A nested tenant lands in the right namespace with the right inherited modules.
- Deleting a parent cascades: descendants and their managed services are collected, the
  finalizer clears, and nothing is orphaned.
- An external actor deleting the `ResourceQuota` gets it recreated.
- Flux shard assignment is deterministic — the same tenant name always maps to the same shard.

**`ServiceClass` → CRD**
- A `ServiceClass` produces a CRD whose schema matches the chart's, with the status
  subresource and printer columns wired.
- Updating the chart's schema updates the CRD; a backward-incompatible change is **rejected**,
  not applied.
- Deleting a `ServiceClass` with live tenant CRs is refused.

**Dynamic CR → HelmRelease**
- The CR spec lands in HelmRelease values, owner-referenced, under our field manager.
- **Drift**: hand-edit the HelmRelease, reconcile, assert our values win and the drift metric
  increments.
- **Conflict**: another field manager claims a field we own; assert the conflict is resolved
  in our favour and logged rather than silently retried forever.
- Status propagates from both the HelmRelease and the underlying operator CR.
- Deleting the CR removes the HelmRelease.

**`App` / `Build`**
- Git source produces a `Build`; the same commit does not rebuild.
- A successful build renders Deployment, Service, `HTTPRoute`, `ScaledObject` — all
  digest-pinned.
- A failed build leaves the previous release serving and surfaces the failure in status.
- Attachment creates the connection Secret; rotating the secret triggers exactly one rollout.
- `scale.min: 0` produces a scale-to-zero-capable `ScaledObject`.

Fixtures live in `testdata/`; each test gets its own namespace and `t.Cleanup` removes it.
Never share state between envtest cases — parallel tests in a shared API server that assume
an empty cluster are a classic slow-burn flake.

## Chart and schema tests

Charts are tenant-facing API, so they get tested like API.

- **Golden rendering** — `helm template` each chart with a canonical values file into
  `testdata/golden/<chart>.yaml`, diffed in CI. `make golden` regenerates. This catches
  accidental image bumps, dropped resource limits, and namespace leaks in review, where they
  are cheap.
- **Schema accept/reject** — for each chart, a table of valid values that must pass and
  invalid values that must fail. Every field a tenant should *not* control (image, pod spec,
  node selector, host paths, privileged) gets an explicit rejection case. This is the concrete
  test of the security boundary described in [architecture.md §2](architecture.md#2-product-surface-what-a-tenant-sees).
- **Backward compatibility** — compare each chart's schema against the previously released
  version; a removed field or narrowed type fails the build unless the chart's major version
  is bumped.
- **Lint** — `helm lint` plus a policy check that no chart requests privileged containers,
  `hostNetwork`, or cluster-scoped RBAC without an explicit allowlist entry.

## End-to-end

**Environment: three local KVM guests running Talos**, managed through libvirt — one control
plane, two workers. That is the minimum that honestly exercises DRBD replication, pod
anti-affinity, and live failover. No physical hardware is needed until phase 5.

Practical constraints worth knowing before writing the harness:

- **Nested virtualisation** must be on for phase 5, since KubeVirt then runs inside these
  guests. Assert it in `hack/e2e.sh` preflight rather than discovering it as a confusing
  KubeVirt failure.
- **Give each worker a second virtual disk** for DRBD. Piraeus needs raw block devices, and a
  single-disk guest will not exercise the storage path the way production does.
- **Use a dedicated libvirt network** with a known CIDR so BGP and service-IP assignment are
  testable and so parallel CI runs on one host do not collide.
- **Pin guest names and MACs** per run ID. Leaked domains and volumes from a crashed run are
  the most common cause of a mysteriously failing local e2e; `hack/e2e.sh down` must be
  aggressive and idempotent.

**Structure.** `hack/e2e.sh` owns infrastructure only: define and boot the KVM guests, install
Talos, bootstrap the cluster, install the platform, export a kubeconfig, and tear everything
down. All
**assertions are Go tests** behind `//go:build e2e`, run against that kubeconfig. Bash
provisions; Go asserts. Assertions in bash are untestable, unreadable at scale, and produce
useless failure output.

```
hack/e2e.sh up      # provision + install, idempotent
go test -tags=e2e ./test/e2e/... -timeout 45m
hack/e2e.sh down
```

**The journey test** — one linear test that mirrors what a customer actually does:

1. Create a tenant; assert namespace, quota, network policy, RBAC.
2. Create a nested child tenant; assert it inherits the parent's monitoring.
3. Order a `Postgres`; wait for ready; connect and write a row.
4. Deploy an `App` from a fixture git repo in `testdata/fixtures/`; wait for the build.
5. `curl` the HTTPS URL through Gateway API and assert a 200 with a valid certificate.
6. Attach the `Postgres`; assert the app reads the row it wrote in step 3.
7. Scale to zero; assert pods drain; issue a cold request and assert it succeeds.
8. Kill the node hosting the Postgres primary; assert failover and no data loss.
9. Delete the tenant; assert **full garbage collection** — no leftover namespaces, PVCs,
   HelmReleases, DRBD resources, registry images, or DNS records.

Step 9 is the one that rots silently. Assert on absence explicitly, resource type by resource
type, rather than trusting the namespace to have taken everything with it.

**Fixture apps** live in `testdata/fixtures/` — a minimal Go service and a minimal Node
service, at least, so the buildpack detection path is genuinely exercised rather than assumed.

## Isolation and security tests

These are negative tests, and they are the ones that must never be skipped or marked flaky.
Each asserts a *failure*:

- A pod in tenant A cannot reach a pod, service, or database in tenant B.
- A tenant pod cannot reach `kube-apiserver` without the opt-in label; with the label, it can.
- A tenant pod cannot reach tenant etcd.
- A tenant's ServiceAccount cannot read secrets in another namespace, list cluster-scoped
  resources, or read another tenant's derived HelmReleases.
- A tenant cannot create a privileged pod, mount a host path, or set `hostNetwork`.
- A tenant cannot set an arbitrary image on a managed service — the schema rejects it.
- A tenant cannot exceed its `ResourceQuota`, and hitting the quota produces a clear status
  message rather than a stuck reconcile.
- A build pod runs unprivileged, with no Docker socket, and cannot reach the cluster API.

Write these so that a *pass* means the operation was denied. A negative test that silently
starts passing because the resource no longer exists is worse than no test: assert on the
specific denial (a 403, a policy drop) rather than on any error.

## Upgrade tests

Install version N-1, bump the `Platform` CR, assert:

- All tenant workloads stay ready throughout, except an explicit allowlist of expected
  restarts.
- CRD schema migrations apply cleanly, and existing tenant CRs remain valid.
- Migrations run before component upgrades — the two-stage OCI repository ordering from
  [architecture.md §6](architecture.md#6-control-plane-components) is asserted, not assumed.
- Rollback to N-1 works.
- No API version of a tenant-facing kind is removed without a conversion path.

## Restore drills

Nightly, on real data volume:

- Restore a tenant `Postgres` from backup into a scratch namespace; validate row counts and a
  checksum, not just that pods started.
- Restore a whole tenant namespace from Velero; assert workloads reconcile back to ready.
- Restore from an etcd snapshot into a throwaway cluster.

Record and track time-to-restore. An RTO nobody measures is a number in a slide deck.

## Scale and soak

Nightly on a dedicated cluster, and before every release:

- 200 tenants with a mix of services; measure tenant-creation latency at p50/p99 and
  reconcile lag.
- Flux shard distribution — assert no shard holds disproportionate load, since single-shard
  saturation is the known bottleneck in this architecture.
- Controller memory and goroutine counts over 24 hours; a monotonic climb fails the run.
  Informer cache growth with tenant count is the thing to watch.
- API server request rate from our controllers — an over-eager requeue loop shows up here
  long before it shows up in production.
- Build queue depth under a simulated CI storm from a single tenant, verifying that one tenant
  cannot starve the others.

## Fuzzing

`go test -fuzz`, corpus committed under `testdata/fuzz/`:

- `values.schema.json` → CRD schema conversion. It parses tenant-influenced input and its
  output defines an API surface; it deserves fuzzing more than anything else in the tree.
- Tenant name and path derivation.
- Usage record parsing and rollup arithmetic.

## CI gates

| Trigger | Runs |
|---|---|
| Every PR | unit + integration + chart + lint + `go vet` + `govulncheck` + build |
| Merge queue | the above plus e2e, isolation, upgrade |
| Nightly | restore drills, scale/soak, fuzz, dependency audit |
| Pre-release | everything, plus a manual failover exercise on real hardware from phase 5 |

Every test job runs with `-race`. Integration and e2e jobs upload cluster state on failure —
`kubectl get all -A`, controller logs, and Flux status — because an e2e failure with no
artifacts costs a full re-run to diagnose.

## Coverage

Coverage is a signal, not a target. Track it per package and review drops; do not gate merges
on a global percentage, which reliably produces tests written for the number rather than for
the bug. The packages that matter — `internal/dynamic` (schema conversion), `pkg/tenancy`
(inheritance resolution), and the usage rollup code — should be near-exhaustive on their input
domains, and reviewers should say so in review when they are not.

## Make targets

```
make test            # unit, -race, < 10s
make test-integration # envtest
make test-charts     # golden + schema
make test-e2e        # provisions VMs, runs e2e suite, tears down
make golden          # regenerate chart golden files
make lint            # golangci-lint, helm lint, policy checks
make fuzz            # short fuzz run against the committed corpus
```

## References

- [controller-runtime FAQ on fake client vs. envtest](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md)
- [envtest package documentation](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest)
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest)
