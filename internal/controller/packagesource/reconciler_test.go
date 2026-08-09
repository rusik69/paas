package packagesource

import (
	"context"
	"errors"
	"strings"
	"testing"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/rusik69/paas/api/v1alpha1"
)

// Error propagation only; every claim about apply semantics is made against
// envtest, because testing server-side apply against a fake tests the fake.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, sourcev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return s
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func source(name string) *v1alpha1.PackageSource {
	return &v1alpha1.PackageSource{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.PackageSourceSpec{URL: "oci://registry.paas.io/paas"},
	}
}

// A read failure that is not NotFound must be returned, so the work is
// retried. Swallowing it drops the reconcile silently.
func TestReconcile_ReturnsAReadFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("platform"))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
}

func TestReconcile_NamesTheObjectItFailedToApply(t *testing.T) {
	t.Parallel()

	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(source("platform")).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("platform"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	if !strings.Contains(err.Error(), "platform") {
		t.Errorf("err = %q, want it to name the object", err)
	}
}

// A scheme that does not know PackageSource cannot express the owner reference,
// and rendering must fail rather than emit an unowned object that nothing
// garbage-collects.
func TestReconcile_FailsWhenTheOwnerCannotBeSet(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(source("platform")).Build()

	_, err := (&Reconciler{Client: c, Scheme: runtime.NewScheme()}).Reconcile(t.Context(), request("platform"))
	if err == nil {
		t.Fatal("an unowned HelmRepository was rendered")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("err = %q, want it to name the owner reference", err)
	}
}

// A private registry is the production case: without the pull secret reaching
// the HelmRepository, every chart fetch fails as unauthorized.
func TestDesired_CarriesThePullSecret(t *testing.T) {
	t.Parallel()

	src := source("platform")
	src.Spec.SecretRef = &v1alpha1.LocalRef{Name: "registry-creds"}

	r := &Reconciler{Scheme: testScheme(t)}
	got, err := r.desired(src)
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if got.Spec.SecretRef == nil {
		t.Fatal("secretRef is nil; a private registry could never be read")
	}
	if got.Spec.SecretRef.Name != "registry-creds" {
		t.Errorf("secretRef = %q, want registry-creds", got.Spec.SecretRef.Name)
	}
}
