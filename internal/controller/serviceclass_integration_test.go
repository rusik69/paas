//go:build integration

package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/chart"
	"github.com/rusik69/paas/internal/controller/engine"
	platformctl "github.com/rusik69/paas/internal/controller/platform"
	"github.com/rusik69/paas/internal/controller/serviceclass"
)

const testRegistry = "oci://registry.paas-system.svc.cluster.local:5000/paas/charts"

// A schema every one of these classes can share: what is under test is the
// path from a ServiceClass to a served kind, not the conversion, which
// internal/schema covers exhaustively.
const validSchema = `{"type":"object","properties":{"size":{"type":"string"}}}`

// ensurePlatformSource writes the PackageSource the platform reconciler owns.
// It is the registry the ServiceClass reconciler must resolve against, and the
// platform tests delete it in their own cleanup, so every test that needs it
// puts it back.
func ensurePlatformSource(t *testing.T) {
	t.Helper()

	src := &v1alpha1.PackageSource{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "PackageSource",
		},
		ObjectMeta: metav1.ObjectMeta{Name: platformctl.SourceName},
		Spec:       v1alpha1.PackageSourceSpec{URL: testRegistry, Insecure: true},
	}
	if err := k8sClient.Patch(t.Context(), src, client.Apply,
		client.FieldOwner("test"), client.ForceOwnership); err != nil {
		t.Fatalf("apply packagesource: %v", err)
	}
}

// fakeSchema stands in for the OCI fetcher and records the registry it was
// asked to pull from.
type fakeSchema struct {
	raw      []byte
	err      error
	registry string
}

func (f *fakeSchema) Schema(_ context.Context, registry, _, _ string) ([]byte, error) {
	f.registry = registry
	if f.err != nil {
		return nil, f.err
	}
	return f.raw, nil
}

// classEngine is an Engine that counts the controllers it was asked to build
// without building one, and the count is how a restart is observed: Running is
// true either side of a stop-and-start. The service controller's own lifecycle
// is asserted in service_integration_test.go; what matters here is what the
// ServiceClass reconciler asks of the engine.
func classEngine(t *testing.T) (*engine.Engine, func(schema.GroupVersionKind) int) {
	t.Helper()

	mgr, err := manager.New(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	var mu sync.Mutex
	builds := map[schema.GroupVersionKind]int{}
	eng := &engine.Engine{
		Manager: mgr,
		Build: func(_ context.Context, gvk schema.GroupVersionKind, _ func()) error {
			mu.Lock()
			defer mu.Unlock()
			builds[gvk]++
			return nil
		},
	}
	return eng, func(gvk schema.GroupVersionKind) int {
		mu.Lock()
		defer mu.Unlock()
		return builds[gvk]
	}
}

func serviceClassReconciler(f chart.Fetcher, eng *engine.Engine) *serviceclass.Reconciler {
	return &serviceclass.Reconciler{Client: k8sClient, Fetcher: f, Engine: eng}
}

// newServiceClass creates a class and removes it, and the CRD it generates,
// afterwards. Every test uses its own kind: the generated CRDs are
// cluster-scoped and outlive the test that made them.
func newServiceClass(t *testing.T, name, kind, plural string) *v1alpha1.ServiceClass {
	t.Helper()

	sc := &v1alpha1.ServiceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ServiceClassSpec{
			Kind:   kind,
			Plural: plural,
			Chart:  v1alpha1.ChartRef{Name: name, Version: "0.1.0"},
		},
	}
	if err := k8sClient.Create(t.Context(), sc); err != nil {
		t.Fatalf("create serviceclass %s: %v", name, err)
	}
	t.Cleanup(func() { cleanupServiceClass(t, name, plural) })
	return sc
}

// cleanupServiceClass drops the finalizer before deleting, so a test that never
// reconciled a deletion cannot wedge the suite on an object nothing will
// release.
func cleanupServiceClass(t *testing.T, name, plural string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var sc v1alpha1.ServiceClass
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &sc); err == nil {
		if len(sc.Finalizers) > 0 {
			sc.Finalizers = nil
			if err := k8sClient.Update(ctx, &sc); err != nil {
				t.Logf("cleanup: release finalizer on %s: %v", name, err)
			}
		}
		if err := k8sClient.Delete(ctx, &sc); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete serviceclass %s: %v", name, err)
		}
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + serviceclass.Group},
	}
	if err := k8sClient.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("cleanup: delete crd %s: %v", crd.Name, err)
	}
}

func gvkOf(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: serviceclass.Group, Version: serviceclass.Version, Kind: kind}
}

func getServiceClass(t *testing.T, name string) *v1alpha1.ServiceClass {
	t.Helper()

	var sc v1alpha1.ServiceClass
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: name}, &sc); err != nil {
		t.Fatalf("get serviceclass %s: %v", name, err)
	}
	return &sc
}

// readyCondition fails the test if the class carries none, since every
// assertion below is about which one it carries.
func readyCondition(t *testing.T, name string) *metav1.Condition {
	t.Helper()

	cond := apimeta.FindStatusCondition(getServiceClass(t, name).Status.Conditions, serviceclass.ConditionReady)
	if cond == nil {
		t.Fatalf("serviceclass %s has no Ready condition", name)
	}
	return cond
}

func getCRD(t *testing.T, plural string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	var crd apiextensionsv1.CustomResourceDefinition
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: plural + "." + serviceclass.Group}, &crd); err != nil {
		t.Fatalf("get crd for %s: %v", plural, err)
	}
	return &crd
}

func TestServiceClass_EstablishesTheCRDAndStartsAController(t *testing.T) {
	ensurePlatformSource(t)

	f := &fakeSchema{raw: []byte(validSchema)}
	eng, _ := classEngine(t)
	sc := newServiceClass(t, "widget", "Widget", "widgets")

	reconcile(t, sc.Name, serviceClassReconciler(f, eng))

	// The registry comes from the platform's own PackageSource rather than a
	// constant of the serviceclass package, so the schema pull and the tenant
	// HelmRepository cannot name different registries.
	if f.registry != testRegistry {
		t.Errorf("fetched from %q, want the platform PackageSource's %q", f.registry, testRegistry)
	}

	crd := getCRD(t, "widgets")
	if !apimeta.IsStatusConditionTrue(crdConditions(crd), string(apiextensionsv1.Established)) {
		t.Errorf("crd conditions = %+v, want Established=True", crd.Status.Conditions)
	}
	if got := crd.Labels[serviceclass.ManagedByLabel]; got != "widget" {
		t.Errorf("%s = %q, want the class name — an orphaned CRD is unfindable without it",
			serviceclass.ManagedByLabel, got)
	}

	if !eng.Running(gvkOf("Widget")) {
		t.Error("no controller is running for the kind the CRD now serves")
	}

	cond := readyCondition(t, sc.Name)
	if cond.Status != metav1.ConditionTrue || cond.Reason != serviceclass.ReasonEstablished {
		t.Errorf("Ready = %s/%s, want True/%s", cond.Status, cond.Reason, serviceclass.ReasonEstablished)
	}
	if got := getServiceClass(t, sc.Name).Status.ObservedChartVersion; got != "0.1.0" {
		t.Errorf("observedChartVersion = %q, want 0.1.0 — which schema is live is unanswerable without it", got)
	}
}

// The security boundary: a schema the generator cannot represent faithfully
// produces no CRD at all, and says which path it choked on. Dropping the field
// and generating the rest would put an unvalidated value straight into Helm.
func TestServiceClass_UnrepresentableSchemaLeavesNoCRD(t *testing.T) {
	ensurePlatformSource(t)

	f := &fakeSchema{raw: []byte(`{"type":"object","properties":{"a":{"$ref":"#/definitions/x"}}}`)}
	eng, _ := classEngine(t)
	sc := newServiceClass(t, "gadget", "Gadget", "gadgets")

	// No error, deliberately: the chart version is pinned, so requeueing would
	// re-read the same published bytes forever.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}
	if _, err := serviceClassReconciler(f, eng).Reconcile(t.Context(), req); err != nil {
		t.Fatalf("reconcile returned %v, want nil — an unfixable schema must not requeue", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "gadgets." + serviceclass.Group}, &crd)
	if !apierrors.IsNotFound(err) {
		t.Errorf("get crd = %v, want NotFound — a CRD was generated from a schema that cannot be represented", err)
	}
	if eng.Running(gvkOf("Gadget")) {
		t.Error("a controller is running for a kind no CRD serves")
	}

	cond := readyCondition(t, sc.Name)
	if cond.Status != metav1.ConditionFalse || cond.Reason != serviceclass.ReasonSchemaNotStructural {
		t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, serviceclass.ReasonSchemaNotStructural)
	}
	if !strings.Contains(cond.Message, ".properties.a") {
		t.Errorf("message = %q, want the offending JSON path — a message saying only \"error\" tells an operator nothing", cond.Message)
	}
}

// A registry blip must never take a serving kind away from the tenants using it.
func TestServiceClass_FetchFailureLeavesAWorkingCRDStanding(t *testing.T) {
	ensurePlatformSource(t)

	f := &fakeSchema{raw: []byte(validSchema)}
	eng, _ := classEngine(t)
	sc := newServiceClass(t, "sprocket", "Sprocket", "sprockets")
	r := serviceClassReconciler(f, eng)

	reconcile(t, sc.Name, r)
	getCRD(t, "sprockets")

	f.err = errors.New("registry unreachable")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sc.Name}}
	if _, err := r.Reconcile(t.Context(), req); err == nil {
		t.Fatal("a fetch failure was not reported, so nothing would retry it")
	}

	crd := getCRD(t, "sprockets")
	if len(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties) == 0 {
		t.Error("the serving schema was emptied by a failed fetch")
	}

	cond := readyCondition(t, sc.Name)
	if cond.Status != metav1.ConditionFalse || cond.Reason != serviceclass.ReasonChartUnavailable {
		t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, serviceclass.ReasonChartUnavailable)
	}
	if !strings.Contains(cond.Message, "registry unreachable") {
		t.Errorf("message = %q, want the fetch failure it reports", cond.Message)
	}
}

// Platform prunes a ServiceClass dropped from a release. If that deleted the
// CRD it would delete every tenant's objects of that kind and, through the
// ownership chain, their data.
func TestServiceClass_DeletedServiceClassLeavesTheCRD(t *testing.T) {
	ensurePlatformSource(t)

	f := &fakeSchema{raw: []byte(validSchema)}
	eng, _ := classEngine(t)
	sc := newServiceClass(t, "doodad", "Doodad", "doodads")
	r := serviceClassReconciler(f, eng)

	reconcile(t, sc.Name, r)
	if !eng.Running(gvkOf("Doodad")) {
		t.Fatal("no controller is running before the deletion under test")
	}

	if err := k8sClient.Delete(t.Context(), getServiceClass(t, sc.Name)); err != nil {
		t.Fatalf("delete serviceclass: %v", err)
	}
	reconcile(t, sc.Name, r)

	if eng.Running(gvkOf("Doodad")) {
		t.Error("the controller is still running for a class that is gone")
	}

	crd := getCRD(t, "doodads")
	if got := crd.Labels[serviceclass.ManagedByLabel]; got != "doodad" {
		t.Errorf("%s = %q, want doodad — the orphaned CRD is unfindable without it",
			serviceclass.ManagedByLabel, got)
	}
	if crd.Annotations[serviceclass.OrphanedAnnotation] == "" {
		t.Errorf("%s is unset, so nothing distinguishes an orphaned CRD from a served one",
			serviceclass.OrphanedAnnotation)
	}

	err := k8sClient.Get(t.Context(), types.NamespacedName{Name: sc.Name}, &v1alpha1.ServiceClass{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("get serviceclass = %v, want NotFound — the finalizer was never released", err)
	}

	// A later release puts the class back. The mark has to come off with it, or
	// the query it exists for answers with kinds that are being served.
	reconcile(t, newServiceClass(t, "doodad", "Doodad", "doodads").Name, r)
	if got := getCRD(t, "doodads").Annotations[serviceclass.OrphanedAnnotation]; got != "" {
		t.Errorf("%s = %q on a kind that is served again", serviceclass.OrphanedAnnotation, got)
	}
}

// A chart version bump has to reach tenants. The running controller holds the
// class it was built from, so re-applying the CRD alone would serve the new
// schema while every tenant's release went on installing the old chart — with
// status reporting the new version.
func TestServiceClass_ChartVersionBumpRestartsTheController(t *testing.T) {
	ensurePlatformSource(t)

	f := &fakeSchema{raw: []byte(validSchema)}
	eng, builds := classEngine(t)
	sc := newServiceClass(t, "flange", "Flange", "flanges")
	r := serviceClassReconciler(f, eng)

	reconcile(t, sc.Name, r)
	if got := builds(gvkOf("Flange")); got != 1 {
		t.Fatalf("controllers built = %d, want 1", got)
	}

	// An unchanged class must not churn its controller.
	reconcile(t, sc.Name, r)
	if got := builds(gvkOf("Flange")); got != 1 {
		t.Errorf("controllers built = %d after reconciling an unchanged class, want 1", got)
	}

	live := getServiceClass(t, sc.Name)
	live.Spec.Chart.Version = "0.2.0"
	if err := k8sClient.Update(t.Context(), live); err != nil {
		t.Fatalf("bump chart version: %v", err)
	}
	reconcile(t, sc.Name, r)

	if got := builds(gvkOf("Flange")); got != 2 {
		t.Errorf("controllers built = %d after a chart version bump, want 2 — tenants keep getting 0.1.0", got)
	}
	if got := getServiceClass(t, sc.Name).Status.ObservedChartVersion; got != "0.2.0" {
		t.Errorf("observedChartVersion = %q, want 0.2.0", got)
	}
}

// The engine's Builder, which is what actually turns a started kind into a
// running service controller. It reads the class and the registry back out of
// the cluster rather than capturing them, so a chart version bump restarts the
// controller against the class as it is now.
func TestServiceClass_BuilderRunsTheKindsController(t *testing.T) {
	ensurePlatformSource(t)

	mgr, err := manager.New(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	mgrCtx, cancelMgr := context.WithCancel(t.Context())
	t.Cleanup(cancelMgr)
	go func() { _ = mgr.Start(mgrCtx) }()
	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatal("cache did not sync")
	}

	newServiceClass(t, "cog", "Cog", "cogs")
	build := serviceclass.BuilderFor(mgr)

	if err := build(t.Context(), gvkOf("Cog"), func() {}); err != nil {
		t.Errorf("build controller for Cog: %v", err)
	}
	// Specifically that no class generates it, not merely that something failed:
	// the same closure also fails on a missing PackageSource and on registration,
	// and a test accepting any error would keep passing if classFor stopped
	// discriminating kinds at all.
	err = build(t.Context(), gvkOf("Nonesuch"), func() {})
	if err == nil || !strings.Contains(err.Error(), "no serviceclass generates") ||
		!strings.Contains(err.Error(), "Nonesuch") {
		t.Errorf("err = %v, want it to say no serviceclass generates Nonesuch", err)
	}
}

// crdConditions adapts a CRD's own condition type to the metav1 helpers, which
// is cheaper than hand-rolling the search twice.
func crdConditions(crd *apiextensionsv1.CustomResourceDefinition) []metav1.Condition {
	out := make([]metav1.Condition, 0, len(crd.Status.Conditions))
	for _, c := range crd.Status.Conditions {
		out = append(out, metav1.Condition{
			Type:               string(c.Type),
			Status:             metav1.ConditionStatus(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime,
		})
	}
	return out
}
