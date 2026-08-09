# paas

A multi-tenant cloud platform for bare metal.

Tenants get three products, all as Kubernetes objects in their own namespace:

- **Managed data services** — Postgres, Kafka, Redis, ClickHouse, S3 buckets.
- **Apps** — push a git repo, get a built image, an HTTPS URL, autoscaling, logs.
- **VMs** — KubeVirt virtual machines, disks, and (later) tenant networks.

Built on Talos Linux, Kubernetes, Cilium, Piraeus/LINSTOR, KubeVirt, and Flux.

## Status

Design phase. No product code yet.

| Document | What it covers |
|---|---|
| [docs/architecture.md](docs/architecture.md) | The design: tenancy, API model, control plane, app plane, storage and network |
| [docs/adr/](docs/adr/) | The four decisions worth arguing about, with rejected alternatives |
| [docs/roadmap.md](docs/roadmap.md) | Build order, seven phases, each with a CI-checkable exit criterion |
| [docs/testing.md](docs/testing.md) | Test strategy from unit through e2e, isolation, upgrade, and restore drills |
| [docs/go-guidelines.md](docs/go-guidelines.md) | Go and controller conventions for this repo |

## Development

Everything through phase 4 runs on three local KVM guests booting Talos, driven by
`hack/e2e.sh`. No physical hardware is needed until phase 5. See
[docs/roadmap.md](docs/roadmap.md#development-and-test-environment) for prerequisites.
