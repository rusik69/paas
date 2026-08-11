package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

var (
	postgresGVK = schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "Postgres"}
	clusterGVK  = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
)

func testReconciler() *Reconciler {
	return &Reconciler{
		GVK:      postgresGVK,
		Registry: "oci://registry.paas-system.svc.cluster.local:5000/paas/charts",
		Class: &v1alpha1.ServiceClass{
			ObjectMeta: metav1.ObjectMeta{Name: "postgres"},
			Spec: v1alpha1.ServiceClassSpec{
				Kind:   "Postgres",
				Plural: "postgreses",
				Chart:  v1alpha1.ChartRef{Name: "postgres", Version: "0.1.0"},
			},
		},
	}
}

func testCR() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"instances": int64(2),
			"size":      "1Gi",
		},
	}}
	u.SetGroupVersionKind(postgresGVK)
	u.SetName("db")
	u.SetNamespace("tenant-acme")
	u.SetUID("abc-123")
	return u
}

// testScheme registers everything a Reconciler touches: the generated kind
// (as unstructured, since it has no Go type), the Flux kinds it derives, and
// the underlying kind a StatusSource reads from.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{helmv2.AddToScheme, sourcev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	s.AddKnownTypeWithName(postgresGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(postgresGVK.GroupVersion().WithKind("PostgresList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(clusterGVK.GroupVersion().WithKind("ClusterList"), &unstructured.UnstructuredList{})
	return s
}

// postgresMarker tells the fake client the generated kind has a status
// subresource, which unstructured.Unstructured cannot express through the
// scheme alone. Without it, Status().Patch cannot find the object at all.
func postgresMarker() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(postgresGVK)
	return u
}

func statusSource() v1alpha1.StatusSource {
	return v1alpha1.StatusSource{
		Path:     ".status.primary",
		From:     v1alpha1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
		JSONPath: "$.status.currentPrimary",
	}
}

func request(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

func TestDesired_SameNamespaceAndOwned(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}

	if hr.Namespace != "tenant-acme" {
		t.Errorf("Namespace = %q, want tenant-acme — helm-controller watches all namespaces so the release belongs beside the CR", hr.Namespace)
	}
	refs := hr.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("got %d owner references, want 1 — reclaim depends entirely on this", len(refs))
	}
	if refs[0].Kind != "Postgres" || refs[0].Name != "db" || refs[0].UID != "abc-123" {
		t.Errorf("owner reference = %+v, want the Postgres that asked for it", refs[0])
	}
	if refs[0].Controller == nil || !*refs[0].Controller {
		t.Error("owner reference is not a controller reference")
	}
}

func TestDesired_SpecBecomesValuesVerbatim(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if hr.Spec.Values == nil {
		t.Fatal("HelmRelease carries no values")
	}
	got := string(hr.Spec.Values.Raw)
	for _, want := range []string{`"instances":2`, `"size":"1Gi"`} {
		if !strings.Contains(got, want) {
			t.Errorf("values = %s, want it to contain %s", got, want)
		}
	}
}

func TestDesired_EmptySpecIsNotAnError(t *testing.T) {
	cr := testCR()
	unstructured.RemoveNestedField(cr.Object, "spec")

	hr, err := testReconciler().desired(cr)
	if err != nil {
		t.Fatalf("desired on a CR with no spec: %v — a chart whose values are all defaulted is legitimate", err)
	}
	if hr == nil {
		t.Fatal("desired returned no HelmRelease")
	}
}

func TestDesired_CarriesTheServiceLabels(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if hr.Labels[LabelServiceName] != "db" || hr.Labels[LabelServiceNamespace] != "tenant-acme" {
		t.Errorf("labels = %v, want the service name and namespace that status watches map back by", hr.Labels)
	}
}

func TestDesired_ReferencesTheSharedRepository(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if hr.Spec.Chart == nil {
		t.Fatal("HelmRelease carries no chart")
	}
	spec := hr.Spec.Chart.Spec
	if spec.Chart != "postgres" || spec.Version != "0.1.0" {
		t.Errorf("chart = %q@%q, want postgres@0.1.0", spec.Chart, spec.Version)
	}
	if spec.SourceRef.Kind != sourcev1.HelmRepositoryKind || spec.SourceRef.Name != SourceName {
		t.Errorf("sourceRef = %+v, want the shared repository %s", spec.SourceRef, SourceName)
	}
}

func TestDesired_RejectsASpecThatIsNotAMap(t *testing.T) {
	cr := testCR()
	cr.Object["spec"] = "oops"

	_, err := testReconciler().desired(cr)
	if err == nil {
		t.Fatal("a non-map spec was accepted")
	}
	if !strings.Contains(err.Error(), "read spec") {
		t.Errorf("err = %q, want it to name the read", err)
	}
}

func TestDesiredSource_PointsAtTheConfiguredRegistry(t *testing.T) {
	repo := testReconciler().desiredSource()
	if repo.Name != SourceName {
		t.Errorf("name = %q, want %q", repo.Name, SourceName)
	}
	if repo.Spec.URL != "oci://registry.paas-system.svc.cluster.local:5000/paas/charts" {
		t.Errorf("url = %q, want the reconciler's registry", repo.Spec.URL)
	}
	if repo.Spec.Type != sourcev1.HelmRepositoryTypeOCI {
		t.Errorf("type = %q, want oci", repo.Spec.Type)
	}
}

func TestByServiceLabels_MapsBackToTheCR(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetLabels(map[string]string{LabelServiceName: "db", LabelServiceNamespace: "tenant-acme"})

	got := byServiceLabels(t.Context(), obj)
	if len(got) != 1 || got[0].Name != "db" || got[0].Namespace != "tenant-acme" {
		t.Errorf("requests = %+v, want one naming tenant-acme/db", got)
	}
}

func TestByServiceLabels_IgnoresUnlabelledObjects(t *testing.T) {
	obj := &unstructured.Unstructured{}
	if got := byServiceLabels(t.Context(), obj); got != nil {
		t.Errorf("requests = %+v, want none for an object without the service labels", got)
	}
}

func TestReconcile_MissingObjectIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).Build()
	r := testReconciler()
	r.Client = c

	if _, err := r.Reconcile(t.Context(), request("tenant-acme", "never-existed")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestReconcile_ReturnsAReadFailure(t *testing.T) {
	boom := errors.New("etcd is unavailable")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()
	r := testReconciler()
	r.Client = c

	_, err := r.Reconcile(t.Context(), request("tenant-acme", "db"))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
}

func TestReconcile_NamesTheRepositoryItFailedToApply(t *testing.T) {
	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(testCR()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			},
		}).Build()
	r := testReconciler()
	r.Client = c

	_, err := r.Reconcile(t.Context(), request("tenant-acme", "db"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	if !strings.Contains(err.Error(), SourceName) {
		t.Errorf("err = %q, want it to name the repository", err)
	}
}

func TestReconcile_NamesTheReleaseItFailedToApply(t *testing.T) {
	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(testCR()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if _, ok := obj.(*helmv2.HelmRelease); ok {
					return boom
				}
				return nil
			},
		}).Build()
	r := testReconciler()
	r.Client = c

	_, err := r.Reconcile(t.Context(), request("tenant-acme", "db"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("err = %q, want it to name the release", err)
	}
}

func TestReconcile_ReturnsADesiredFailure(t *testing.T) {
	cr := testCR()
	cr.Object["spec"] = "oops"

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cr).Build()
	r := testReconciler()
	r.Client = c

	_, err := r.Reconcile(t.Context(), request("tenant-acme", "db"))
	if err == nil {
		t.Fatal("a CR with an unrenderable spec was accepted")
	}
}

func TestReconcile_RendersTheRepositoryAndReleaseAndSyncsStatus(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(testCR()).Build()
	r := testReconciler()
	r.Client = c

	if _, err := r.Reconcile(t.Context(), request("tenant-acme", "db")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var repo sourcev1.HelmRepository
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "flux-system", Name: SourceName}, &repo); err != nil {
		t.Fatalf("get helmrepository: %v", err)
	}

	var hr helmv2.HelmRelease
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "tenant-acme", Name: "db"}, &hr); err != nil {
		t.Fatalf("get helmrelease: %v", err)
	}

	var cr unstructured.Unstructured
	cr.SetGroupVersionKind(postgresGVK)
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "tenant-acme", Name: "db"}, &cr); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(cr.Object, "status", "conditions")
	if len(conds) != 1 {
		t.Errorf("status.conditions = %v, want one Ready condition synced from the release", conds)
	}
}

func TestFieldPath(t *testing.T) {
	got := fieldPath(".status.primary")
	if len(got) != 2 || got[0] != "status" || got[1] != "primary" {
		t.Errorf("fieldPath = %v, want [status primary]", got)
	}
}

func TestSyncStatus_NoHelmReleaseIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(testCR()).Build()
	r := testReconciler()
	r.Client = c

	if err := r.syncStatus(t.Context(), testCR(), &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "tenant-acme"},
	}); err != nil {
		t.Fatalf("syncStatus: %v — the release may not exist yet, which is early rather than broken", err)
	}
}

func TestSyncStatus_WrapsAReadFailure(t *testing.T) {
	boom := errors.New("etcd is unavailable")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(testCR()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*helmv2.HelmRelease); ok {
					return boom
				}
				return nil
			},
		}).Build()
	r := testReconciler()
	r.Client = c

	err := r.syncStatus(t.Context(), testCR(), &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "tenant-acme"},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the read failure", err)
	}
	if !strings.Contains(err.Error(), "read helmrelease") {
		t.Errorf("err = %q, want it to name the read", err)
	}
}

func helmReleaseWithCondition(name, namespace string, conds ...metav1.Condition) *helmv2.HelmRelease {
	return &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     helmv2.HelmReleaseStatus{Conditions: conds},
	}
}

func TestSyncStatus_DefaultsToPendingWhenNoReadyCondition(t *testing.T) {
	cr := testCR()
	live := helmReleaseWithCondition("db", "tenant-acme")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cr, live).Build()
	r := testReconciler()
	r.Client = c

	if err := r.syncStatus(t.Context(), cr, live); err != nil {
		t.Fatalf("syncStatus: %v", err)
	}

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "tenant-acme", Name: "db"}, &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if len(conds) != 1 {
		t.Fatalf("status.conditions = %v, want one condition", conds)
	}
	cond := conds[0].(map[string]any)
	if cond["reason"] != "Pending" || cond["status"] != string(metav1.ConditionUnknown) {
		t.Errorf("condition = %+v, want reason Pending and status Unknown", cond)
	}
}

func TestSyncStatus_CopiesTheReadyCondition(t *testing.T) {
	cr := testCR()
	live := helmReleaseWithCondition("db", "tenant-acme", metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "InstallSucceeded", Message: "release ready",
	})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cr, live).Build()
	r := testReconciler()
	r.Client = c

	if err := r.syncStatus(t.Context(), cr, live); err != nil {
		t.Fatalf("syncStatus: %v", err)
	}

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "tenant-acme", Name: "db"}, &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	cond := conds[0].(map[string]any)
	if cond["status"] != string(metav1.ConditionTrue) || cond["reason"] != "InstallSucceeded" || cond["message"] != "release ready" {
		t.Errorf("condition = %+v, want the release's own Ready condition mirrored", cond)
	}
}

func TestSyncStatus_PropagatesAStatusFromError(t *testing.T) {
	cr := testCR()
	live := helmReleaseWithCondition("db", "tenant-acme")
	cluster := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace("tenant-acme")
	cluster.SetName("db")
	cluster.SetLabels(map[string]string{LabelServiceName: "db", LabelServiceNamespace: "tenant-acme"})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).
		WithObjects(cr, live, cluster).Build()
	r := testReconciler()
	r.Client = c
	r.Class.Spec.StatusFrom = []v1alpha1.StatusSource{{
		Path:     ".status.primary",
		From:     v1alpha1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
		JSONPath: "[",
	}}

	if err := r.syncStatus(t.Context(), cr, live); err == nil {
		t.Fatal("a malformed jsonPath in StatusFrom was accepted")
	}
}

func TestSyncStatus_SkipsAnAbsentStatusFromValue(t *testing.T) {
	cr := testCR()
	live := helmReleaseWithCondition("db", "tenant-acme")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).
		WithObjects(cr, live).Build()
	r := testReconciler()
	r.Client = c
	r.Class.Spec.StatusFrom = []v1alpha1.StatusSource{statusSource()}

	if err := r.syncStatus(t.Context(), cr, live); err != nil {
		t.Fatalf("syncStatus: %v", err)
	}

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "tenant-acme", Name: "db"}, &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "primary"); found {
		t.Error("status.primary was set, but the release has not created its Cluster yet")
	}
}

func TestSyncStatus_WritesAStatusFromField(t *testing.T) {
	cr := testCR()
	live := helmReleaseWithCondition("db", "tenant-acme")
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"currentPrimary": "db-1"},
	}}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace("tenant-acme")
	cluster.SetName("db")
	cluster.SetLabels(map[string]string{LabelServiceName: "db", LabelServiceNamespace: "tenant-acme"})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cr, live, cluster).Build()
	r := testReconciler()
	r.Client = c
	r.Class.Spec.StatusFrom = []v1alpha1.StatusSource{statusSource()}

	if err := r.syncStatus(t.Context(), cr, live); err != nil {
		t.Fatalf("syncStatus: %v", err)
	}

	var got unstructured.Unstructured
	got.SetGroupVersionKind(postgresGVK)
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "tenant-acme", Name: "db"}, &got); err != nil {
		t.Fatalf("get postgres: %v", err)
	}
	primary, found, _ := unstructured.NestedString(got.Object, "status", "primary")
	if !found || primary != "db-1" {
		t.Errorf("status.primary = %q (found %t), want db-1", primary, found)
	}
}

func TestSyncStatus_ReturnsAPatchFailure(t *testing.T) {
	boom := errors.New("conflict")
	cr := testCR()
	live := helmReleaseWithCondition("db", "tenant-acme")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cr, live).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return boom
			},
		}).Build()
	r := testReconciler()
	r.Client = c

	if err := r.syncStatus(t.Context(), cr, live); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the patch failure", err)
	}
}

func TestReadStatusFrom_AbsentSourceLeavesTheFieldUnset(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).Build()
	r := testReconciler()
	r.Client = c

	got, err := r.readStatusFrom(t.Context(), testCR(), statusSource())
	if err != nil {
		t.Fatalf("readStatusFrom: %v", err)
	}
	if got != "" {
		t.Errorf("readStatusFrom = %q, want empty — the release has not created its Cluster yet", got)
	}
}

func TestReadStatusFrom_ReadsTheLabelledObject(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"currentPrimary": "db-1"},
	}}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace("tenant-acme")
	cluster.SetName("db")
	cluster.SetLabels(map[string]string{
		LabelServiceName:      "db",
		LabelServiceNamespace: "tenant-acme",
	})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(client.Object(cluster)).Build()
	r := testReconciler()
	r.Client = c

	got, err := r.readStatusFrom(t.Context(), testCR(), statusSource())
	if err != nil {
		t.Fatalf("readStatusFrom: %v", err)
	}
	if got != "db-1" {
		t.Errorf("readStatusFrom = %q, want db-1", got)
	}
}

// The fake client resolves an unregistered kind to an empty list rather than
// a meta.NoMatchError — a real API server's RESTMapper is what actually
// returns that error for a kind the cluster does not serve, which is why the
// NoMatchError branch itself is exercised in the envtest suite instead.
func TestReadStatusFrom_UnregisteredKindIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).Build()
	r := testReconciler()
	r.Client = c

	src := statusSource()
	src.From = v1alpha1.ObjectRef{APIVersion: "unregistered.example.com/v1", Kind: "Whatever"}

	got, err := r.readStatusFrom(t.Context(), testCR(), src)
	if err != nil {
		t.Fatalf("readStatusFrom: %v — an unserved kind is early, not broken", err)
	}
	if got != "" {
		t.Errorf("readStatusFrom = %q, want empty", got)
	}
}

func TestReadStatusFrom_ReturnsAListFailure(t *testing.T) {
	boom := errors.New("list refused")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()
	r := testReconciler()
	r.Client = c

	_, err := r.readStatusFrom(t.Context(), testCR(), statusSource())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the list failure", err)
	}
	if !strings.Contains(err.Error(), "Cluster") {
		t.Errorf("err = %q, want it to name the kind", err)
	}
}

func TestReadStatusFrom_RejectsAMalformedJSONPath(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace("tenant-acme")
	cluster.SetName("db")
	cluster.SetLabels(map[string]string{LabelServiceName: "db", LabelServiceNamespace: "tenant-acme"})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cluster).Build()
	r := testReconciler()
	r.Client = c

	src := statusSource()
	src.JSONPath = "["
	_, err := r.readStatusFrom(t.Context(), testCR(), src)
	if err == nil {
		t.Fatal("a malformed jsonPath was accepted")
	}
	if !strings.Contains(err.Error(), "parse jsonPath") {
		t.Errorf("err = %q, want it to name the parse failure", err)
	}
}

func TestReadStatusFrom_ExecuteFailureIsNotAnError(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetNamespace("tenant-acme")
	cluster.SetName("db")
	cluster.SetLabels(map[string]string{LabelServiceName: "db", LabelServiceNamespace: "tenant-acme"})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(postgresMarker()).WithObjects(cluster).Build()
	r := testReconciler()
	r.Client = c

	src := statusSource()
	src.JSONPath = "$.status.doesNotExist"
	got, err := r.readStatusFrom(t.Context(), testCR(), src)
	if err != nil {
		t.Fatalf("readStatusFrom: %v — a field the object does not have yet is early, not broken", err)
	}
	if got != "" {
		t.Errorf("readStatusFrom = %q, want empty", got)
	}
}
