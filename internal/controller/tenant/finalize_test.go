package tenant

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
	"github.com/rusik69/paas/pkg/tenancy"
)

// The release half of deletion. envtest runs no namespace controller, so a
// Terminating namespace never disappears there and this is the only place the
// "namespace is gone" path can be reached.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, corev1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return s
}

func deleting(name string) *corev1alpha1.Tenant {
	now := metav1.Now()
	return &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         tenancy.RootNamespace,
			Name:              name,
			Finalizers:        []string{Finalizer},
			DeletionTimestamp: &now,
		},
		Spec: corev1alpha1.TenantSpec{Plan: corev1alpha1.PlanTrial},
	}
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: tenancy.RootNamespace, Name: name}}
}

func TestReconcile_ReleasesOnceTheNamespaceIsGone(t *testing.T) {
	t.Parallel()

	obj := deleting("leaving")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(&corev1alpha1.Tenant{}).
		Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("leaving")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The fake client deletes an object outright once its last finalizer goes.
	var got corev1alpha1.Tenant
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), &got); err == nil && len(got.Finalizers) > 0 {
		t.Errorf("finalizers = %v, want released", got.Finalizers)
	}
}

// A Tenant naming a namespace it does not own must not be able to delete it, or
// naming "kube-system" would be a way to take the cluster down.
func TestReconcile_WillNotDeleteANamespaceItDoesNotOwn(t *testing.T) {
	t.Parallel()

	obj := deleting("leaving")
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "tenant-leaving",
		Labels: map[string]string{TenantLabel: "somebody-else"},
	}}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj, foreign).WithStatusSubresource(&corev1alpha1.Tenant{}).
		Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("leaving")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var stillThere corev1.Namespace
	if err := c.Get(t.Context(), types.NamespacedName{Name: "tenant-leaving"}, &stillThere); err != nil {
		t.Fatalf("another tenant's namespace was deleted: %v", err)
	}
	if !stillThere.DeletionTimestamp.IsZero() {
		t.Error("another tenant's namespace was asked to delete")
	}
}

// A path that never derived created nothing, so there is nothing to reclaim and
// no reason to hold the object open — otherwise a mistyped tenant is
// undeletable.
func TestReconcile_ReleasesATenantWhosePathNeverDerived(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	obj := &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "stray",
			Finalizers:        []string{Finalizer},
			DeletionTimestamp: &now,
		},
		Spec: corev1alpha1.TenantSpec{Plan: corev1alpha1.PlanTrial},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(&corev1alpha1.Tenant{}).
		Build()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "stray"}}
	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got corev1alpha1.Tenant
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), &got); err == nil && len(got.Finalizers) > 0 {
		t.Errorf("finalizers = %v, want released", got.Finalizers)
	}
}
