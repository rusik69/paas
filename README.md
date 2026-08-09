# paas

A multi-tenant cloud platform for bare metal.

Tenants get three products, all as Kubernetes objects in their own namespace:

- **Managed data services** — Postgres, Kafka, Redis, ClickHouse, S3 buckets.
- **Apps** — push a git repo, get a built image, an HTTPS URL, autoscaling, logs.
- **VMs** — KubeVirt virtual machines, disks, and (later) tenant networks.

Built on Talos Linux, Kubernetes, Cilium, Piraeus/LINSTOR, KubeVirt, and Flux.

## Status

Phase 0 — foundation. The e2e harness, Talos install flow, Cilium and
Piraeus/DRBD are in place; no platform code yet.

| Document | What it covers |
|---|---|
| [docs/architecture.md](docs/architecture.md) | The design: tenancy, API model, control plane, app plane, storage and network |
| [docs/adr/](docs/adr/) | The four decisions worth arguing about, with rejected alternatives |
| [docs/roadmap.md](docs/roadmap.md) | Build order, seven phases, each with a CI-checkable exit criterion |
| [docs/testing.md](docs/testing.md) | Test strategy from unit through e2e, isolation, upgrade, and restore drills |
| [docs/go-guidelines.md](docs/go-guidelines.md) | Go and controller conventions for this repo |
| [AGENTS.md](AGENTS.md) | Short-form conventions, commands and repo-specific traps, for coding agents |

## Development

Everything through phase 4 runs on three local KVM guests booting Talos, driven by
`hack/e2e.sh`. No physical hardware is needed until phase 5. See
[docs/roadmap.md](docs/roadmap.md#development-and-test-environment) for prerequisites.

```sh
make deps            # report missing tooling
make deps-install    # install it (needs sudo; log out and back in for group changes)
make versions        # confirm every pinned upstream version still resolves

make test            # unit tests, race detector on, under ten seconds

make cluster-up      # three Talos guests, Cilium, Piraeus, StorageClasses
make e2e             # the Go assertions, including the replicated-3 failover test
make cluster-down
```

`hack/e2e.sh` provisions infrastructure and asserts nothing; every assertion lives in Go
under [test/e2e/](test/e2e/). Pinned versions live in one place,
[hack/versions.sh](hack/versions.sh).
