package tenant

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
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
// found this — so the deny policy carries one ingress rule naming that
// namespace, in the label form Cilium matches namespaces with.
func TestDefaultDeny_AllowsIngressFromThePlatformNamespace(t *testing.T) {
	t.Parallel()

	deny := defaultDenyPolicy("tenant-acme", "acme")

	var found bool
	for _, rule := range policyRules(t, deny, "ingress") {
		endpoints, ok, err := unstructured.NestedSlice(rule.(map[string]any), "fromEndpoints")
		if err != nil || !ok || len(endpoints) != 1 {
			continue
		}
		labels, _, _ := unstructured.NestedStringMap(endpoints[0].(map[string]any), "matchLabels")
		if len(labels) == 0 {
			continue
		}
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

// The platform allowance must not have become a general cross-namespace
// allowance: paas-system is the only namespace ingress names, so another
// tenant's namespace is still denied by absence.
func TestDefaultDeny_KeepsCrossTenantIngressDenied(t *testing.T) {
	t.Parallel()

	deny := defaultDenyPolicy("tenant-acme", "acme")

	want := []string{pkgctl.TargetNamespace}
	got := selectedNamespaces(t, deny, "ingress", "fromEndpoints")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ingress namespaces differ (-want +got):\n%s", diff)
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
