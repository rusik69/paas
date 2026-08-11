package tenant

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
	"github.com/rusik69/paas/internal/controller/service"
)

// Cilium's policy API, referenced rather than imported: CiliumNetworkPolicy is
// a small, stable schema, and pulling in cilium/cilium would drag its whole
// k8s.io dependency set into a module already pinned to a matched pair.
const (
	policyAPIVersion = "cilium.io/v2"
	policyKind       = "CiliumNetworkPolicy"
	// k8sLabelPrefix is the label source Cilium files a Kubernetes label under.
	k8sLabelPrefix = "k8s:"
)

// Policy names. One object per allowance rather than one object total, so each
// can be reasoned about — and revoked — without touching the deny.
const (
	// PolicyDefaultDeny confines a tenant to its own namespace.
	PolicyDefaultDeny = "default-deny"
	// PolicyAllowAPIServer grants labelled pods access to the API server.
	PolicyAllowAPIServer = "allow-to-apiserver"
	// PolicyAllowPlatform lets the platform's operators reach the instances
	// they provision.
	PolicyAllowPlatform = "allow-from-platform"
)

// AllowAPIServerLabel opts a single pod into API server access.
//
// Per-pod rather than per-tenant: a tenant that runs one controller needing the
// API server should not thereby grant it to every workload it runs.
const AllowAPIServerLabel = "policy.paas.io/allow-to-apiserver"

// defaultDenyPolicy confines every pod in the namespace to its own namespace,
// plus cluster DNS.
//
// Deny-by-default is a property of Cilium selecting the endpoint at all: once a
// policy matches, everything not explicitly allowed is dropped. So the rules
// below are the entire allow-list — same-namespace traffic and DNS — and
// cross-tenant traffic, the platform namespace and the API server are denied by
// their absence rather than by a deny rule.
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

// allowPlatformPolicy lets the platform namespace reach the workloads it
// provisions inside this tenant's namespace.
//
// The platform's operators manage the instances a tenant asks for — CNPG polls
// each Postgres instance's status endpoint in the tenant's own namespace, and
// every managed service added later needs the same reach. No per-pod opt-in can
// express it: the source is the operator's namespace, not anything the tenant
// runs.
//
// Narrowed to the endpoints that are a managed instance, by the chart-contract
// label every catalog chart already stamps on everything it creates (see
// service.LabelServiceName, and CNPG's inheritedMetadata in
// packages/apps/postgres). A tenant's own pods carry no such label and stay
// unreachable from paas-system, which is what the allowance should have said
// all along.
//
// Ingress only: nothing has needed a tenant pod to dial into paas-system, and
// its own object rather than a rule inside the deny, so revoking it is deleting
// one policy.
func allowPlatformPolicy(namespace, tenant string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": policyAPIVersion,
		"kind":       policyKind,
		"metadata": map[string]any{
			"name":      PolicyAllowPlatform,
			"namespace": namespace,
			"labels":    map[string]any{TenantLabel: tenant},
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{
				// Explicitly source-prefixed, as the namespace selectors here
				// are. Cilium normalises a bare key in matchLabels to the "any"
				// source; whether it does the same inside matchExpressions is
				// not something to find out from a policy that silently selects
				// nothing.
				"matchExpressions": []any{map[string]any{
					"key":      k8sLabelPrefix + service.LabelServiceName,
					"operator": "Exists",
				}},
			},
			"ingress": []any{map[string]any{
				"fromEndpoints": []any{map[string]any{
					"matchLabels": map[string]any{
						"k8s:io.kubernetes.pod.namespace": pkgctl.TargetNamespace,
					},
				}},
			}},
		},
	}}
}

// policiesFor returns every policy backing one tenant namespace.
func policiesFor(namespace, tenant string) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		defaultDenyPolicy(namespace, tenant),
		allowAPIServerPolicy(namespace, tenant),
		allowPlatformPolicy(namespace, tenant),
	}
}
