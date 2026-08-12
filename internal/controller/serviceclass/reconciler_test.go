package serviceclass

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/chart"
	"github.com/rusik69/paas/internal/controller/engine"
	"github.com/rusik69/paas/internal/controller/platform"
)

const validSchema = `{"type":"object","properties":{"instances":{"type":"integer"}}}`

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return s
}

func testSource() *v1alpha1.PackageSource {
	return &v1alpha1.PackageSource{
		ObjectMeta: metav1.ObjectMeta{Name: platform.SourceName},
		Spec:       v1alpha1.PackageSourceSpec{URL: "oci://registry.test/charts", Insecure: true},
	}
}

// stubFetcher returns one schema, or one error, for every chart.
type stubFetcher struct {
	raw []byte
	err error
}

func (f stubFetcher) Schema(context.Context, chart.Source, string, string) ([]byte, error) {
	return f.raw, f.err
}

// recordingFetcher keeps the Source it was called with, so a test can assert
// the schema pull and the HelmRepository take their transport from the same
// place rather than from two settings that happen to agree.
type recordingFetcher struct{ src chart.Source }

func (f *recordingFetcher) Schema(_ context.Context, src chart.Source, _, _ string) ([]byte, error) {
	f.src = src
	return []byte(validSchema), nil
}

func builder(t *testing.T, objects ...client.Object) *fake.ClientBuilder {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.ServiceClass{}, &apiextensionsv1.CustomResourceDefinition{}).
		WithObjects(objects...)
}

// testReconciler wires a class, the platform's PackageSource and a fetcher into
// a reconciler over a fake client. Server-side apply against a fake is not
// trusted for behaviour — go-guidelines says testing it there tests the fake —
// so these assert the branches around it, and the apply itself is asserted in
// the envtest suite.
func testReconciler(t *testing.T, c client.Client, f chart.Fetcher) *Reconciler {
	t.Helper()

	return &Reconciler{
		Client:  c,
		Fetcher: f,
		Engine:  &engine.Engine{Build: func(context.Context, schema.GroupVersionKind, func()) error { return nil }},
	}
}

func request() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: "postgres"}}
}

func readyOf(t *testing.T, c client.Client, name string) *metav1.Condition {
	t.Helper()

	var sc v1alpha1.ServiceClass
	if err := c.Get(t.Context(), types.NamespacedName{Name: name}, &sc); err != nil {
		t.Fatalf("get serviceclass %s: %v", name, err)
	}
	cond := apimeta.FindStatusCondition(sc.Status.Conditions, ConditionReady)
	if cond == nil {
		t.Fatalf("serviceclass %s has no Ready condition", name)
	}
	return cond
}

// A class that is gone is not an error: reconcile is level-triggered and races
// deletion constantly.
func TestReconcile_MissingClassIsNotAnError(t *testing.T) {
	t.Parallel()

	c := builder(t).Build()

	if _, err := testReconciler(t, c, stubFetcher{}).Reconcile(t.Context(), request()); err != nil {
		t.Errorf("Reconcile: %v", err)
	}
}

func TestReconcile_ReadFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := builder(t).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
}

// No PackageSource means the platform has not rolled out yet. It is the same
// case as an unreachable registry: report it, retry, touch nothing.
func TestReconcile_MissingPackageSourceIsChartUnavailable(t *testing.T) {
	t.Parallel()

	c := builder(t, testClass()).Build()

	_, err := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)}).Reconcile(t.Context(), request())
	if err == nil {
		t.Fatal("a missing PackageSource was not reported, so nothing would retry it")
	}
	if cond := readyOf(t, c, "postgres"); cond.Reason != ReasonChartUnavailable {
		t.Errorf("reason = %q, want %q", cond.Reason, ReasonChartUnavailable)
	}
}

func TestReconcile_FetchFailureIsChartUnavailable(t *testing.T) {
	t.Parallel()

	boom := errors.New("registry unreachable")
	c := builder(t, testClass(), testSource()).Build()

	_, err := testReconciler(t, c, stubFetcher{err: boom}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the fetch failure", err)
	}

	cond := readyOf(t, c, "postgres")
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonChartUnavailable {
		t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, ReasonChartUnavailable)
	}
	if !strings.Contains(cond.Message, "registry unreachable") {
		t.Errorf("message = %q, want the cause it reports", cond.Message)
	}
}

// A published chart cannot be fixed by retrying, so neither shape of bad schema
// returns an error — but they get different reasons, because "not valid JSON"
// and "not expressible as a structural schema" are different bugs to go and fix.
func TestReconcile_BadSchemaIsTerminal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		raw    string
		reason string
		want   string
	}{
		{"unrepresentable", `{"type":"object","properties":{"a":{"$ref":"#/x"}}}`, ReasonSchemaNotStructural, ".properties.a"},
		{"malformed", `not json`, ReasonSchemaInvalid, "parse values.schema.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := builder(t, testClass(), testSource()).Build()

			if _, err := testReconciler(t, c, stubFetcher{raw: []byte(tc.raw)}).Reconcile(t.Context(), request()); err != nil {
				t.Fatalf("Reconcile returned %v, want nil — retrying a published schema cannot fix it", err)
			}

			cond := readyOf(t, c, "postgres")
			if cond.Status != metav1.ConditionFalse || cond.Reason != tc.reason {
				t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, tc.reason)
			}
			if !strings.Contains(cond.Message, tc.want) {
				t.Errorf("message = %q, want it to name %q", cond.Message, tc.want)
			}
		})
	}
}

func TestReconcile_CRDApplyFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("admission webhook refused the CRD")
	c := builder(t, testClass(), testSource()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
				return boom
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the apply failure", err)
	}
	if cond := readyOf(t, c, "postgres"); cond.Reason != ReasonApplyFailed {
		t.Errorf("reason = %q, want %q", cond.Reason, ReasonApplyFailed)
	}
}

// A CRD the API server has not accepted yet is early, not broken: no error, no
// requeue — the watch on generated CRDs brings the class back — and above all
// no controller for a kind that is not being served.
func TestReconcile_UnestablishedCRDStartsNoController(t *testing.T) {
	t.Parallel()

	// Not seeded at all, which is what a just-applied CRD looks like through a
	// cache that has not caught up yet.
	c := builder(t, testClass(), testSource()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: applyIsANoOp,
	}).Build()
	r := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)})

	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("Reconcile returned %v, want nil — waiting is not a fault", err)
	}
	if r.Engine.Running(GVKFor(testClass())) {
		t.Error("a controller was started for a kind the API server has not established")
	}

	cond := readyOf(t, c, "postgres")
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNotEstablished {
		t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, ReasonNotEstablished)
	}
	if !strings.Contains(cond.Message, "postgreses."+Group) {
		t.Errorf("message = %q, want it to name the CRD being waited on", cond.Message)
	}
}

// Names already taken by another CRD are never granted, so this one is
// terminal: reported, and not retried until something changes.
func TestReconcile_RejectedNamesAreTerminal(t *testing.T) {
	t.Parallel()

	rejected := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "postgreses." + Group},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
				Type:    apiextensionsv1.NamesAccepted,
				Status:  apiextensionsv1.ConditionFalse,
				Reason:  "ListKindConflict",
				Message: "another CRD already serves postgreses",
			}},
		},
	}
	c := builder(t, testClass(), testSource(), rejected).WithInterceptorFuncs(interceptor.Funcs{
		Patch: applyIsANoOp,
	}).Build()
	r := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)})

	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("Reconcile returned %v, want nil — retrying cannot free the name", err)
	}
	if r.Engine.Running(GVKFor(testClass())) {
		t.Error("a controller was started for a kind whose names were refused")
	}

	cond := readyOf(t, c, "postgres")
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNamesRejected {
		t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, ReasonNamesRejected)
	}
	if !strings.Contains(cond.Message, "another CRD already serves postgreses") {
		t.Errorf("message = %q, want the API server's own conflict message", cond.Message)
	}
}

func TestReconcile_CRDReadFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := builder(t, testClass(), testSource()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: applyIsANoOp,
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the read failure", err)
	}
	if cond := readyOf(t, c, "postgres"); cond.Reason != ReasonNotEstablished {
		t.Errorf("reason = %q, want %q", cond.Reason, ReasonNotEstablished)
	}
}

func TestReconcile_ControllerStartFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("cache never synced")
	c := builder(t, testClass(), testSource(), establishedCRD()).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyIsANoOp}).Build()
	r := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)})
	r.Engine = &engine.Engine{Build: func(context.Context, schema.GroupVersionKind, func()) error { return boom }}

	_, err := r.Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the builder's failure", err)
	}
	if cond := readyOf(t, c, "postgres"); cond.Reason != ReasonControllerFailed {
		t.Errorf("reason = %q, want %q", cond.Reason, ReasonControllerFailed)
	}
}

func TestReconcile_ServesTheKindAndRecordsTheChartVersion(t *testing.T) {
	t.Parallel()

	c := builder(t, testClass(), testSource(), establishedCRD()).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyIsANoOp}).Build()
	r := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)})

	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !r.Engine.Running(GVKFor(testClass())) {
		t.Error("no controller is running for the kind that is now established")
	}

	var sc v1alpha1.ServiceClass
	if err := c.Get(t.Context(), types.NamespacedName{Name: "postgres"}, &sc); err != nil {
		t.Fatalf("get serviceclass: %v", err)
	}
	if sc.Status.ObservedChartVersion != "0.1.0" {
		t.Errorf("observedChartVersion = %q, want 0.1.0", sc.Status.ObservedChartVersion)
	}
	if !apimeta.IsStatusConditionTrue(sc.Status.Conditions, ConditionReady) {
		t.Errorf("conditions = %+v, want Ready=True", sc.Status.Conditions)
	}
}

// The status write failing must not swallow what it was trying to report.
func TestReconcile_StatusFailureDoesNotMaskTheCause(t *testing.T) {
	t.Parallel()

	boom := errors.New("registry unreachable")
	statusBoom := errors.New("conflict on status")
	c := builder(t, testClass(), testSource()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return statusBoom
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{err: boom}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) || !errors.Is(err, statusBoom) {
		t.Errorf("err = %v, want both the cause and the failed recording", err)
	}
}

func TestReconcile_FinalizerPatchFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("conflict")
	c := builder(t, testClass(), testSource()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the finalizer patch failure", err)
	}
}

// A class deleted before it ever generated a CRD has nothing to mark orphaned,
// and must not be held open over it.
func TestReconcile_DeletionWithoutACRDReleasesTheClass(t *testing.T) {
	t.Parallel()

	c := builder(t, deletingClass()).Build()

	if _, err := testReconciler(t, c, stubFetcher{}).Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sc v1alpha1.ServiceClass
	switch err := c.Get(t.Context(), types.NamespacedName{Name: "postgres"}, &sc); {
	case apierrors.IsNotFound(err):
		// The fake client reclaims an object once its last finalizer goes.
	case err != nil:
		t.Fatalf("get serviceclass: %v", err)
	case len(sc.Finalizers) > 0:
		t.Errorf("finalizers = %v, want released", sc.Finalizers)
	}
}

func TestReconcile_OrphanFailureHoldsTheClassOpen(t *testing.T) {
	t.Parallel()

	boom := errors.New("conflict on the crd")
	c := builder(t, deletingClass(), establishedCRD()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
				return boom
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the failure to mark the CRD orphaned", err)
	}

	var sc v1alpha1.ServiceClass
	if err := c.Get(t.Context(), types.NamespacedName{Name: "postgres"}, &sc); err != nil {
		t.Fatalf("get serviceclass: %v", err)
	}
	if len(sc.Finalizers) == 0 {
		t.Error("the finalizer was released although the CRD was never marked")
	}
}

func TestReconcile_ReleasingTheFinalizerReportsItsFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("conflict on the class")
	c := builder(t, deletingClass(), establishedCRD()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*v1alpha1.ServiceClass); ok {
				return boom
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the failure to release the finalizer", err)
	}
}

// Reconcile is called more than once for the same deletion, so releasing a
// finalizer that is already gone must be a no-op rather than a patch.
func TestRemoveFinalizer_IsIdempotent(t *testing.T) {
	t.Parallel()

	sc := deletingClass()
	sc.Finalizers = nil
	c := builder(t).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			t.Error("a patch was issued for a finalizer that was not there")
			return nil
		},
	}).Build()

	if err := testReconciler(t, c, stubFetcher{}).removeFinalizer(t.Context(), sc); err != nil {
		t.Errorf("removeFinalizer: %v", err)
	}
}

// Serving a kind again has to take the orphan mark off the CRD, and a failure
// to do so must not be reported as success.
func TestReconcile_ClearingTheOrphanMarkIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("conflict on the crd")
	marked := establishedCRD()
	marked.Annotations = map[string]string{OrphanedAnnotation: "2026-08-11T00:00:00Z"}
	c := builder(t, testClass(), testSource(), marked).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patch == client.Apply {
				return nil
			}
			if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
				return boom
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	_, err := testReconciler(t, c, stubFetcher{raw: []byte(validSchema)}).Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the failure to clear the mark", err)
	}
	if cond := readyOf(t, c, "postgres"); cond.Reason != ReasonMarkFailed {
		t.Errorf("reason = %q, want %q — ApplyFailed would read as the CRD apply", cond.Reason, ReasonMarkFailed)
	}
}

// The watch's mapping, whose failure mode is silent: a CRD that maps to no
// request, or to the wrong one, just means nothing ever reconciles.
func TestByManagedClass(t *testing.T) {
	t.Parallel()

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "postgreses." + Group,
			Labels: map[string]string{ManagedByLabel: "postgres"},
		},
	}
	got := byManagedClass(t.Context(), crd)
	if len(got) != 1 || got[0].Name != "postgres" {
		t.Errorf("requests = %v, want one naming the class", got)
	}

	// Every other CRD in the cluster comes through this watch too.
	crd.Labels = nil
	if got := byManagedClass(t.Context(), crd); got != nil {
		t.Errorf("requests = %v for a CRD this operator did not generate, want none", got)
	}
}

func TestSourceFrom_ReadsTheRegistryAndItsTransport(t *testing.T) {
	t.Parallel()

	c := builder(t, testSource()).Build()

	src, err := SourceFrom(t.Context(), c)
	if err != nil {
		t.Fatalf("SourceFrom: %v", err)
	}
	if src.Registry != "oci://registry.test/charts" || !src.Insecure {
		t.Errorf("source = %+v, want the PackageSource's own url and transport", src)
	}
}

// The drift this closes: the schema pull once took its transport from an
// operator flag while the HelmRepository took it from the PackageSource, so
// the same registry could be reached two ways.
func TestReconcile_PullsTheSchemaWithThePackageSourcesOwnTransport(t *testing.T) {
	t.Parallel()

	c := builder(t, testClass(), testSource(), establishedCRD()).
		WithInterceptorFuncs(interceptor.Funcs{Patch: applyIsANoOp}).Build()
	f := &recordingFetcher{}

	if _, err := testReconciler(t, c, f).Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	want := chart.Source{Registry: "oci://registry.test/charts", Insecure: true}
	if f.src != want {
		t.Errorf("schema pulled from %+v, want the PackageSource's own %+v", f.src, want)
	}
}

// A kind whose controller stopped stays dead unless something reconciles its
// class again: the engine frees the slot, and this is what turns that into a
// restart.
func TestWakeOnStop_SendsTheOwningClass(t *testing.T) {
	t.Parallel()

	c := builder(t, testClass()).Build()
	wake := make(chan event.GenericEvent, 1)

	WakeOnStop(c, wake)(t.Context(), GVKFor(testClass()))

	select {
	case ev := <-wake:
		if ev.Object == nil || ev.Object.GetName() != "postgres" {
			t.Errorf("event names %v, want the ServiceClass the stopped kind came from", ev.Object)
		}
	default:
		t.Error("nothing was sent; the freed slot would stay unserved until the informer's ten-hour resync")
	}
}

// A class deleted is the ordinary reason a kind stops. There is nothing to
// wake, and blocking or panicking over it would be a worse answer than
// silence.
func TestWakeOnStop_UnknownKindSendsNothing(t *testing.T) {
	t.Parallel()

	c := builder(t).Build()
	wake := make(chan event.GenericEvent, 1)

	WakeOnStop(c, wake)(t.Context(), GVKFor(testClass()))

	select {
	case ev := <-wake:
		t.Errorf("sent %v for a kind no class generates", ev.Object)
	default:
	}
}

// The send is bounded: an already-cancelled context must return rather than
// hold the goroutine that reported the stop.
func TestWakeOnStop_GivesUpWhenNothingReceives(t *testing.T) {
	t.Parallel()

	c := builder(t, testClass()).Build()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		WakeOnStop(c, make(chan event.GenericEvent))(ctx, GVKFor(testClass()))
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("WakeOnStop blocked on an unreceived send with its context already cancelled")
	}
}

func TestClassFor_ReportsAKindNoClassGenerates(t *testing.T) {
	t.Parallel()

	c := builder(t, testClass()).Build()

	if _, err := classFor(t.Context(), c, GVKFor(testClass())); err != nil {
		t.Errorf("classFor: %v", err)
	}

	_, err := classFor(t.Context(), c, schema.GroupVersionKind{Group: Group, Version: Version, Kind: "Nonesuch"})
	if err == nil || !strings.Contains(err.Error(), "Nonesuch") {
		t.Errorf("err = %v, want it to name the kind nothing generates", err)
	}
}

func TestClassFor_ReportsAListFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := builder(t).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		},
	}).Build()

	if _, err := classFor(t.Context(), c, GVKFor(testClass())); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the list failure", err)
	}
}

// applyIsANoOp lets the reconcile past its server-side apply without asking the
// fake client to imitate one, which go-guidelines forbids relying on.
func applyIsANoOp(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if patch == client.Apply {
		return nil
	}
	return c.Patch(ctx, obj, patch, opts...)
}

func establishedCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "postgreses." + Group},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
				Type:   apiextensionsv1.Established,
				Status: apiextensionsv1.ConditionTrue,
				Reason: "InitialNamesAccepted",
			}},
		},
	}
}

func deletingClass() *v1alpha1.ServiceClass {
	sc := testClass()
	now := metav1.Now()
	sc.DeletionTimestamp = &now
	sc.Finalizers = []string{Finalizer}
	return sc
}
