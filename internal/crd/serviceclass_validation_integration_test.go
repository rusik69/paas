//go:build integration

package crd

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func serviceClass(name, kind, plural string) *v1alpha1.ServiceClass {
	return &v1alpha1.ServiceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ServiceClassSpec{
			Kind:   kind,
			Plural: plural,
			Chart:  v1alpha1.ChartRef{Name: name, Version: "0.1.0"},
		},
	}
}

// The CRD a class generates is named from kind and plural, and the engine only
// ever stops the kind the class currently names. Editing either would therefore
// leave the previous kind's CRD serving with a controller nothing can reach to
// stop, and a second one running beside it — so the API server refuses the edit
// rather than the reconciler coping with it.
func TestServiceClass_KindAndPluralAreImmutable(t *testing.T) {
	installCRDs(t)

	for _, tc := range []struct {
		name  string
		edit  func(*v1alpha1.ServiceClass)
		field string
	}{
		{"kind", func(sc *v1alpha1.ServiceClass) { sc.Spec.Kind = "Postgresql" }, "kind"},
		{"plural", func(sc *v1alpha1.ServiceClass) { sc.Spec.Plural = "postgresqls" }, "plural"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := serviceClass("immutable-"+tc.name, "Postgres", "postgreses"+tc.name)
			if err := k8sClient.Create(t.Context(), sc); err != nil {
				t.Fatalf("create serviceclass: %v", err)
			}
			t.Cleanup(func() {
				if err := k8sClient.Delete(context.Background(), sc); err != nil {
					t.Logf("cleanup: delete serviceclass: %v", err)
				}
			})

			tc.edit(sc)
			err := k8sClient.Update(t.Context(), sc)
			if err == nil {
				t.Fatalf("spec.%s was edited; the immutability rule is not in effect", tc.name)
			}
			if !strings.Contains(err.Error(), tc.field+" is immutable") {
				t.Errorf("err = %q, want it to say %s is immutable", err, tc.field)
			}
		})
	}
}

// The chart version is the one field of the spec that must move: a catalog
// upgrade is a version bump, and the reconciler restarts the kind's controller
// on it.
func TestServiceClass_ChartVersionIsMutable(t *testing.T) {
	installCRDs(t)

	sc := serviceClass("mutable-chart", "Redis", "redises")
	if err := k8sClient.Create(t.Context(), sc); err != nil {
		t.Fatalf("create serviceclass: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(context.Background(), sc); err != nil {
			t.Logf("cleanup: delete serviceclass: %v", err)
		}
	})

	sc.Spec.Chart.Version = "0.2.0"
	if err := k8sClient.Update(t.Context(), sc); err != nil {
		t.Errorf("bump chart version: %v — a catalog upgrade would be rejected", err)
	}
}
