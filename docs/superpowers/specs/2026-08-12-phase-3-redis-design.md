# Phase 3, part 2 — redis, and the property it exists to test

- **Status:** approved, not implemented
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

The Valkey image version goes in [hack/versions.sh](../../../hack/versions.sh) and nowhere
else, per non-negotiable 1.

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
column that means something rather than a second Primary column that does not apply.

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
