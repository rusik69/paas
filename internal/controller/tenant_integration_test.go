//go:build integration

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
	tenantctl "github.com/rusik69/paas/internal/controller/tenant"
	"github.com/rusik69/paas/pkg/tenancy"
)

func tenantReconciler() *tenantctl.Reconciler {
	return &tenantctl.Reconciler{Client: k8sClient, Scheme: scheme}
}

// reconcileIn drives the reconciler for a namespaced object.
func reconcileIn(t *testing.T, ns, name string, r reconciler) {
	t.Helper()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
}

func ensureNamespace(t *testing.T, name string) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// newTenant creates a Tenant and removes it, plus its namespace, afterwards.
func newTenant(t *testing.T, ns, name string, plan corev1alpha1.Plan) *corev1alpha1.Tenant {
	t.Helper()

	ensureNamespace(t, ns)
	obj := &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1alpha1.TenantSpec{Plan: plan},
	}
	if err := k8sClient.Create(t.Context(), obj); err != nil {
		t.Fatalf("create tenant %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() { cleanupTenant(t, ns, name) })
	return obj
}

// cleanupTenant drops the finalizer before deleting, so a test that never
// reconciled a deletion cannot wedge the suite on a namespace nothing will
// reclaim.
func cleanupTenant(t *testing.T, ns, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var obj corev1alpha1.Tenant
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &obj); err != nil {
		return
	}
	if len(obj.Finalizers) > 0 {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.Finalizers = nil
		if err := k8sClient.Patch(ctx, &obj, patch); err != nil {
			t.Logf("cleanup: drop finalizers on %s/%s: %v", ns, name, err)
		}
	}
	if err := k8sClient.Delete(ctx, &obj); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("cleanup: delete tenant %s/%s: %v", ns, name, err)
	}
}

func tenantCondition(t *testing.T, ns, name, condType string) *metav1.Condition {
	t.Helper()

	var got corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	return apimeta.FindStatusCondition(got.Status.Conditions, condType)
}

func TestTenant_RootProducesItsNamespaceAndQuota(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "acme", corev1alpha1.PlanBusiness)
	reconcileIn(t, tenancy.RootNamespace, "acme", tenantReconciler())

	var ns corev1.Namespace
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "tenant-acme"}, &ns); err != nil {
		t.Fatalf("get namespace tenant-acme: %v", err)
	}
	if ns.Labels[tenantctl.TenantLabel] != "acme" {
		t.Errorf("namespace label = %q, want acme", ns.Labels[tenantctl.TenantLabel])
	}

	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-acme", Name: "tenant"}
	if err := k8sClient.Get(t.Context(), key, &quota); err != nil {
		t.Fatalf("get resourcequota: %v", err)
	}
	if got := quota.Spec.Hard[corev1.ResourceRequestsCPU]; got.String() != "8" {
		t.Errorf("quota requests.cpu = %s, want 8 for the business plan", got.String())
	}
	if got := quota.Spec.Hard[corev1.ResourceLimitsCPU]; got.String() != "8" {
		t.Errorf("quota limits.cpu = %s, want 8 — bounding requests alone lets a tenant burst past its plan", got.String())
	}

	var limits corev1.LimitRange
	if err := k8sClient.Get(t.Context(), key, &limits); err != nil {
		t.Fatalf("get limitrange: %v", err)
	}
	if len(limits.Spec.Limits) == 0 || limits.Spec.Limits[0].DefaultRequest.Cpu().IsZero() {
		t.Error("limitrange sets no default cpu request; pods with no request would be rejected by the quota")
	}

	if c := tenantCondition(t, tenancy.RootNamespace, "acme", tenantctl.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True", c)
	}
}

// The tree is expressed by containment, so a child's namespace name comes from
// where it lives rather than from a field it could contradict.
func TestTenant_ChildNamespaceIsPathDerived(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "acme", corev1alpha1.PlanBusiness)
	reconcileIn(t, tenancy.RootNamespace, "acme", tenantReconciler())

	newTenant(t, "tenant-acme", "beta", corev1alpha1.PlanTrial)
	reconcileIn(t, "tenant-acme", "beta", tenantReconciler())

	var ns corev1.Namespace
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "tenant-acme-beta"}, &ns); err != nil {
		t.Fatalf("get namespace tenant-acme-beta: %v", err)
	}

	// Isolation does not inherit: the child gets its own quota, at its own plan.
	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-acme-beta", Name: "tenant"}
	if err := k8sClient.Get(t.Context(), key, &quota); err != nil {
		t.Fatalf("get child resourcequota: %v", err)
	}
	if got := quota.Spec.Hard[corev1.ResourceRequestsCPU]; got.String() != "2" {
		t.Errorf("child quota requests.cpu = %s, want 2 for the trial plan", got.String())
	}
}

// A truncated name would let two tenants collide on one namespace, so the
// reconciler must refuse rather than create anything.
func TestTenant_OverlongNameIsRejectedWithoutCreatingANamespace(t *testing.T) {
	long := strings.Repeat("a", 60)
	newTenant(t, tenancy.RootNamespace, long, corev1alpha1.PlanTrial)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: tenancy.RootNamespace, Name: long}}
	if _, err := tenantReconciler().Reconcile(t.Context(), req); err == nil {
		t.Fatal("an overlong tenant name was accepted")
	}

	c := tenantCondition(t, tenancy.RootNamespace, long, tenantctl.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", c)
	}
	if !strings.Contains(c.Message, "63") {
		t.Errorf("Degraded message = %q, want it to name the limit", c.Message)
	}

	var ns corev1.Namespace
	err := k8sClient.Get(t.Context(), types.NamespacedName{Name: tenancy.Prefix + long}, &ns)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a namespace was created for an overlong name (err %v)", err)
	}
}

func TestTenant_PlanChangeUpdatesTheQuota(t *testing.T) {
	obj := newTenant(t, tenancy.RootNamespace, "grow", corev1alpha1.PlanTrial)
	reconcileIn(t, tenancy.RootNamespace, "grow", tenantReconciler())

	var fresh corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(obj), &fresh); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	fresh.Spec.Plan = corev1alpha1.PlanEnterprise
	if err := k8sClient.Update(t.Context(), &fresh); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	reconcileIn(t, tenancy.RootNamespace, "grow", tenantReconciler())

	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-grow", Name: "tenant"}
	if err := k8sClient.Get(t.Context(), key, &quota); err != nil {
		t.Fatalf("get resourcequota: %v", err)
	}
	if got := quota.Spec.Hard[corev1.ResourceRequestsCPU]; got.String() != "32" {
		t.Errorf("quota requests.cpu = %s, want 32 after moving to enterprise", got.String())
	}
}

// ADR 0004 requires deletion to reclaim descendants, and the namespace going is
// what does it. envtest runs no namespace controller, so the namespace stays
// Terminating forever here — which makes this the right place to assert that the
// tenant is *held open*, and the release path a unit test with a fake client.
func TestTenant_DeletionAsksTheNamespaceToGoAndHoldsTheTenant(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "leaving", corev1alpha1.PlanTrial)
	reconcileIn(t, tenancy.RootNamespace, "leaving", tenantReconciler())

	key := types.NamespacedName{Namespace: tenancy.RootNamespace, Name: "leaving"}
	var withFinalizer corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), key, &withFinalizer); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if len(withFinalizer.Finalizers) == 0 {
		t.Fatal("no finalizer was added; deletion would orphan the namespace")
	}

	if err := k8sClient.Delete(t.Context(), &withFinalizer); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	req := ctrl.Request{NamespacedName: key}
	if _, err := tenantReconciler().Reconcile(t.Context(), req); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	var ns corev1.Namespace
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "tenant-leaving"}, &ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if ns.DeletionTimestamp.IsZero() {
		t.Error("namespace was not asked to delete")
	}

	var stillHere corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), key, &stillHere); err != nil {
		t.Errorf("tenant was released before its namespace was gone: %v", err)
	}
}

func TestTenant_MissingObjectIsNotAnError(t *testing.T) {
	reconcileIn(t, tenancy.RootNamespace, "never-existed", tenantReconciler())
}
