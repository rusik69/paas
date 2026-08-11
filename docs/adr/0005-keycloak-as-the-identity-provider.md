# ADR 0005 — Keycloak as the identity provider

- **Status:** accepted
- **Date:** 2026-08-11

## Context

[architecture.md §9](../architecture.md) left the OIDC provider open: "Keycloak, or Dex
fronting an upstream IdP". Phase 2 built Keycloak, and building it exposed what it costs.
Measured on the three-guest dev cluster:

- ~512 MiB resident once idle, and an allocation spike during its own startup large enough to
  be OOM-killed on a 4 GiB node it shared with etcd and the API server. The control-plane guest
  is 6 GiB because of it.
- A CloudNativePG database, so identity depends on the storage stack being up.
- About 75 seconds to Ready, plus the database bootstrap ahead of it — close enough to Flux's
  five-minute install timeout that a slow bring-up rolled the release back and started again.
- A four-line patch to the vendored chart, because it has no `hostNetwork` option.

Dex costs almost none of that: a Go binary of tens of megabytes, no database if it stores state
in Kubernetes, and a Deployment and ConfigMap rather than a vendored upstream chart.

So the weight argument favours Dex, and the question is what the weight buys.

## Decision

**Keycloak, because this platform is the identity provider rather than a client of one.**

Dex is a broker. It authenticates against an upstream — Google, GitHub, LDAP, an existing OIDC
issuer — and its local user store is static configuration, not something a reconciler creates a
tenant's users in. Choosing it means every tenant must arrive with an IdP of their own, or the
platform grows its own user management later.

A tenant of this platform gets a namespace tree, a plan, and a set of people who may use it.
Handing that last part to an upstream the tenant may not have is a product limitation, not an
implementation detail — and building the replacement later costs far more than the memory
Keycloak occupies.

## Consequences

**Good**

- A tenant needs no IdP of their own. The platform creates users and groups, and
  `paas:tenant:<name>` group membership is something the platform can put there rather than
  something it can only read.
- Self-service — password reset, profile, eventually per-tenant federation — comes with the
  product instead of being written.
- Keycloak can still front an upstream IdP for tenants who have one, so the Dex use case
  remains reachable. The reverse is not true.

**Bad**

- **The control plane carries a JVM it would not otherwise need**, and a second one beside the
  LINSTOR controller. That is the 6 GiB guest, and on a 14 GiB developer machine it is real.
- **Identity depends on CloudNativePG.** Phase 3 needs CNPG anyway, but it means a storage
  failure takes authentication with it, which a Dex storing state in Kubernetes would not.
- **Startup is slow enough to interact with timeouts.** It has already caused one reinstall
  loop, and any future change that slows bring-up further will find the same edge.
- The vendored chart carries a local patch, so its upgrades are no longer purely mechanical.

## Alternatives rejected

**Dex fronting an upstream IdP.** Lighter by an order of magnitude and the reason
architecture.md named it. Rejected because it makes "bring your own IdP" a requirement of being
a tenant. Worth revisiting if the product turns out to sell only to organisations that already
have one — the cost of switching is one package and the token half of one e2e test, and it does
not touch the API server's configuration or the RBAC bindings, which are what would have been
expensive.

**Zitadel.** A closer match to Keycloak's capability in a Go binary. Still wants Postgres, and
its Kubernetes-OIDC path is less trodden than Keycloak's. Not enough saving to justify the less
travelled road.

**Ory Hydra.** Light, but it delegates the user store entirely, which lands in the same place as
Dex with more assembly.

**Nothing — static kubeconfigs only.** What phase 2 had before this. Fine for CI, and it is
still how the CI kubeconfig works, but architecture.md rules it out for humans and it cannot
express tenant membership at all.
