//go:build integration

package crd

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func platform(name, registry string) *v1alpha1.Platform {
	return &v1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PlatformSpec{
			Version:  "v1.4.2",
			Registry: registry,
		},
	}
}

const ociRegistry = "oci://registry.paas.io/paas"

func TestPlatform_AcceptsTheSingletonName(t *testing.T) {
	installCRDs(t)

	p := platform("cluster", ociRegistry)
	if err := k8sClient.Create(t.Context(), p); err != nil {
		t.Fatalf("create the singleton Platform: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(context.Background(), p); err != nil {
			t.Logf("cleanup: delete platform: %v", err)
		}
	})
}

// Asserts the specific denial. A test that accepts any error keeps passing
// after the rule it guards is deleted.
func TestPlatform_RejectsAnyOtherName(t *testing.T) {
	installCRDs(t)

	err := k8sClient.Create(t.Context(), platform("notcluster", ociRegistry))
	if err == nil {
		t.Fatal("a second Platform name was accepted; the singleton rule is not in effect")
	}
	if !strings.Contains(err.Error(), "must be named cluster") {
		t.Errorf("err = %q, want it to name the singleton rule", err)
	}
}

func TestPlatform_RejectsARegistryThatIsNotOCI(t *testing.T) {
	installCRDs(t)

	err := k8sClient.Create(t.Context(), platform("cluster", "https://registry.paas.io/paas"))
	if err == nil {
		t.Fatal("a non-oci:// registry was accepted")
	}
	if !strings.Contains(err.Error(), "spec.registry") {
		t.Errorf("err = %q, want it to name spec.registry", err)
	}
}

func TestPlatform_RejectsAnEmptyVersion(t *testing.T) {
	installCRDs(t)

	p := platform("cluster", ociRegistry)
	p.Spec.Version = ""
	err := k8sClient.Create(t.Context(), p)
	if err == nil {
		t.Fatal("an empty version was accepted")
	}
	if !strings.Contains(err.Error(), "spec.version") {
		t.Errorf("err = %q, want it to name spec.version", err)
	}
}
