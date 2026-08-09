# AGENTS.md

Instructions for coding agents working in this repository. Humans should read
[docs/architecture.md](docs/architecture.md) instead; this file is the short
version plus the mistakes that are specific to this codebase.

## What this is

A multi-tenant PaaS on bare metal: Talos → Kubernetes → Cilium → Piraeus/DRBD,
with tenants nested as namespaces and three products on top (managed data
services, a Heroku-style app plane, KubeVirt VMs). Phase 0 of seven.

## Read before writing code

Read the one that covers what you are touching. Do not infer conventions from
surrounding code alone — several of them are deliberate and unusual.

| Doc | When |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Anything that adds a component or an API |
| [docs/go-guidelines.md](docs/go-guidelines.md) | **Any Go change.** Not optional |
| [docs/testing.md](docs/testing.md) | Any test, and any change that needs one |
| [docs/roadmap.md](docs/roadmap.md) | Deciding whether the work belongs in this phase |
| [docs/adr/](docs/adr/) | Questioning a load-bearing decision |

## Commands

```sh
make test          # unit tests, -race. Must stay under ten seconds
make vet           # go vet
make verify        # vet + test; run before every commit
go vet -tags e2e ./...   # the e2e suite is invisible to `make test`; check it compiles

make deps          # report missing tooling
make versions      # confirm every pinned upstream version still resolves

make cluster-up    # three Talos guests on KVM, ~15 min cold
make e2e           # Go assertions against a running cluster
make cluster-down  # always run this; a leaked guest holds gigabytes of RAM
```

## Non-negotiables

Each of these has cost someone a day. They are enforced by review, not by a
linter, which is why they are listed here.

1. **Pinned versions live only in [hack/versions.sh](hack/versions.sh).** A
   version hardcoded anywhere else drifts, and the drift surfaces weeks later
   as an unreproducible cluster failure.
2. **Bash provisions, Go asserts.** `hack/*.sh` creates infrastructure and
   checks nothing. Every assertion lives in `test/e2e` behind `//go:build e2e`.
   Adding a `grep -q` "check" to the shell scripts starts a second, untested
   test framework.
3. **No `time.Sleep` in tests.** Use [pkg/wait](pkg/wait) or
   `testing/synctest`. A sleep is a guess about cluster timing and it is wrong
   on both fast and loaded machines.
4. **`make test` stays under ten seconds and needs no cluster.** The moment a
   unit test needs one, the tier boundary is gone and so is the feedback loop.
5. **Reconcilers are level-triggered and idempotent**, use server-side apply
   with a stable field manager, and never `context.Background()`. See
   go-guidelines; the field-manager string is API and changing it orphans field
   ownership across the fleet.
6. **Negative tests assert the specific denial** — a 403, a policy drop — never
   just that an error occurred. A test that accepts any error keeps passing
   after the feature it guards is deleted.
7. **Never fork an upstream chart.** Vendor it under `charts/` and patch it, so
   upstream updates stay mechanical.

## Layout

Full version in architecture.md §10 and go-guidelines. The parts that are easy
to get wrong:

- `api/v1alpha1/` — API types only. Its dependency set is `k8s.io/apimachinery`
  and nothing else, because external clients import it.
- `internal/` — everything that is not a deliberate public contract. Moving out
  of `internal/` later is easy; moving in is a breaking change.
- `pkg/` — code we would accept an external importer for.
- `hack/` — provisioning. `versions.sh` is sourced by everything.
- `test/e2e/` — `//go:build e2e`, one topology definition, read from
  `hack/e2e.sh nodes`.

## Repository-specific traps

- **The Talos installer image must carry the DRBD system extension**
  ([hack/talos/schematic.yaml](hack/talos/schematic.yaml)). Upgrading a node to
  a bare `siderolabs/installer` image silently drops every extension, and the
  next replicated PVC hangs with a mount error that never mentions DRBD.
- **Talos runs no kube-proxy and no CNI of its own.** Cilium provides both. A
  patch that re-enables either produces a cluster where pods start and
  networking intermittently does not work.
- **Gateway API CRDs are pinned to what Cilium declares conformance against.**
  A newer release installs fields the Cilium operator does not understand and
  reports it as a GatewayClass reconcile error.
- **Do not build phase 5 before phase 4.** VMs are the most seductive and least
  differentiating piece of the platform.

## Working agreements

- Verify before claiming. Run the command, paste the output. "Should work" is
  not a result.
- Say what you did not do. Scope reduction is the user's call, not yours.
- Keep changes inside the current phase unless asked. The roadmap's ordering
  constraints are load-bearing, not preferences.
