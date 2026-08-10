//go:build integration

package crd

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func pkg(name string, stage v1alpha1.PackageStage) *v1alpha1.Package {
	return &v1alpha1.Package{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PackageSpec{
			SourceRef: v1alpha1.LocalRef{Name: "platform"},
			Chart:     "cilium",
			Version:   "1.18.12",
			Stage:     stage,
		},
	}
}

func TestPackage_AcceptsBothStages(t *testing.T) {
	installCRDs(t)

	for _, stage := range []v1alpha1.PackageStage{v1alpha1.StageMigration, v1alpha1.StageComponent} {
		t.Run(string(stage), func(t *testing.T) {
			p := pkg("cilium-"+string(stage), stage)
			if err := k8sClient.Create(t.Context(), p); err != nil {
				t.Fatalf("create package with stage %s: %v", stage, err)
			}
			t.Cleanup(func() {
				if err := k8sClient.Delete(context.Background(), p); err != nil {
					t.Logf("cleanup: delete package: %v", err)
				}
			})
		})
	}
}

func TestPackage_RejectsAnUnknownStage(t *testing.T) {
	installCRDs(t)

	err := k8sClient.Create(t.Context(), pkg("cilium-bogus", v1alpha1.PackageStage("bogus")))
	if err == nil {
		t.Fatal("an unknown stage was accepted; the enum is not in effect")
	}
	if !strings.Contains(err.Error(), "spec.stage") {
		t.Errorf("err = %q, want it to name spec.stage", err)
	}
}

// The default exists so a PackageSource created by the Platform reconciler does
// not have to restate it, and a default that silently stops applying is the
// kind of thing nothing notices until polling stops.
func TestPackageSource_DefaultsTheInterval(t *testing.T) {
	installCRDs(t)

	src := &v1alpha1.PackageSource{
		ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec:       v1alpha1.PackageSourceSpec{URL: "oci://registry.paas.io/paas"},
	}
	if err := k8sClient.Create(t.Context(), src); err != nil {
		t.Fatalf("create packagesource: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(context.Background(), src); err != nil {
			t.Logf("cleanup: delete packagesource: %v", err)
		}
	})

	if got := src.Spec.Interval.Duration.String(); got != "5m0s" {
		t.Errorf("interval = %s, want 5m0s from the CRD default", got)
	}
}

func TestPackageSource_RejectsANonOCIURL(t *testing.T) {
	installCRDs(t)

	src := &v1alpha1.PackageSource{
		ObjectMeta: metav1.ObjectMeta{Name: "bad"},
		Spec:       v1alpha1.PackageSourceSpec{URL: "https://registry.paas.io/paas"},
	}
	err := k8sClient.Create(t.Context(), src)
	if err == nil {
		t.Fatal("a non-oci:// url was accepted")
	}
	if !strings.Contains(err.Error(), "spec.url") {
		t.Errorf("err = %q, want it to name spec.url", err)
	}
}
