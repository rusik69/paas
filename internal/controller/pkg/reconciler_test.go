package pkg

import (
	"context"
	"errors"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, helmv2.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return s
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func component(name string) *v1alpha1.Package {
	return &v1alpha1.Package{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{PlatformLabel: "rel"}},
		Spec: v1alpha1.PackageSpec{
			SourceRef: v1alpha1.LocalRef{Name: "platform"},
			Chart:     "cnpg",
			Version:   "1.0.0",
			Stage:     v1alpha1.StageComponent,
		},
	}
}

func TestReconcile_ReturnsAReadFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("cnpg"))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
}

// Without the sibling list a component's dependsOn cannot be computed, and
// rendering it anyway would drop the ordering guarantee silently.
func TestReconcile_ReturnsAListFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("list refused")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(component("cnpg")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("cnpg"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the list failure", err)
	}
	if !strings.Contains(err.Error(), "cnpg") {
		t.Errorf("err = %q, want it to name the package", err)
	}
}

func TestReconcile_NamesTheReleaseItFailedToApply(t *testing.T) {
	t.Parallel()

	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(component("cnpg")).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("cnpg"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	if !strings.Contains(err.Error(), "cnpg") {
		t.Errorf("err = %q, want it to name the release", err)
	}
}

func TestReconcile_FailsWhenTheOwnerCannotBeSet(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(component("cnpg")).Build()

	_, err := (&Reconciler{Client: c, Scheme: runtime.NewScheme()}).Reconcile(t.Context(), request("cnpg"))
	if err == nil {
		t.Fatal("an unowned HelmRelease was rendered")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("err = %q, want it to name the owner reference", err)
	}
}

// Values are opaque here: the schema that constrains them lives in the chart.
// This only pins that they are carried through rather than dropped.
func TestReconcile_CarriesValuesThrough(t *testing.T) {
	t.Parallel()

	p := component("cnpg")
	p.Spec.Values = &runtime.RawExtension{Raw: []byte(`{"replicas":3}`)}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: testScheme(t)}

	got, err := r.desired(p, nil)
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if got.Spec.Values == nil || string(got.Spec.Values.Raw) != `{"replicas":3}` {
		t.Errorf("values = %v, want them carried through unchanged", got.Spec.Values)
	}
}
