# AGENTS.md

Instructions for coding agents working in this repository. Humans should read
[docs/architecture.md](docs/architecture.md) instead; this file is the short
version plus the mistakes that are specific to this codebase.

## What this is

A multi-tenant PaaS on bare metal: Talos → Kubernetes → Cilium → Piraeus/DRBD,
with tenants nested as namespaces and three products on top (managed data
services, a Heroku-style app plane, KubeVirt VMs). Phase 0 of eight.

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
make verify        # vet + vet-e2e + lint + cover + check-stdout. Before every commit
make test          # unit tests, -race. Must stay under ten seconds
make cover         # tests plus the coverage floor (COVERAGE_MIN)
make vet-e2e       # the e2e suite is invisible to `make test`; check it compiles
make lint          # golangci-lint, gofumpt included; needs `make deps-install` first
make actionlint    # the CI workflows are a deliverable and nothing else checks them
make vuln          # govulncheck blocks the merge; cheaper to learn that here

make deps          # report missing tooling
make versions      # confirm every pinned upstream version still resolves

make cluster-up    # three Talos guests on KVM, ~15 min cold
make e2e           # Go assertions against a running cluster
make cluster-down  # always run this; a leaked guest holds gigabytes of RAM
make test-e2e      # all three in one shot; tears down even when the assertions fail
```

## Subagents

Defined in [.claude/agents/](.claude/agents/). They inherit this file automatically, so each
definition adds only what is specific to its role.

| Agent | Does | Cannot |
|---|---|---|
| `gate-runner` | Runs every local gate, reports only failures | Edit anything; touch a cluster |
| `e2e-author` | Go assertions under `test/e2e` | Touch `hack/`; run the suite |
| `go-reviewer` | Audits a Go diff against go-guidelines | Write — it has no shell |
| `provisioner` | Edits `hack/`; explicit invocation only | Assert in bash; run `virsh` or `hack/e2e.sh up`/`down` |

Order of increasing cost: `gate-runner` and `go-reviewer` before a commit, `/code-review`
before a push, `make e2e` in the merge queue. A green `gate-runner` is not a merge signal —
it runs the cheap local subset, never the e2e job.

Cluster lifecycle stays manual. `make cluster-up` is a singleton over three libvirt guests;
two agents driving it would fight over the same machines.

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
8. **E2E runs on real Talos guests. Always.** Never kind, never minikube, never
   a mocked API server. A substitute makes the suite green while testing none of
   what it exists to prove. `TestCluster_NodesAreRealTalosGuests` enforces this.
9. **Coverage is comprehensive and `make cover` gates it, per package.** The
   floor applies to the packages named in `COVERED_PACKAGES`, not to a global
   percentage — a global number is satisfied by covering whatever is cheapest,
   which produces tests written for the number rather than for the bug. Cover
   error and conflict paths, not only the happy one; every bug fixed gets the
   test that would have caught it. Add a package to the list when it becomes
   load-bearing, raise `COVERAGE_MIN` when a phase lands above it, and never
   lower either to make a build green. When a branch cannot be reached by any
   test, delete it or restructure until it can — an unreachable error path is
   not covered by being excused.
10. **Every gate is a CI job.** A check that runs only when someone remembers is
    not a gate. Adding a rule means adding a job in
    [.github/workflows/ci.yml](.github/workflows/ci.yml).
11. **Write fewer comments.** Default to none. A comment earns its place only by
    explaining *why* — a non-obvious constraint, an upstream bug, a decision
    that looks wrong until you know the reason. Comments that restate the code
    go stale and then lie. Doc comments on exported identifiers are the
    exception and stay required.
12. **Keep code clean and concise.** The smallest change that fully does the
    job, in the plainest form the language offers. No speculative abstraction,
    no options nobody asked for, no helper with one caller. Every extra line is
    a line the next person has to read and someone has to keep working.

## Layout

Full version in architecture.md §10 and go-guidelines. The parts that are easy
to get wrong:

- `api/<group>/v1alpha1/` — API types only, one package per API group
  (`core`, `apps`, `platform`), because controller-gen takes one `+groupName`
  per package. Its dependency set is `k8s.io/apimachinery` and nothing else,
  because external clients import it.
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
- **Do not build phase 6 before phase 5.** VMs are the most seductive and least
  differentiating piece of the platform.

## Working agreements

- Verify before claiming. Run the command, paste the output. "Should work" is
  not a result.
- Say what you did not do. Scope reduction is the user's call, not yours.
- Keep changes inside the current phase unless asked. The roadmap's ordering
  constraints are load-bearing, not preferences.
