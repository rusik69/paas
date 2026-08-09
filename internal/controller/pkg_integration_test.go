//go:build integration

package controller

import (
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rusik69/paas/api/v1alpha1"
	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
	"github.com/rusik69/paas/internal/flux"
)

func packageReconciler() *pkgctl.Reconciler {
	return &pkgctl.Reconciler{Client: k8sClient, Scheme: scheme}
}

func newPackage(name, platform string, stage v1alpha1.PackageStage) *v1alpha1.Package {
	return &v1alpha1.Package{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{pkgctl.PlatformLabel: platform},
		},
		Spec: v1alpha1.PackageSpec{
			SourceRef: v1alpha1.LocalRef{Name: "platform"},
			Chart:     name,
			Version:   "1.0.0",
			Stage:     stage,
		},
	}
}

func helmRelease(t *testing.T, name string) *helmv2.HelmRelease {
	t.Helper()

	got := &helmv2.HelmRelease{}
	key := types.NamespacedName{Namespace: flux.Namespace, Name: name}
	if err := k8sClient.Get(t.Context(), key, got); err != nil {
		t.Fatalf("get helmrelease %s: %v", name, err)
	}
	return got
}

func TestPackage_MigrationHasNoDependencies(t *testing.T) {
	p := newPackage("cnpg-migrate", "rel-a", v1alpha1.StageMigration)
	mustCreate(t, p)
	reconcile(t, p.Name, packageReconciler())

	got := helmRelease(t, p.Name)
	if len(got.Spec.DependsOn) != 0 {
		t.Errorf("dependsOn = %+v, want none — a migration waits for nothing", got.Spec.DependsOn)
	}
	if got.Spec.Chart.Spec.Chart != p.Spec.Chart {
		t.Errorf("chart = %q, want %q", got.Spec.Chart.Spec.Chart, p.Spec.Chart)
	}
	if got.Spec.Chart.Spec.SourceRef.Kind != sourcev1.HelmRepositoryKind {
		t.Errorf("sourceRef kind = %q, want %q",
			got.Spec.Chart.Spec.SourceRef.Kind, sourcev1.HelmRepositoryKind)
	}
}

// The two-stage guarantee, which is the reason Package carries a stage at all.
func TestPackage_ComponentDependsOnEveryMigrationInItsRelease(t *testing.T) {
	const release = "rel-b"
	for _, n := range []string{"b-migrate-two", "b-migrate-one"} {
		mustCreate(t, newPackage(n, release, v1alpha1.StageMigration))
	}
	// A migration in a different release must not be depended on.
	mustCreate(t, newPackage("other-migrate", "rel-other", v1alpha1.StageMigration))

	comp := newPackage("b-app", release, v1alpha1.StageComponent)
	mustCreate(t, comp)
	reconcile(t, comp.Name, packageReconciler())

	var got []string
	for _, d := range helmRelease(t, comp.Name).Spec.DependsOn {
		got = append(got, d.Name)
	}

	// Sorted, so the rendered object does not churn between reconciles.
	want := []string{"b-migrate-one", "b-migrate-two"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dependsOn differs (-want +got):\n%s", diff)
	}
}

func TestPackage_FollowsAVersionChange(t *testing.T) {
	p := newPackage("c-app", "rel-c", v1alpha1.StageComponent)
	mustCreate(t, p)
	reconcile(t, p.Name, packageReconciler())

	p.Spec.Version = "2.0.0"
	if err := k8sClient.Update(t.Context(), p); err != nil {
		t.Fatalf("update package: %v", err)
	}
	reconcile(t, p.Name, packageReconciler())

	if got := helmRelease(t, p.Name).Spec.Chart.Spec.Version; got != "2.0.0" {
		t.Errorf("chart version = %q, want 2.0.0 — the derived release did not follow", got)
	}
}

func TestPackage_MissingObjectIsNotAnError(t *testing.T) {
	reconcile(t, "never-existed-package", packageReconciler())
}
