//go:build integration

package crd

import (
	"context"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
)

func applyAll(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func getCRD(t *testing.T, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get crd %s: %v", name, err)
	}
	return got
}

func TestApply_InstallsAndEstablishesEveryCRD(t *testing.T) {
	applyAll(t)

	want, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, w := range want {
		got := getCRD(t, w.Name)
		var established bool
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		if !established {
			t.Errorf("crd %s is not Established: %+v", w.Name, got.Status.Conditions)
		}
	}
}

// The level-triggered claim, tested rather than asserted.
func TestApply_IsIdempotent(t *testing.T) {
	applyAll(t)
	before := getCRD(t, "platforms.platform.paas.io")

	applyAll(t)
	after := getCRD(t, "platforms.platform.paas.io")

	if before.Generation != after.Generation {
		t.Errorf("generation moved %d -> %d on a no-op apply", before.Generation, after.Generation)
	}
}

// Drift in a field the operator owns. Another manager writing it makes the
// next apply a conflict, and without ForceOwnership that conflict is one the
// operator can never resolve on its own — it would wedge here for good.
func TestApply_RestoresDriftInFieldsItOwns(t *testing.T) {
	applyAll(t)

	const name = "platforms.platform.paas.io"
	want := len(getCRD(t, name).Spec.Versions[0].AdditionalPrinterColumns)
	if want == 0 {
		t.Fatal("fixture has no printer columns to drift")
	}

	drifted := getCRD(t, name)
	drifted.Spec.Versions[0].AdditionalPrinterColumns = nil
	if err := k8sClient.Update(t.Context(), drifted); err != nil {
		t.Fatalf("simulate a manual edit: %v", err)
	}
	if got := len(getCRD(t, name).Spec.Versions[0].AdditionalPrinterColumns); got != 0 {
		t.Fatalf("the edit did not take: %d columns remain", got)
	}

	applyAll(t)

	if got := len(getCRD(t, name).Spec.Versions[0].AdditionalPrinterColumns); got != want {
		t.Errorf("printer columns = %d after re-apply, want %d — drift was not corrected", got, want)
	}
}

// Server-side apply manages only the fields it specifies, so a field the
// operator never sets is left where another manager put it. Documented as a
// test because the obvious reading of "the operator owns these CRDs" is that
// apply is a replace, and someone will otherwise file the difference as a bug.
func TestApply_LeavesFieldsItDoesNotSpecify(t *testing.T) {
	applyAll(t)

	const name = "platforms.platform.paas.io"
	drifted := getCRD(t, name)
	drifted.Spec.Names.ShortNames = []string{"plat"}
	if err := k8sClient.Update(t.Context(), drifted); err != nil {
		t.Fatalf("add a shortName the manifest does not set: %v", err)
	}

	applyAll(t)

	var found bool
	for _, s := range getCRD(t, name).Spec.Names.ShortNames {
		if s == "plat" {
			found = true
		}
	}
	if !found {
		t.Error("a field the manifest never sets was removed; apply is behaving as a replace")
	}
}

// The path cmd/paas-operator takes, exercised end to end against a real
// apiserver so the binary's only untested part is its flag wiring.
func TestInstall_FromARestConfig(t *testing.T) {
	n, err := Install(t.Context(), restCfg, 2*time.Minute)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != len(want) {
		t.Errorf("Install reported %d CRDs, want %d", n, len(want))
	}
}

func TestInstall_ReportsAnUnreachableAPIServer(t *testing.T) {
	bad := *restCfg
	bad.Host = "https://127.0.0.1:1"

	if _, err := Install(t.Context(), &bad, 2*time.Second); err == nil {
		t.Error("an unreachable apiserver was reported as a successful install")
	}
}
