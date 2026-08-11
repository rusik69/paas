package serviceclass

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func testClass() *v1alpha1.ServiceClass {
	return &v1alpha1.ServiceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres"},
		Spec: v1alpha1.ServiceClassSpec{
			Kind:   "Postgres",
			Plural: "postgreses",
			Chart:  v1alpha1.ChartRef{Name: "postgres", Version: "0.1.0"},
			StatusFrom: []v1alpha1.StatusSource{{
				Path:     ".status.primary",
				From:     v1alpha1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
				JSONPath: ".status.currentPrimary",
			}},
		},
	}
}

func TestCRDFor(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object","properties":{"instances":{"type":"integer"}}}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}

	if got, want := crd.Name, "postgreses."+Group; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got := crd.Spec.Scope; got != "Namespaced" {
		t.Errorf("Scope = %q, want Namespaced", got)
	}
	if got, want := crd.Spec.Names.Kind, "Postgres"; got != want {
		t.Errorf("Kind = %q, want %q", got, want)
	}

	v := crd.Spec.Versions[0]
	if !v.Served || !v.Storage {
		t.Error("the single version must be both served and storage")
	}
	if v.Subresources == nil || v.Subresources.Status == nil {
		t.Error("status subresource is off — kubectl get would report nothing real")
	}

	spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		t.Fatal("generated CRD has no spec")
	}
	if _, ok := spec.Properties["instances"]; !ok {
		t.Error("the chart's schema did not become the spec schema")
	}
	if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"]; !ok {
		t.Error("generated CRD has no status")
	}
	if v.Schema.OpenAPIV3Schema.XPreserveUnknownFields != nil {
		t.Error("preserve-unknown-fields is set at the root")
	}
}

func TestCRDFor_StatusFromBecomesAPrinterColumn(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}
	var found bool
	for _, c := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		if c.JSONPath == ".status.primary" {
			found = true
		}
	}
	if !found {
		t.Error("the first statusFrom path is not a printer column, so kubectl get shows no primary")
	}
}

func TestCRDFor_RejectsUnrepresentableSchema(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object","properties":{"a":{"$ref":"#/x"}}}`))
	if err == nil {
		t.Fatal("CRDFor built a CRD from a schema it cannot represent")
	}
	if crd != nil {
		t.Error("CRDFor returned a CRD alongside its error")
	}
}

func TestGVKFor(t *testing.T) {
	gvk := GVKFor(testClass())
	if gvk.Group != Group || gvk.Version != Version || gvk.Kind != "Postgres" {
		t.Errorf("GVKFor = %v, want %s/%s Postgres", gvk, Group, Version)
	}
}
