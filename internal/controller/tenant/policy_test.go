package tenant

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
	"github.com/rusik69/paas/internal/controller/service"
)

const namespaceLabel = "k8s:io.kubernetes.pod.namespace"

func policyRules(t *testing.T, policy *unstructured.Unstructured, direction string) []any {
	t.Helper()

	got, found, err := unstructured.NestedSlice(policy.Object, "spec", direction)
	if err != nil || !found {
		t.Fatalf("spec.%s: found %t, err %v", direction, found, err)
	}
	return got
}

// selectedNamespaces returns the namespace each rule's endpoint selector names,
// skipping the empty selector that means "this namespace".
func selectedNamespaces(t *testing.T, policy *unstructured.Unstructured, direction, field string) []string {
	t.Helper()

	var out []string
	for _, rule := range policyRules(t, policy, direction) {
		endpoints, found, err := unstructured.NestedSlice(rule.(map[string]any), field)
		if err != nil || !found {
			continue
		}
		for _, endpoint := range endpoints {
			labels, _, _ := unstructured.NestedStringMap(endpoint.(map[string]any), "matchLabels")
			if ns, ok := labels[namespaceLabel]; ok {
				out = append(out, ns)
			}
		}
	}
	return out
}

// The platform's operators live in paas-system and must reach the workloads
// they provision — CNPG scraping an instance's status endpoint is the case that
// found this — so one policy names that namespace as an ingress source, in the
// label form Cilium matches namespaces with.
func TestAllowPlatform_AllowsIngressFromThePlatformNamespace(t *testing.T) {
	t.Parallel()

	allow := allowPlatformPolicy("tenant-acme", "acme")

	var found bool
	for _, rule := range policyRules(t, allow, "ingress") {
		endpoints, ok, err := unstructured.NestedSlice(rule.(map[string]any), "fromEndpoints")
		if err != nil || !ok || len(endpoints) != 1 {
			continue
		}
		labels, _, _ := unstructured.NestedStringMap(endpoints[0].(map[string]any), "matchLabels")
		want := map[string]string{namespaceLabel: pkgctl.TargetNamespace}
		if diff := cmp.Diff(want, labels); diff != "" {
			t.Errorf("platform ingress selector differs (-want +got):\n%s", diff)
		}
		found = true
	}
	if !found {
		t.Errorf("no ingress rule selects %s; the platform's operators cannot manage what they provision",
			pkgctl.TargetNamespace)
	}
}

// The allowance reaches the instances the platform provisions, not everything
// the tenant runs: the endpoint selector is the chart-contract label every
// catalog chart stamps on what it creates, so a tenant's own pods are no more
// reachable from paas-system than from another tenant.
func TestAllowPlatform_SelectsOnlyManagedInstances(t *testing.T) {
	t.Parallel()

	allow := allowPlatformPolicy("tenant-acme", "acme")

	selector, found, err := unstructured.NestedMap(allow.Object, "spec", "endpointSelector")
	if err != nil || !found {
		t.Fatalf("spec.endpointSelector: found %t, err %v", found, err)
	}
	if len(selector) == 0 {
		t.Fatal("endpointSelector is empty, so the allowance reaches every pod in the namespace")
	}

	expressions, found, err := unstructured.NestedSlice(allow.Object, "spec", "endpointSelector", "matchExpressions")
	if err != nil || !found || len(expressions) != 1 {
		t.Fatalf("matchExpressions = %v (found %t, err %v), want one", expressions, found, err)
	}
	want := map[string]any{"key": "k8s:" + service.LabelServiceName, "operator": "Exists"}
	if diff := cmp.Diff(want, expressions[0]); diff != "" {
		t.Errorf("endpoint selector differs (-want +got):\n%s", diff)
	}
}

// Ingress only, in its own object. An egress rule would let a tenant's managed
// instance dial into paas-system, which nothing has needed.
func TestAllowPlatform_GrantsNoEgress(t *testing.T) {
	t.Parallel()

	allow := allowPlatformPolicy("tenant-acme", "acme")

	if _, found, _ := unstructured.NestedSlice(allow.Object, "spec", "egress"); found {
		t.Errorf("the platform allowance carries egress rules: %v", allow.Object)
	}
}

// Three objects, not two rules inside one: the deny stays a deny, and either
// allowance can be revoked by deleting one policy.
func TestPoliciesFor_CarriesTheDenyAndBothAllowances(t *testing.T) {
	t.Parallel()

	var got []string
	for _, p := range policiesFor("tenant-acme", "acme") {
		got = append(got, p.GetName())
	}
	want := []string{PolicyDefaultDeny, PolicyAllowAPIServer, PolicyAllowPlatform}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("policies differ (-want +got):\n%s", diff)
	}
}

// The allowance is ingress only. Nothing has shown a tenant pod needs to dial
// into paas-system, and an egress rule would widen the boundary for free.
func TestDefaultDeny_GrantsNoEgressToThePlatformNamespace(t *testing.T) {
	t.Parallel()

	deny := defaultDenyPolicy("tenant-acme", "acme")

	want := []string{"kube-system"}
	got := selectedNamespaces(t, deny, "egress", "toEndpoints")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("egress namespaces differ (-want +got):\n%s", diff)
	}
}

// The deny names no namespace at all now that the platform allowance is its
// own object: ingress is the tenant's own namespace and nothing else, so every
// other namespace — another tenant's, or paas-system for an unlabelled pod —
// is denied by absence.
func TestDefaultDeny_KeepsCrossNamespaceIngressDenied(t *testing.T) {
	t.Parallel()

	deny := defaultDenyPolicy("tenant-acme", "acme")

	if got := selectedNamespaces(t, deny, "ingress", "fromEndpoints"); len(got) != 0 {
		t.Errorf("ingress names %v; the deny policy should select no namespace but this one", got)
	}

	// Nor by an entity selector, which would sweep in every endpoint in the
	// cluster regardless of namespace.
	rendered := fmt.Sprint(deny.Object)
	for _, wildcard := range []string{"fromEntities", "toEntities"} {
		if strings.Contains(rendered, wildcard) {
			t.Errorf("the deny policy contains %q, a cluster-wide allowance: %s", wildcard, rendered)
		}
	}
}
