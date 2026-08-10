//go:build integration

package controller

import (
	"context"
	"testing"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/controller/packagesource"
	"github.com/rusik69/paas/internal/flux"
)

func packageSourceReconciler() *packagesource.Reconciler {
	return &packagesource.Reconciler{Client: k8sClient, Scheme: scheme}
}

type reconciler interface {
	Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
}

func reconcile(t *testing.T, name string, r reconciler) {
	t.Helper()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

func TestPackageSource_RendersAHelmRepository(t *testing.T) {
	src := &v1alpha1.PackageSource{
		ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec: v1alpha1.PackageSourceSpec{
			URL:      "oci://registry.paas.io/paas",
			Insecure: true,
		},
	}
	mustCreate(t, src)
	reconcile(t, src.Name, packageSourceReconciler())

	got := &sourcev1.HelmRepository{}
	key := types.NamespacedName{Namespace: flux.Namespace, Name: src.Name}
	if err := k8sClient.Get(t.Context(), key, got); err != nil {
		t.Fatalf("get helmrepository: %v", err)
	}

	if got.Spec.Type != sourcev1.HelmRepositoryTypeOCI {
		t.Errorf("type = %q, want %q", got.Spec.Type, sourcev1.HelmRepositoryTypeOCI)
	}
	if got.Spec.URL != src.Spec.URL {
		t.Errorf("url = %q, want %q", got.Spec.URL, src.Spec.URL)
	}
	if !got.Spec.Insecure {
		t.Error("insecure = false, want true — the in-cluster registry speaks plain HTTP")
	}
	if got.Spec.Interval.Duration != 5*time.Minute {
		t.Errorf("interval = %v, want the CRD default 5m", got.Spec.Interval.Duration)
	}

	// Deleting the PackageSource must reclaim the HelmRepository. A namespaced
	// dependent may name a cluster-scoped owner, which is what makes this legal.
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != src.Name {
		t.Errorf("ownerReferences = %+v, want one naming %s", got.OwnerReferences, src.Name)
	}
}

// The level-triggered claim: the derived object follows the source on a later
// reconcile, not only on the first.
func TestPackageSource_FollowsAnIntervalChange(t *testing.T) {
	src := &v1alpha1.PackageSource{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-interval"},
		Spec: v1alpha1.PackageSourceSpec{
			URL:      "oci://registry.paas.io/paas",
			Interval: &metav1.Duration{Duration: time.Minute},
		},
	}
	mustCreate(t, src)
	reconcile(t, src.Name, packageSourceReconciler())

	src.Spec.Interval = &metav1.Duration{Duration: 30 * time.Minute}
	if err := k8sClient.Update(t.Context(), src); err != nil {
		t.Fatalf("update packagesource: %v", err)
	}
	reconcile(t, src.Name, packageSourceReconciler())

	got := &sourcev1.HelmRepository{}
	key := types.NamespacedName{Namespace: flux.Namespace, Name: src.Name}
	if err := k8sClient.Get(t.Context(), key, got); err != nil {
		t.Fatalf("get helmrepository: %v", err)
	}
	if got.Spec.Interval.Duration != 30*time.Minute {
		t.Errorf("interval = %v, want 30m — the derived object did not follow", got.Spec.Interval.Duration)
	}
}

// A PackageSource that is gone must not be an error: the reconciler races
// deletion constantly and requeuing on NotFound is a hot loop.
func TestPackageSource_MissingObjectIsNotAnError(t *testing.T) {
	reconcile(t, "never-existed", packageSourceReconciler())
}
