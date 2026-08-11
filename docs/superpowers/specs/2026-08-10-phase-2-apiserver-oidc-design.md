# Phase 2, part 3 — making the API server trust Keycloak

- **Status:** proposed, not implemented
- **Date:** 2026-08-10
- **Covers:** the last of roadmap phase 2 — the OIDC provider actually being an
  authentication source, rather than a running pod
- **Depends on:** Keycloak and its CloudNativePG database, both landed and proven by
  `TestKeycloak_ServesItsDiscoveryDocument`

## What is already true

`tenant-admin` and `tenant-viewer` are bound to `paas:tenant:<name>` groups in every tenant
namespace, and Keycloak is running. Those bindings currently match nothing, because the API
server has never been told to trust any issuer. This is the link between the two.

## The constraint that decides the design

**The API server runs in the host network namespace and does not use cluster DNS.** It cannot
resolve `keycloak.paas-system.svc.cluster.local`, so the issuer URL cannot be a Service DNS
name — which is the obvious first choice and a dead end.

It *can* reach a Service ClusterIP: Cilium's socket load balancer programs the host namespace,
and phase 0 already proves it with `TestRegistry_ClusterIPReachableFromHostNetwork`, where
containerd on every node reaches the registry at a pinned `10.96.0.30`.

So Keycloak follows the registry's pattern: a **pinned ClusterIP**, and an issuer URL built
from it.

A second constraint compounds it: **Kubernetes requires the OIDC issuer to be HTTPS.** Keycloak
currently runs `start-dev` over plain HTTP, which is why this is not simply four extra flags.

## The design

**One CA, generated at bring-up.** `hack/e2e.sh` mints a CA and a serving certificate whose
SAN is the pinned IP, alongside the Talos PKI it already generates into `.e2e/`. Generated
rather than committed, because a committed private key is a private key in a public repository
whatever the README says about it being for tests.

**The API server is told about it through Talos machine config**, in
`hack/talos/controlplane.patch.yaml`:

- `machine.files` writes the CA bundle to a path on the control-plane node. The API server
  reads `--oidc-ca-file` from disk, so the CA has to exist as a file on the host — a Secret is
  not reachable from where the API server runs.
- `cluster.apiServer.extraArgs` carries `oidc-issuer-url`, `oidc-client-id`,
  `oidc-username-claim`, `oidc-groups-claim` and `oidc-ca-file`.

`oidc-groups-claim` must be the claim Keycloak actually emits, and Keycloak does not put group
membership in tokens by default — a group membership mapper has to be configured on the client,
or the claim is simply absent and every binding still matches nothing. This is the step most
likely to be skipped and to produce a silent no-op.

**Keycloak serves HTTPS** from the generated certificate, mounted as a Secret, with
`KC_HOSTNAME` set to the same `https://<ip>:<port>` the issuer URL uses. Keycloak stamps that
value into the `iss` claim, and the API server rejects a token whose issuer does not match its
configured URL exactly — including the port and any trailing path.

**The realm is provisioned by import**, not by hand: a realm export JSON in the Keycloak
package, mounted and imported at startup, defining the `paas` realm, a `kubernetes` client, a
groups mapper, and a test user in `paas:tenant:acme`.

## What proves it

An e2e that obtains a token from Keycloak for a user in `paas:tenant:acme`, builds a
kubeconfig from it, and then:

- reads the tenant's own namespace successfully;
- gets a **403** — not any error — from another tenant's namespace and from `kube-system`.

The positive half matters as much as the negative: a token that authenticates nobody produces
a 401 everywhere, which would pass a carelessly written negative test. The same trap the
network-policy tests already avoid with their controls.

## Findings from the attempt (2026-08-11)

Everything below the issuer's reachability now works and is committed: Keycloak serves TLS from
the generated CA, the realm imports, a token is issued, and the API server has its flags. Four
silent failure modes were found and fixed on the way — the management interface going TLS with
the client port and breaking the chart's HTTP probes; busybox's wget being unable to speak TLS
at all; a public client's `aud` claim carrying `account` rather than the client id; and a stale
certificate being reused for a changed address.

**What is still unsolved is reachability, and the cause is now narrowed.** The API server
cannot dial the issuer through *any* Kubernetes Service:

- a pinned ClusterIP was refused with `connect: operation not permitted`, with the Service's
  endpoints healthy;
- a NodePort on the control-plane's own address was refused the same way.

Both are EPERM from Cilium's socket load balancer. It translates service addresses for pods and
for host-network pods — `TestRegistry_ClusterIPReachableFromHostNetwork` proves that much — but
the `kube-apiserver` static pod does not get that translation, and the authenticator retries
every ten seconds saying so. The precedent that motivated the ClusterIP design does not
transfer, and neither does the NodePort fallback.

### Cilium is not the fault (measured 2026-08-11)

A `hostNetwork` pod pinned to the control-plane node reaches **both** addresses with HTTP 200 —
the ClusterIP and the NodePort — and `cilium-dbg service list` on that node shows both frontends
with an active backend. So the datapath, the service tables and host-namespace socket load
balancing all work on the very node the API server runs on.

What refuses the connection is therefore specific to the `kube-apiserver` static pod, not to
Cilium or to the node. Changing Cilium's values will not fix it, and the two rounds spent moving
the issuer between service types were looking in the wrong place.

**The remaining approach is to avoid service translation altogether**: run Keycloak with
`hostNetwork: true`, pinned to the control-plane node, binding the issuer port directly. That is
the measured recommendation rather than a guess: a host-network pod on that node is exactly
what was just shown to be reachable, and the API server dials a real listener on the
node's own address with no Service, no frontend and nothing to translate, while in-cluster clients
reach the same address because a node IP is routable from pods. The certificate SAN, the issuer
URL and `KC_HOSTNAME` all become that node address, and `hack/e2e.sh` already regenerates the
certificate when that address changes.

### The host-network issuer (built 2026-08-11)

Keycloak binds `10.77.0.11:8443` on the control-plane node's own network namespace. No Service
of any kind is in the path: the extra `keycloak-oidc` Service is gone, and `OIDC_PORT` is
Keycloak's own HTTPS port rather than a NodePort.

Three details this rests on:

- **The vendored chart had no `hostNetwork` option**, so it carries a four-line patch —
  `hostNetwork` plus `dnsPolicy: ClusterFirstWithHostNet`, marked `PAAS PATCH` in its
  `values.yaml`. The DNS policy is not optional: Keycloak resolves `keycloak-db-rw` through
  cluster DNS, which a host-network pod does not get by default, and without it the pod would
  fail at the database rather than at the issuer.
- **The control plane, not a worker.** The storage suite powers a worker off to prove DRBD
  failover, and pinning the cluster's authentication source to a node the tests destroy would
  make every later test's failure mode depend on ordering. The LINSTOR controller is pinned
  there for the same reason.
- **Scheduling on the control plane is off** (`allowSchedulingOnControlPlanes: false`), so this
  needs the `node-role.kubernetes.io/control-plane:NoSchedule` toleration alongside the
  nodeSelector. Piraeus already carries the same pair.

The Cilium-settings check the previous round suggested (`socketLB`, `bpf-lb-sock-hostns-only`)
was not run: it would only have salvaged a Service-based issuer, and the host-network shape
removes the Service rather than fixing translation for it. If the static pod's exclusion from
socket load balancing ever matters for something else, that is where to start.

## Risks

**A bad `--oidc-issuer-url` can stop the API server coming up**, and on a single control-plane
node that is the cluster. The flags should be added and a bring-up proven *before* anything
depends on them, and `hack/e2e.sh` should be run end to end on the first attempt rather than
layering the Keycloak TLS change on top of an unverified control-plane change.

**A pinned ClusterIP is a second hardcoded address**, after the registry's. Both now need to
stay inside the service CIDR and not collide. If a third appears, they belong in one place with
the CIDR that constrains them.

**The issuer is only reachable in-cluster.** A developer's `kubectl` on the host can reach the
ClusterIP only because Cilium programs the host namespace on cluster nodes — a laptop cannot.
Real deployments publish the issuer through the Gateway with a real certificate; this design is
honest for the dev cluster and should not be mistaken for the production shape.
