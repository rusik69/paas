//go:build integration

package controller

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rusik69/paas/api/v1alpha1"
	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
	platformctl "github.com/rusik69/paas/internal/controller/platform"
)

// fakeFetcher serves releases from a map. The reconciler's behaviour is what
// this suite exists to pin; standing up a registry would test the registry.
type fakeFetcher struct {
	releases map[string]*platformctl.Release
	err      error
}

func (f *fakeFetcher) Fetch(_ context.Context, _, version string) (*platformctl.Release, error) {
	if f.err != nil {
		return nil, f.err
	}
	rel, ok := f.releases[version]
	if !ok {
		return nil, errors.New("no such release: " + version)
	}
	return rel, nil
}

func release(version string, entries ...platformctl.Entry) *platformctl.Release {
	return &platformctl.Release{Version: version, Digest: "sha256:" + version, Packages: entries}
}

func entry(name string, stage v1alpha1.PackageStage, chartVersion string) platformctl.Entry {
	return platformctl.Entry{Name: name, Chart: name, Version: chartVersion, Stage: stage}
}

func platformReconciler(f platformctl.Fetcher) *platformctl.Reconciler {
	return &platformctl.Reconciler{Client: k8sClient, Scheme: scheme, Fetcher: f}
}

func newPlatform(t *testing.T, version string) *v1alpha1.Platform {
	t.Helper()

	p := &v1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       v1alpha1.PlatformSpec{Version: version, Registry: "oci://registry.paas.io/paas"},
	}
	mustCreate(t, p)
	t.Cleanup(func() { cleanupPackages(t, p.Name) })
	return p
}

// Re-read before writing: the reconciler writes status on every pass, so a
// handle held across one is stale and Update fails on resourceVersion.
func setVersion(t *testing.T, name, version string) {
	t.Helper()

	got := &v1alpha1.Platform{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get platform: %v", err)
	}
	got.Spec.Version = version
	if err := k8sClient.Update(t.Context(), got); err != nil {
		t.Fatalf("set version %s: %v", version, err)
	}
}

func ownedPackages(t *testing.T, platform string) []v1alpha1.Package {
	t.Helper()

	var list v1alpha1.PackageList
	if err := k8sClient.List(t.Context(), &list,
		client.MatchingLabels{pkgctl.PlatformLabel: platform}); err != nil {
		t.Fatalf("list packages: %v", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

func packageNames(t *testing.T, platform string) []string {
	t.Helper()

	var got []string
	for _, p := range ownedPackages(t, platform) {
		got = append(got, p.Name)
	}
	return got
}

// The comparable part of a Package: what a rollback has to reproduce.
func packageSpecs(t *testing.T, platform string) map[string]v1alpha1.PackageSpec {
	t.Helper()

	out := map[string]v1alpha1.PackageSpec{}
	for _, p := range ownedPackages(t, platform) {
		out[p.Name] = p.Spec
	}
	return out
}

func cleanupPackages(t *testing.T, platform string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var list v1alpha1.PackageList
	if err := k8sClient.List(ctx, &list, client.MatchingLabels{pkgctl.PlatformLabel: platform}); err != nil {
		t.Logf("cleanup: list packages: %v", err)
		return
	}
	for i := range list.Items {
		if err := k8sClient.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete package %s: %v", list.Items[i].Name, err)
		}
	}
	src := &v1alpha1.PackageSource{ObjectMeta: metav1.ObjectMeta{Name: platformctl.SourceName}}
	if err := k8sClient.Delete(ctx, src); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("cleanup: delete packagesource: %v", err)
	}
}

func TestPlatform_RendersTheReleaseItNames(t *testing.T) {
	f := &fakeFetcher{releases: map[string]*platformctl.Release{
		"v1.0.0": release("v1.0.0",
			entry("cnpg-migrate", v1alpha1.StageMigration, "1.0.0"),
			entry("cnpg", v1alpha1.StageComponent, "1.0.0"),
		),
	}}

	p := newPlatform(t, "v1.0.0")
	reconcile(t, p.Name, platformReconciler(f))

	if diff := cmp.Diff([]string{"cnpg", "cnpg-migrate"}, packageNames(t, p.Name)); diff != "" {
		t.Errorf("packages differ (-want +got):\n%s", diff)
	}

	src := &v1alpha1.PackageSource{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: platformctl.SourceName}, src); err != nil {
		t.Fatalf("get packagesource: %v", err)
	}
	if src.Spec.URL != p.Spec.Registry {
		t.Errorf("source url = %q, want %q", src.Spec.URL, p.Spec.Registry)
	}

	got := &v1alpha1.Platform{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: p.Name}, got); err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if got.Status.Current == nil {
		t.Fatal("status.current is unset after a successful rollout")
	}
	if got.Status.Current.Version != "v1.0.0" || got.Status.Current.Digest != "sha256:v1.0.0" {
		t.Errorf("status.current = %+v, want v1.0.0 with its resolved digest", got.Status.Current)
	}
}

// The case an apply-only reconciler gets wrong: without pruning, "roll out a
// complete platform version" quietly means "a superset of every version so far".
func TestPlatform_UpgradeAddsUpdatesAndRemoves(t *testing.T) {
	f := &fakeFetcher{releases: map[string]*platformctl.Release{
		"v1.0.0": release("v1.0.0",
			entry("stays", v1alpha1.StageComponent, "1.0.0"),
			entry("goes", v1alpha1.StageComponent, "1.0.0"),
		),
		"v2.0.0": release("v2.0.0",
			entry("stays", v1alpha1.StageComponent, "2.0.0"),
			entry("arrives", v1alpha1.StageComponent, "2.0.0"),
		),
	}}

	p := newPlatform(t, "v1.0.0")
	reconcile(t, p.Name, platformReconciler(f))

	setVersion(t, p.Name, "v2.0.0")
	reconcile(t, p.Name, platformReconciler(f))

	if diff := cmp.Diff([]string{"arrives", "stays"}, packageNames(t, p.Name)); diff != "" {
		t.Errorf("packages after upgrade differ (-want +got):\n%s", diff)
	}

	stays := &v1alpha1.Package{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "stays"}, stays); err != nil {
		t.Fatalf("get package stays: %v", err)
	}
	if stays.Spec.Version != "2.0.0" {
		t.Errorf("stays version = %q, want 2.0.0 — an existing package was not updated", stays.Spec.Version)
	}

	err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "goes"}, &v1alpha1.Package{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("package 'goes' still exists (err=%v); it was dropped from the release", err)
	}
}

// The phase-1 done-when. Compares the whole spec set, not a spot-checked field:
// a rollback that leaves one package behind is the failure worth catching.
func TestPlatform_RollbackReproducesTheEarlierState(t *testing.T) {
	f := &fakeFetcher{releases: map[string]*platformctl.Release{
		"v1.0.0": release("v1.0.0",
			entry("a", v1alpha1.StageMigration, "1.0.0"),
			entry("b", v1alpha1.StageComponent, "1.0.0"),
		),
		"v2.0.0": release("v2.0.0",
			entry("b", v1alpha1.StageComponent, "2.0.0"),
			entry("c", v1alpha1.StageComponent, "2.0.0"),
		),
	}}

	p := newPlatform(t, "v1.0.0")
	reconcile(t, p.Name, platformReconciler(f))
	before := packageSpecs(t, p.Name)

	setVersion(t, p.Name, "v2.0.0")
	reconcile(t, p.Name, platformReconciler(f))

	setVersion(t, p.Name, "v1.0.0")
	reconcile(t, p.Name, platformReconciler(f))

	if diff := cmp.Diff(before, packageSpecs(t, p.Name)); diff != "" {
		t.Errorf("rollback did not reproduce the earlier state (-before +after):\n%s", diff)
	}
}

func TestPlatform_FetchFailureIsDegradedAndCarriesTheReason(t *testing.T) {
	f := &fakeFetcher{err: errors.New("registry unreachable")}

	p := newPlatform(t, "v9.9.9")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name}}
	if _, err := platformReconciler(f).Reconcile(t.Context(), req); err == nil {
		t.Fatal("a fetch failure was reported as a successful rollout")
	}

	got := &v1alpha1.Platform{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: p.Name}, got); err != nil {
		t.Fatalf("get platform: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, platformctl.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", cond)
	}
	if cond.Message != "registry unreachable" {
		t.Errorf("Degraded message = %q, want the fetcher's own reason", cond.Message)
	}
}

func TestPlatform_MissingObjectIsNotAnError(t *testing.T) {
	reconcile(t, "never-existed-platform", platformReconciler(&fakeFetcher{}))
}
