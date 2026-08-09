# ADR 0002 — Cilium as the single dataplane

- **Status:** proposed
- **Date:** 2026-08-09

## Context

A bare-metal platform has to solve four networking problems that a managed cloud hands you for
free:

1. **Pod networking** — a CNI.
2. **Service exposure** — `type: LoadBalancer` has to allocate and advertise a real IP,
   because there is no cloud load balancer.
3. **Tenant isolation** — enforced network policy between namespaces, since namespace-level
   isolation is our entire security model.
4. **Ingress routing** — HTTP routing with TLS for tenant apps.

Later, VMs add a fifth: **tenant L2/VPC networking** — isolated subnets, floating IPs, tenant
routers.

The conventional bare-metal answer assembles a specialist for each: Kube-OVN for VM and VPC
networking, Cilium for policy and Gateway API, MetalLB for bare-metal service IPs. Each is
the best tool for its own job.

That works, but it means three control planes, three failure modes, three upgrade cadences,
and a debugging story where the first question is always "which layer dropped the packet".
For a small team standing up a platform from nothing, that cost lands early and stays.

## Decision

**Cilium alone, for as long as it suffices.**

| Problem | Cilium feature |
|---|---|
| Pod networking | eBPF CNI, kube-proxy replacement |
| Service exposure | BGP control plane, peering with top-of-rack — **no MetalLB** |
| Tenant isolation | `CiliumNetworkPolicy`, default-deny across namespaces |
| Ingress routing | Cilium Gateway API implementation |
| Observability | Hubble flow metrics — which also supply per-tenant egress bytes for billing |

**Kube-OVN is deferred to phase 6**, and introduced only for tenant VPC networking, attached
to VMs via Multus as a secondary interface. It does not become the pod CNI.

## Consequences

**Good**

- One dataplane, one set of upgrade notes, one place to look when a packet vanishes.
- MetalLB disappears entirely; Cilium BGP covers the same ground and integrates with the
  policy layer.
- Hubble gives per-namespace egress accounting for free. Metering that would otherwise need a
  separate collector falls out of the CNI we already run.
- eBPF kube-proxy replacement removes iptables scaling behaviour from the equation early,
  which matters at high service counts.

**Bad**

- **BGP peering is a hard dependency on the network team.** If the top-of-rack switches will
  not peer, service IP advertisement has no fallback and phase 0 stalls. This must be
  confirmed against the hardware baseline before phase 0 starts, not discovered during it.
- Cilium's Gateway API implementation is less battle-worn for exotic HTTP routing than
  ingress-nginx. If a tenant needs something it cannot express, the escape hatch is running
  ingress-nginx as a tenant-level `extra` behind the Gateway, not replacing the dataplane.
- Deferring Kube-OVN means the phase 6 VM networking design is not proven now. Accepted:
  the alternative is carrying an unused overlay through four phases.
- Adding Kube-OVN later, alongside a running Cilium, is genuinely more disruptive than having
  installed both from the start. This is the main risk of the decision and the reason phase 6
  gets a dedicated networking spike.

## Alternatives rejected

**Kube-OVN + Cilium + MetalLB from day one.** The end state is more capable and is the right
answer for a platform whose primary product is VMs with real VPCs. Ours is
managed services and apps; VMs are phase 6. Paying the operational cost from phase 0 for a
capability first needed in phase 6 is the wrong trade for this product.

**Calico + MetalLB + ingress-nginx.** The conventional, boring choice, and defensible. Loses
Hubble-based egress metering, needs a fourth component for Gateway API, and does not remove
iptables from the service path. The consolidation Cilium offers is worth more than the
familiarity.

**Cloud-style overlay with no BGP** (e.g. L2 announcements). Simpler to bring up and avoids
the network-team dependency, but L2 mode does not scale past a single broadcast domain and
constrains rack topology permanently. Reasonable as a *temporary* phase 0 fallback if BGP
peering is delayed — but it must be logged as debt, not accepted as the design.
