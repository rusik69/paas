//go:build integration

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/controller/service"
	"github.com/rusik69/paas/internal/controller/serviceclass"
	"github.com/rusik69/paas/pkg/wait"
)

var (
	postgresGVK = schema.GroupVersionKind{Group: serviceclass.Group, Version: serviceclass.Version, Kind: "Postgres"}
	redisGVK    = schema.GroupVersionKind{Group: serviceclass.Group, Version: serviceclass.Version, Kind: "Redis"}
)

// postgresClass mirrors the class the serviceclass package's own tests use, so
// the CRD it generates and the Reconciler tested here agree on shape.
func postgresClass() *v1alpha1.ServiceClass {
	return &v1alpha1.ServiceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres"},
		Spec: v1alpha1.ServiceClassSpec{
			Kind:   "Postgres",
			Plural: "postgreses",
			Chart:  v1alpha1.ChartRef{Name: "postgres", Version: "0.1.0"},
			StatusFrom: []v1alpha1.StatusSource{{
				Path:     ".status.primary",
				From:     v1alpha1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
				JSONPath: "$.status.currentPrimary",
			}},
		},
	}
}

// redisClass is a second catalog entry, and exists only so one test can create
// two generated kinds carrying the same CR name.
func redisClass() *v1alpha1.ServiceClass {
	return &v1alpha1.ServiceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "redis"},
		Spec: v1alpha1.ServiceClassSpec{
			Kind:   "Redis",
			Plural: "redises",
			Chart:  v1alpha1.ChartRef{Name: "redis", Version: "0.1.0"},
		},
	}
}

func installPostgresCRD(t *testing.T) {
	t.Helper()
	installCRD(t, postgresClass())
}

// installCRD installs the CRD a class generates, so tests can create real
// objects of that kind against envtest's API server rather than a fake one.
func installCRD(t *testing.T, class *v1alpha1.ServiceClass) {
	t.Helper()

	crd, err := serviceclass.CRDFor(class, []byte(`{"type":"object","properties":{"instances":{"type":"integer"},"size":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("build %s crd: %v", class.Spec.Kind, err)
	}
	if err := k8sClient.Patch(t.Context(), crd, client.Apply,
		client.FieldOwner("test"), client.ForceOwnership); err != nil {
		t.Fatalf("apply %s crd: %v", class.Spec.Kind, err)
	}

	// Established on the CRD's own status is not the same thing as the client's
	// RESTMapper knowing about it yet: that mapping is cached and only
	// refreshed on a miss, so a Create right after Patch can still race it.
	probe := func(ctx context.Context) (bool, error) {
		var p unstructured.Unstructured
		p.SetGroupVersionKind(serviceclass.GVKFor(class))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "___probe___"}, &p)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	if err := wait.For(t.Context(), 50*time.Millisecond, class.Spec.Kind+" kind being served", probe); err != nil {
		t.Fatalf("wait for %s kind: %v", class.Spec.Kind, err)
	}
}

func serviceReconciler(t *testing.T, class *v1alpha1.ServiceClass) *service.Reconciler {
	t.Helper()
	return &service.Reconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		GVK:      serviceclass.GVKFor(class),
		Class:    class,
		Registry: "oci://registry.paas-system.svc.cluster.local:5000/paas/charts",
		// The in-cluster registry speaks plain HTTP — see
		// platform.Reconciler.applySource for the same fact on the platform's
		// own registry access.
		Insecure: true,
	}
}

func newPostgres(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	return newObject(t, postgresGVK, ns, name)
}

func newObject(t *testing.T, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()

	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"instances": int64(1), "size": "1Gi"},
	}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	if err := k8sClient.Create(t.Context(), u); err != nil {
		t.Fatalf("create %s %s/%s: %v", gvk.Kind, ns, name, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(t.Context(), u)
	})
	return u
}

func TestService_RendersARepositoryAndReleaseAgainstARealAPIServer(t *testing.T) {
	installPostgresCRD(t)
	ensureNamespace(t, "tenant-service")

	cr := newPostgres(t, "tenant-service", "db")
	reconcileIn(t, "tenant-service", "db", serviceReconciler(t, postgresClass()))

	// In the tenant's own namespace, not flux-system: a cross-namespace
	// SourceRef would let source-controller collide two tenants' HelmCharts.
	var repo sourcev1.HelmRepository
	repoKey := types.NamespacedName{Namespace: "tenant-service", Name: service.SourceName}
	if err := k8sClient.Get(t.Context(), repoKey, &repo); err != nil {
		t.Fatalf("get helmrepository: %v", err)
	}
	if repo.Spec.URL != "oci://registry.paas-system.svc.cluster.local:5000/paas/charts" {
		t.Errorf("repository url = %q, want the reconciler's registry", repo.Spec.URL)
	}
	if !repo.Spec.Insecure {
		t.Error("insecure = false, want true — the in-cluster registry speaks plain HTTP")
	}

	var hr helmv2.HelmRelease
	hrKey := types.NamespacedName{Namespace: "tenant-service", Name: "db-postgres"}
	if err := k8sClient.Get(t.Context(), hrKey, &hr); err != nil {
		t.Fatalf("get helmrelease: %v", err)
	}
	if hr.Spec.Chart == nil || hr.Spec.Chart.Spec.Chart != "postgres" || hr.Spec.Chart.Spec.Version != "0.1.0" {
		t.Errorf("chart = %+v, want postgres@0.1.0", hr.Spec.Chart)
	}
	if hr.Spec.Chart.Spec.SourceRef.Name != service.SourceName || hr.Spec.Chart.Spec.SourceRef.Namespace != "tenant-service" {
		t.Errorf("sourceRef = %+v, want the repository in the tenant's own namespace", hr.Spec.Chart.Spec.SourceRef)
	}
	if len(hr.OwnerReferences) != 1 || hr.OwnerReferences[0].Name != "db" {
		t.Errorf("ownerReferences = %+v, want one naming the Postgres", hr.OwnerReferences)
	}
	if hr.Spec.Install == nil || hr.Spec.Install.Remediation == nil || hr.Spec.Install.Remediation.Retries == 0 {
		t.Error("install has no retries; a release that failed once would stay failed")
	}
	if hr.Spec.Upgrade == nil || hr.Spec.Upgrade.Remediation == nil || hr.Spec.Upgrade.Remediation.Retries == 0 {
		t.Error("upgrade has no retries")
	}

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if len(conds) != 1 {
		t.Errorf("status.conditions = %v, want one Ready condition synced from the release", conds)
	}
}

// Two generated kinds, one CR name, one namespace — the shape that used to
// destroy a tenant's data. Both CRs rendered the same HelmRelease: the API
// server refuses a second controller owner reference on it, and helm-controller
// upgrades the release to whichever chart wrote last, deleting the other kind's
// underlying object and its PVCs with it.
//
// Against a real API server rather than in desired() alone, because the owner
// reference is the half a fake client will happily accept.
func TestService_TwoKindsWithOneCRNameGetTheirOwnReleases(t *testing.T) {
	installPostgresCRD(t)
	installCRD(t, redisClass())
	ensureNamespace(t, "tenant-service")

	const name = "cache"
	newObject(t, postgresGVK, "tenant-service", name)
	newObject(t, redisGVK, "tenant-service", name)

	reconcileIn(t, "tenant-service", name, serviceReconciler(t, postgresClass()))
	reconcileIn(t, "tenant-service", name, serviceReconciler(t, redisClass()))

	for _, tc := range []struct{ release, kind string }{
		{name + "-postgres", "Postgres"},
		{name + "-redis", "Redis"},
	} {
		var hr helmv2.HelmRelease
		key := types.NamespacedName{Namespace: "tenant-service", Name: tc.release}
		if err := k8sClient.Get(t.Context(), key, &hr); err != nil {
			t.Fatalf("get helmrelease %s: %v", tc.release, err)
		}
		if len(hr.OwnerReferences) != 1 || hr.OwnerReferences[0].Kind != tc.kind {
			t.Errorf("%s ownerReferences = %+v, want one naming the %s that asked for it",
				tc.release, hr.OwnerReferences, tc.kind)
		}
		if hr.Labels[service.LabelServiceName] != tc.release {
			t.Errorf("%s labelled %s=%q, want the release name — a status watch maps back by it",
				tc.release, service.LabelServiceName, hr.Labels[service.LabelServiceName])
		}
	}
}

// The level-triggered claim: a later spec change is picked up on the next
// reconcile rather than only on the first.
func TestService_FollowsASpecChange(t *testing.T) {
	installPostgresCRD(t)
	ensureNamespace(t, "tenant-service")

	cr := newPostgres(t, "tenant-service", "grow")
	reconciler := serviceReconciler(t, postgresClass())
	reconcileIn(t, "tenant-service", "grow", reconciler)

	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(cr), cr); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	if err := unstructured.SetNestedField(cr.Object, int64(3), "spec", "instances"); err != nil {
		t.Fatalf("set instances: %v", err)
	}
	if err := k8sClient.Update(t.Context(), cr); err != nil {
		t.Fatalf("update postgres: %v", err)
	}
	reconcileIn(t, "tenant-service", "grow", reconciler)

	var hr helmv2.HelmRelease
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Namespace: "tenant-service", Name: "grow-postgres"}, &hr); err != nil {
		t.Fatalf("get helmrelease: %v", err)
	}
	if !strings.Contains(string(hr.Spec.Values.Raw), `"instances":3`) {
		t.Errorf("values = %s, want instances:3 — the derived release did not follow", hr.Spec.Values.Raw)
	}
}

func TestService_SyncsReadyFromARealHelmRelease(t *testing.T) {
	installPostgresCRD(t)
	ensureNamespace(t, "tenant-service")

	cr := newPostgres(t, "tenant-service", "ready")
	reconciler := serviceReconciler(t, postgresClass())
	reconcileIn(t, "tenant-service", "ready", reconciler)

	var hr helmv2.HelmRelease
	key := types.NamespacedName{Namespace: "tenant-service", Name: "ready-postgres"}
	if err := k8sClient.Get(t.Context(), key, &hr); err != nil {
		t.Fatalf("get helmrelease: %v", err)
	}
	hr.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "InstallSucceeded",
		Message: "release ready", LastTransitionTime: metav1.Now(),
	}}
	if err := k8sClient.Status().Update(t.Context(), &hr); err != nil {
		t.Fatalf("update helmrelease status: %v", err)
	}

	reconcileIn(t, "tenant-service", "ready", reconciler)

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if len(conds) != 1 {
		t.Fatalf("status.conditions = %v, want one condition", conds)
	}
	cond := conds[0].(map[string]any)
	if cond["status"] != string(metav1.ConditionTrue) || cond["reason"] != "InstallSucceeded" {
		t.Errorf("condition = %+v, want the release's own Ready condition mirrored", cond)
	}
}

// A real API server's RESTMapper is what actually returns a NoMatchError for
// a kind nothing has installed a CRD for, which is the case this reconciler
// must treat as early rather than broken.
func TestService_StatusFromANeverInstalledKindIsNotAnError(t *testing.T) {
	installPostgresCRD(t)
	ensureNamespace(t, "tenant-service")

	cr := newPostgres(t, "tenant-service", "early")
	reconciler := serviceReconciler(t, postgresClass())
	reconcileIn(t, "tenant-service", "early", reconciler)

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "primary"); found {
		t.Error("status.primary was set, but no Cluster CRD is installed in this suite")
	}
}

func TestService_MissingObjectIsNotAnError(t *testing.T) {
	reconcileIn(t, "tenant-service", "never-existed", serviceReconciler(t, postgresClass()))
}

// Registration is where a missing watch source or a bad GVK surfaces. The
// manager is built but never started, mirroring
// TestSetupWithManager_RegistersBothReconcilers: this asserts the wiring, and
// the behaviour is asserted by the reconcile tests above.
func TestServiceSetupWithManager_RegistersTheReconciler(t *testing.T) {
	mgr, err := manager.New(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	if err := serviceReconciler(t, postgresClass()).SetupWithManager(t.Context(), mgr, func() {}); err != nil {
		t.Errorf("register service reconciler: %v", err)
	}
}

// Without StatusFrom, SetupWithManager registers no extra watch — the loop
// that builds one per source must not be assumed to run.
func TestServiceSetupWithManager_RegistersWithNoStatusFrom(t *testing.T) {
	mgr, err := manager.New(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	class := postgresClass()
	class.Name = "postgres-no-status"
	class.Spec.StatusFrom = nil
	if err := serviceReconciler(t, class).SetupWithManager(t.Context(), mgr, func() {}); err != nil {
		t.Errorf("register service reconciler: %v", err)
	}
}

func waitForHelmRelease(t *testing.T, namespace, name string) {
	t.Helper()

	key := types.NamespacedName{Namespace: namespace, Name: name}
	err := wait.For(t.Context(), 50*time.Millisecond, "helmrelease "+name, func(ctx context.Context) (bool, error) {
		var hr helmv2.HelmRelease
		err := k8sClient.Get(ctx, key, &hr)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		t.Fatalf("wait for helmrelease %s/%s: %v", namespace, name, err)
	}
}

// The regression guard for the engine/controller-runtime lifecycle bug: a
// managed controller (ctrl.NewControllerManagedBy(mgr).Complete) runs on the
// manager's own context and registers its name in a process-global registry
// that is never freed, so calling SetupWithManager again for the same kind
// after the engine's Stop failed forever with "already exists" — and,
// separately, the first controller kept running regardless of the ctx the
// engine thought it owned. This proves both are fixed: the second
// registration succeeds, and it is actually functional, not just error-free.
func TestServiceSetupWithManager_StartAfterStopSucceeds(t *testing.T) {
	installPostgresCRD(t)
	ensureNamespace(t, "tenant-service")

	mgr, err := manager.New(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	mgrCtx, cancelMgr := context.WithCancel(t.Context())
	t.Cleanup(cancelMgr)
	go func() {
		_ = mgr.Start(mgrCtx)
	}()
	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatal("cache did not sync")
	}

	class := postgresClass()
	class.Name = "postgres-restart"
	class.Spec.StatusFrom = nil
	r := serviceReconciler(t, class)

	firstDone := make(chan struct{})
	ctx1, cancel1 := context.WithCancel(t.Context())
	if err := r.SetupWithManager(ctx1, mgr, func() { close(firstDone) }); err != nil {
		t.Fatalf("first SetupWithManager: %v", err)
	}
	newPostgres(t, "tenant-service", "restart-one")
	waitForHelmRelease(t, "tenant-service", "restart-one-postgres")

	cancel1() // simulate the engine's Stop for this kind
	select {
	case <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("done was not called after ctx was cancelled — engine.Start would never be able to run this kind again")
	}

	ctx2, cancel2 := context.WithCancel(t.Context())
	t.Cleanup(cancel2)
	if err := r.SetupWithManager(ctx2, mgr, func() {}); err != nil {
		t.Fatalf("second SetupWithManager after stop: %v", err)
	}
	newPostgres(t, "tenant-service", "restart-two")
	waitForHelmRelease(t, "tenant-service", "restart-two-postgres")
}

