package tenant

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
)

// Cilium's policy API, referenced rather than imported: CiliumNetworkPolicy is
// a small, stable schema, and pulling in cilium/cilium would drag its whole
// k8s.io dependency set into a module already pinned to a matched pair.
const (
	policyAPIVersion = "cilium.io/v2"
	policyKind       = "CiliumNetworkPolicy"
)

// Policy names. Two objects rather than one, so the opt-in can be reasoned
// about — and revoked — without touching the deny.
const (
	// PolicyDefaultDeny confines a tenant to its own namespace.
	PolicyDefaultDeny = "default-deny"
	// PolicyAllowAPIServer grants labelled pods access to the API server.
	PolicyAllowAPIServer = "allow-to-apiserver"
)

// AllowAPIServerLabel opts a single pod into API server access.
//
// Per-pod rather than per-tenant: a tenant that runs one controller needing the
// API server should not thereby grant it to every workload it runs.
const AllowAPIServerLabel = "policy.paas.io/allow-to-apiserver"

// defaultDenyPolicy confines every pod in the namespace to its own namespace,
// plus cluster DNS and ingress from the platform namespace.
//
// Deny-by-default is a property of Cilium selecting the endpoint at all: once a
// policy matches, everything not explicitly allowed is dropped. So the rules
// below are the entire allow-list — same-namespace traffic, the platform's
// operators, and DNS — and cross-tenant traffic and the API server are denied
// by their absence rather than by a deny rule.
func defaultDenyPolicy(namespace, tenant string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": policyAPIVersion,
		"kind":       policyKind,
		"metadata": map[string]any{
			"name":      PolicyDefaultDeny,
			"namespace": namespace,
			"labels":    map[string]any{TenantLabel: tenant},
		},
		"spec": map[string]any{
			// Every endpoint in the namespace.
			"endpointSelector": map[string]any{},
			"ingress": []any{
				// An empty endpoint selector means "the same namespace" in
				// Cilium, which is exactly the boundary a tenant is.
				map[string]any{
					"fromEndpoints": []any{map[string]any{}},
				},
				// The platform's operators run in paas-system and manage the
				// workloads they provision for tenants: CNPG polls each Postgres
				// instance's status endpoint in the tenant's own namespace, and
				// every managed service added later needs the same reach. Without
				// this the operator's probes time out and the service never
				// becomes ready. Ingress only — nothing has needed a tenant pod to
				// dial into paas-system, and this stays the narrower half.
				map[string]any{
					"fromEndpoints": []any{map[string]any{
						"matchLabels": map[string]any{
							"k8s:io.kubernetes.pod.namespace": pkgctl.TargetNamespace,
						},
					}},
				},
			},
			"egress": []any{
				map[string]any{
					"toEndpoints": []any{map[string]any{}},
				},
				// DNS, or nothing in the namespace can resolve its own services.
				map[string]any{
					"toEndpoints": []any{map[string]any{
						"matchLabels": map[string]any{
							"k8s:io.kubernetes.pod.namespace": "kube-system",
							"k8s:k8s-app":                     "kube-dns",
						},
					}},
					"toPorts": []any{map[string]any{
						"ports": []any{
							map[string]any{"port": "53", "protocol": "UDP"},
							map[string]any{"port": "53", "protocol": "TCP"},
						},
					}},
				},
			},
		},
	}}
}

// allowAPIServerPolicy grants API server access to pods that ask for it by
// label.
//
// A separate object selecting a label, because Cilium unions allow rules: this
// adds a permission to the pods it selects without weakening the deny for
// everything else in the namespace.
func allowAPIServerPolicy(namespace, tenant string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": policyAPIVersion,
		"kind":       policyKind,
		"metadata": map[string]any{
			"name":      PolicyAllowAPIServer,
			"namespace": namespace,
			"labels":    map[string]any{TenantLabel: tenant},
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{
				"matchLabels": map[string]any{AllowAPIServerLabel: "true"},
			},
			"egress": []any{
				// The entity, not a CIDR: the API server's address is not
				// stable and a CIDR would be wrong after any control-plane
				// change.
				map[string]any{"toEntities": []any{"kube-apiserver"}},
			},
		},
	}}
}

// policiesFor returns every policy backing one tenant namespace.
func policiesFor(namespace, tenant string) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		defaultDenyPolicy(namespace, tenant),
		allowAPIServerPolicy(namespace, tenant),
	}
}
