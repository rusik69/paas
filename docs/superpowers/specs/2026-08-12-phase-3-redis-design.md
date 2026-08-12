# Phase 3, part 2 — redis, and the property it exists to test

- **Status:** implemented. Proven by `TestRedis_BecomesReadyAndReportsItsReplicaCount`,
  `TestRedis_StoresWhatWasWritten`, `TestRedis_OffSchemaFieldIsRejectedWithItsOwnMessage`, and
  `TestRedis_DeleteReclaimsEverything`, on real Talos guests, with the postgres
  `TestService_*` suite passing in the same run.
- **Date:** 2026-08-12
- **Covers:** the second catalog entry of roadmap phase 3. `bucket` stays outstanding.
- **Depends on:** the machinery from
  [part 1](2026-08-11-phase-3-serviceclass-design.md), implemented and proven.

## Why this entry exists

Not because the platform needs Redis. Because part 1 claims that adding a managed service
costs a chart and a catalog template and **nothing in Go**, and a claim tested only against the
service it was written for is not tested at all. A postgres-shaped assumption inside the
generator would be invisible until something else went through it.

So the deliverable is really the diff. If redis lands as five files and one deliberate,
one-time Go change, the claim holds. If it needs more, the machinery has a gap and finding it
is worth more than the service.

## The one Go change, and why there is exactly one

`statusSchema()` in `internal/controller/serviceclass/crd.go` hardcodes a `primary` string
field, because postgres is the only entry and it copies into `.status.primary`. A structural
schema silently drops an undeclared field on write, so redis writing into `.status.ready`
would produce nothing, and nothing would say why. Part 1 recorded this deliberately rather than
guessing at the general shape before a second shape existed.

The general shape turns out to be smaller than expected. `readStatusFrom` renders a JSONPath
into a buffer and returns a **string**, so every value `statusFrom` can produce is already a
string. The generalisation is therefore not a type system: declare each path named in
`spec.statusFrom` as a string, instead of declaring a fixed `primary`.

No API change. No `type` field on `StatusSource`. The printer column already reads
`StatusFrom[0].Path` and needs nothing. And no third service pays this cost again — after
this, a service's status fields are declared by its own catalog template.

**This is the only Go change the entry is allowed.** Anything else it turns out to need is a
finding about the machinery, to be reported rather than absorbed quietly.

## What backs it

**A small Valkey chart, written here** — a StatefulSet of one, a headless Service, and a
`volumeClaimTemplate`. Not a vendored upstream chart, whose bulk would be carried and mostly
unused given that our `values.schema.json` exposes a curated subset anyway; and not an
operator, which is the right long-term shape for a managed service but adds a second platform
component to a phase whose remaining question is about packaging, not about Valkey.

The cost is honest: we own the operational quality, and this is a dev-cluster-grade Valkey.

**Persistent, single instance.** A PVC on the tenant's chosen replicated class, so data
survives a restart or a reschedule. A tenant who treats it as a cache loses nothing by its
durability, while a tenant who assumed durability and got `emptyDir` loses everything. One
instance because Valkey replication without an operator means writing failover into a chart,
which is where a small chart stops being small — and HA is its own design, not this entry's.

## The five files

| File | Contains |
|---|---|
| `packages/apps/redis/Chart.yaml` | name, version |
| `packages/apps/redis/values.schema.json` | the security boundary |
| `packages/apps/redis/values.yaml` | defaults |
| `packages/apps/redis/templates/valkey.yaml` | StatefulSet and Service |
| `packages/system/catalog/templates/redis.yaml` | the `ServiceClass` |

The Valkey image version is declared in [hack/versions.sh](../../../hack/versions.sh) — what
`make versions` checks still resolves upstream — and repeated as a literal in
`packages/apps/redis/values.yaml` and in `Chart.yaml`'s `appVersion`, because a chart must be
self-contained. A comment in `values.yaml` asks the two to agree; nothing enforces it. This is
the same arrangement `KEYCLOAK_VERSION` has with the vendored keycloakx chart, so it is
established precedent here, not a new gap. See [Findings](#findings) for the one difference
between the postgres copy of this pattern and this one.

## The schema

Storage size, storage class, and a CPU and memory envelope — bounded, in the shape postgres
settled on after review. No image, no arguments, no config-file injection.

Deliberately **not** exposing `maxmemory-policy` or its neighbours in v1. Every knob is a
decision that a tenant may set it, nobody has asked for these, and a schema is far easier to
widen later than to narrow.

## What this entry tests for free

**Redis does not talk to the API server, so its chart must not carry
`policy.paas.io/allow-to-apiserver`.** Part 1 added that requirement to the chart contract
after CNPG's instance manager was blocked by the tenant default-deny policy — a defect found
only on a real cluster. A contract discovered that way invites the opposite failure, where
every subsequent chart copies the label defensively and the per-pod opt-in quietly becomes
universal.

Redis is the first chance to demonstrate the label is genuinely conditional. Its absence here
is a deliberate assertion, not an omission.

## Status propagation

`statusFrom` reads the StatefulSet's own `.status.readyReplicas` into `.status.ready`. The
StatefulSet carries the chart-contract labels, so the existing reverse map finds it with no
new machinery.

That path is what forces the generalisation above, and it gives `kubectl get redis` a Ready
column backed by a real condition. The second column is a different story: `crd.go` still
names it `Primary` unconditionally, so `kubectl get redis` prints `PRIMARY 1` — a label that
made sense for postgres and does not for a StatefulSet's ready-replica count. See
[Findings](#findings) for what that assumption is and why it stays as found.

## What proves it

**e2e, on the real cluster, and it goes further than postgres's does.** The postgres test
asserts a primary was elected; it never asks whether Postgres works. Here the functional check
is cheap, so:

- a `Redis` in a tenant namespace reaches Ready, and `.status.ready` equals the StatefulSet's
  `.status.readyReplicas` — equality, because a hardcoded value would satisfy non-emptiness;
- a `SET` followed by a `GET` through `valkey-cli` returns the value written. A pod that is
  Running and a datastore that actually stores are different claims, and the second is the one
  a tenant is buying;
- deleting the `Redis` reclaims the PVC;
- an off-schema field is refused with its **specific** message, per non-negotiable 6.

**And the property itself**, checked at review rather than in code: the diff for this entry
contains no `.go` changes beyond the `statusSchema` generalisation. That is the actual
deliverable, and it is worth stating in the roadmap when it lands.

## Findings

**The claim held.** `git diff --stat` from the commit before the first task to the last shows
exactly one production Go file changed: `internal/controller/serviceclass/crd.go`, generalising
`statusSchema` — the one change [budgeted above](#the-one-go-change-and-why-there-is-exactly-one)
before redis existed to need it. Everything else the entry needed was three test files
(`internal/controller/serviceclass/crd_test.go`, `internal/schema/structural_test.go`,
`test/e2e/redis_test.go`) and the five non-Go files the design promised: `hack/versions.sh`, the
four `packages/apps/redis/` files, and `packages/system/catalog/templates/redis.yaml`. A second
service really did cost a chart and a catalog template and nothing in Go beyond the change
already planned for it — measured, not asserted, and the postgres `TestService_*` suite stayed
green in the same run, which is what proves the generalisation did not regress the machinery
every other generated CRD depends on.

**The chart leaked the tenant's volume on delete, and only the e2e could find it.**
`TestRedis_DeleteReclaimsEverything` failed on its first run: the `HelmRelease` and the
`StatefulSet` were both gone, but the PVC stayed `Bound`. Helm never creates a StatefulSet's
`volumeClaimTemplate` PVCs — the StatefulSet controller does, at scale-up — so they are absent
from the release manifest and `helm uninstall` never touches them, and Kubernetes deliberately
leaves them behind when the StatefulSet itself is deleted. `postgres` never hit this because CNPG
owner-references its PVCs to its `Cluster` (see the [phase 3 part 1
findings](2026-08-11-phase-3-serviceclass-design.md#findings)); a hand-written StatefulSet chart
gets no such reference for free. Fixed with `spec.persistentVolumeClaimRetentionPolicy`
(`whenDeleted: Delete`, `whenScaled: Retain`) on the StatefulSet, after which the test passes in
43s. This is a trap for every future hand-written StatefulSet chart in this catalog, not just
this one.

**`statusSchema` was fail-open for its own reserved names.** The generalisation had to reserve
`.status.conditions` and `.status.ready` from being redeclared by a class's own `statusFrom`, and
the first version of that check did not actually enforce it: a `ServiceClass` naming
`.status.conditions` in `statusFrom` would have overwritten the conditions array's schema with a
string and produced a CRD that looked valid but rejected every status write the API server made
afterward. Found in review, not on the cluster; fixed by deriving the reserved set from the
seeded map's own keys instead of a second, hand-maintained list that could drift from it.

**The deliberate absence held.** `packages/apps/redis` does not carry
`policy.paas.io/allow-to-apiserver` — Valkey never talks to the API server — and nothing broke.
This is the demonstration [promised above](#what-this-entry-tests-for-free): the chart-contract
label added after CNPG's instance manager was blocked by the tenant default-deny policy is a
genuine per-pod opt-in, tested here on a chart that has no reason to carry it, rather than
something every chart in the catalog copies defensively regardless of need.

**A surviving postgres-shaped assumption: the second printer column's name is hardcoded.**
`CRDFor` in `crd.go` builds it as `Name: "Primary"` unconditionally, while its `JSONPath`
correctly comes from `sc.Spec.StatusFrom[0].Path`. With one catalog entry the two could not
disagree — postgres's path *is* `.status.primary` — so nothing forced the name to generalise
alongside the value it labels, and it was invisible until a second entry read from a
differently-named path. Redis's is `.status.ready`, so `kubectl get redis` now prints a column
headed `PRIMARY` carrying the ready-replica count, which is the exact "second Primary column
that does not apply" the status-propagation section above used to claim redis avoided. Fixing
it means deriving the column name from the status path's last segment instead of a literal —
small, but it is a Go change, and this entry's whole deliverable is the measurement of how
much Go a second catalog entry costs. Absorbing a second Go change quietly here, on the grounds
that it is small and obviously correct, would corrupt that measurement rather than report it.
It is left unfixed, on the record, for whoever adds the third service class.

**The version triplication has no assertion, unlike its precedent.**
`TestCNPG_OperatorIsDeliveredByThePlatform` in `test/e2e/cnpg_test.go` reads `CNPG_VERSION` out
of `hack/versions.sh` and asserts it against the running operator's `app.kubernetes.io/version`
label, so CNPG's three copies of its version cannot drift without a red e2e run. Redis has the
same three-copy arrangement — see [the five files above](#the-five-files) — but
`test/e2e/redis_test.go` asserts none of it;
only the comment in `values.yaml` asks `hack/versions.sh` and the chart to agree. This is a
real gap, not a new one — writing and verifying that assertion needs a cluster run, and this
branch's cluster run is done, so it is recorded here as a follow-up rather than added now.

## Risks

**The `statusSchema` change touches what every generated CRD depends on.** A regression breaks
postgres, not redis. The existing `TestService_*` suite is what catches it, so the e2e run that
verifies redis must include those tests rather than running the redis ones alone — the obvious
mistake, and the expensive one, since it costs a fifteen-minute cluster cycle to learn.

**A numeric status rendered as a string.** `readStatusFrom` returns strings, so `.status.ready`
carries `"1"` rather than `1`. Consistent with how every other `statusFrom` value behaves, and
honest, but it will look odd to whoever reads the CRD first. Declaring real types is more
machinery than one odd-looking field justifies today; revisit if a third service wants a
number that is compared rather than displayed.

**Dev-cluster-grade Valkey.** One instance, no failover, no tuning. Correct for proving the
packaging property and wrong for a tenant expecting a managed cache to survive a node. The
roadmap should say so where a reader will see it, so this is not mistaken for the finished
product.

**The memory limit equals the request, and no `maxmemory` is configured.** A tenant filling
the cache gets an OOMKill loop rather than key eviction — a surprising failure mode for
something a tenant may reasonably treat as a cache, which is supposed to shed data under
pressure rather than restart.

**The pod template carries no `securityContext`.** No `runAsNonRoot`, no dropped capabilities,
no seccomp profile. This is the catalog's first hand-written pod template, so whatever it does
here is the precedent the third chart will copy. A weak precedent copied three times is harder
to fix than one, so this should be said plainly rather than left to be noticed later.
