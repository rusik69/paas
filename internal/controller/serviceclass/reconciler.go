package serviceclass

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/chart"
	"github.com/rusik69/paas/internal/controller/engine"
	"github.com/rusik69/paas/internal/controller/platform"
	"github.com/rusik69/paas/internal/controller/service"
	"github.com/rusik69/paas/internal/crd"
	paasschema "github.com/rusik69/paas/internal/schema"
)

// FieldManager owns the generated CustomResourceDefinitions. API: changing it
// orphans field ownership of every CRD already generated.
const FieldManager = "paas-operator/serviceclass"

// Finalizer holds a ServiceClass open until its controller has been stopped.
//
// A finalizer rather than a watch on deletion, because the engine's Stop must
// run while the class is still readable: the GVK to stop is derived from its
// spec, and by the time the object is gone there is nothing left to derive it
// from.
const Finalizer = "platform.paas.io/serviceclass-controller"

// OrphanedAnnotation records when a generated CRD's ServiceClass went away.
//
// The CRD deliberately outlives its class, so without this an orphan is
// indistinguishable from a served kind. Together with ManagedByLabel it makes
// "which CRDs have nothing reconciling them" a label selector rather than an
// archaeology exercise.
const OrphanedAnnotation = "platform.paas.io/orphaned-since"

// ConditionReady reports whether the class's kind is being served.
const ConditionReady = "Ready"

// Reasons for the Ready condition.
const (
	ReasonEstablished         = "Established"
	ReasonChartUnavailable    = "ChartUnavailable"
	ReasonSchemaNotStructural = "SchemaNotStructural"
	ReasonSchemaInvalid       = "SchemaInvalid"
	ReasonApplyFailed         = "ApplyFailed"
	ReasonNotEstablished      = "NotEstablished"
	ReasonControllerFailed    = "ControllerFailed"
)

// EstablishTimeout bounds the wait for the API server to accept a generated
// CRD. Establishment is normally immediate; the bound is what keeps a wedged
// API server from holding a reconcile open for the life of the process.
const EstablishTimeout = 30 * time.Second

// Source is the registry catalog charts are pulled from, and whether it speaks
// plain HTTP.
type Source struct {
	Registry string
	Insecure bool
}

// SourceFrom reads the platform's own PackageSource.
//
// The schema pull here and the HelmRepository the service reconciler renders
// into a tenant namespace have to name the same registry over the same
// transport. Reading the one object the platform reconciler already writes
// makes that agreement checkable; a second constant would let the two drift,
// and the failure — every tenant chart pull refused against a plain-HTTP
// registry — is invisible to any test that does not pull a real chart.
func SourceFrom(ctx context.Context, c client.Reader) (Source, error) {
	var src v1alpha1.PackageSource
	if err := c.Get(ctx, client.ObjectKey{Name: platform.SourceName}, &src); err != nil {
		return Source{}, fmt.Errorf("read packagesource %s: %w", platform.SourceName, err)
	}
	return Source{Registry: src.Spec.URL, Insecure: src.Spec.Insecure}, nil
}

// BuilderFor returns the engine.Builder that runs the controller for a
// generated kind.
//
// It lives here rather than in the operator so the engine never learns what it
// is starting: engine importing this package would be a cycle, since the
// ServiceClass reconciler drives the engine.
//
// It must not call back into the Engine. The engine invokes it holding that
// kind's entry lock, which is not reentrant.
func BuilderFor(mgr ctrl.Manager) engine.Builder {
	return func(ctx context.Context, gvk schema.GroupVersionKind, done func()) error {
		// Read fresh rather than captured when the class reconciled: a chart
		// version bump stops and restarts the controller, and it has to come up
		// against the class as it is now.
		sc, err := classFor(ctx, mgr.GetClient(), gvk)
		if err != nil {
			return err
		}
		src, err := SourceFrom(ctx, mgr.GetClient())
		if err != nil {
			return err
		}
		return (&service.Reconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			GVK:      gvk,
			Class:    sc,
			Registry: src.Registry,
			Insecure: src.Insecure,
		}).SetupWithManager(ctx, mgr, done)
	}
}

// classFor finds the ServiceClass a GVK was generated from. Listing rather than
// getting by name, because the class name and the kind are independent fields.
func classFor(ctx context.Context, c client.Reader, gvk schema.GroupVersionKind) (*v1alpha1.ServiceClass, error) {
	var classes v1alpha1.ServiceClassList
	if err := c.List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("list serviceclasses: %w", err)
	}
	for i := range classes.Items {
		if GVKFor(&classes.Items[i]) == gvk {
			return &classes.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no serviceclass generates %s", gvk)
}

// Reconciler turns a ServiceClass into a served kind.
type Reconciler struct {
	client.Client
	// Fetcher reads a chart's values.schema.json.
	Fetcher chart.Fetcher
	// Engine owns the controllers for the kinds this reconciler establishes.
	Engine *engine.Engine
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ServiceClass{}).
		Watches(&apiextensionsv1.CustomResourceDefinition{}, handler.EnqueueRequestsFromMapFunc(byManagedClass)).
		Complete(r)
}

// byManagedClass maps a generated CRD back to the class it came from.
//
// Owns() cannot do this: the CRD carries no owner reference, deliberately, so
// that pruning a class cannot garbage-collect every tenant's objects of its
// kind. Without the watch, a CRD deleted or edited out from under us is not
// noticed until the cache's next full resync, while the class goes on
// reporting the kind as served.
func byManagedClass(_ context.Context, obj client.Object) []ctrl.Request {
	name := obj.GetLabels()[ManagedByLabel]
	if name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: name}}}
}

// Reconcile converges a ServiceClass on a CRD the API server has accepted and a
// controller running for it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sc v1alpha1.ServiceClass
	if err := r.Get(ctx, req.NamespacedName, &sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	gvk := GVKFor(&sc)

	if !sc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &sc, gvk)
	}
	if err := r.addFinalizer(ctx, &sc); err != nil {
		return ctrl.Result{}, err
	}

	src, err := SourceFrom(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &sc, ReasonChartUnavailable, err)
	}
	raw, err := r.Fetcher.Schema(ctx, src.Registry, sc.Spec.Chart.Name, sc.Spec.Chart.Version)
	if err != nil {
		// Nothing below runs on this path, deliberately: a registry blip must
		// never take a serving kind away from the tenants using it.
		return ctrl.Result{}, r.fail(ctx, &sc, ReasonChartUnavailable,
			fmt.Errorf("fetch schema for chart %s:%s: %w", sc.Spec.Chart.Name, sc.Spec.Chart.Version, err))
	}

	desired, err := CRDFor(&sc, raw)
	if err != nil {
		// No error returned, because there is nothing a retry could change: the
		// chart version is pinned, so the next pass reads the same published
		// bytes. The message carries the offending JSON path, which is the only
		// thing that tells whoever publishes the chart what to fix.
		reason := ReasonSchemaInvalid
		var unrepresentable *paasschema.UnrepresentableError
		if errors.As(err, &unrepresentable) {
			reason = ReasonSchemaNotStructural
		}
		return ctrl.Result{}, r.record(ctx, &sc, metav1.ConditionFalse, reason, err.Error())
	}

	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, r.fail(ctx, &sc, ReasonApplyFailed,
			fmt.Errorf("apply crd %s: %w", desired.Name, err))
	}

	// No controller for a kind the API server has not accepted: it would watch
	// a resource that does not resolve and never sync its cache.
	establish, cancel := context.WithTimeout(ctx, EstablishTimeout)
	defer cancel()
	if err := crd.WaitEstablished(establish, r.Client, desired.Name); err != nil {
		return ctrl.Result{}, r.fail(ctx, &sc, ReasonNotEstablished, err)
	}

	if err := r.serve(ctx, &sc, gvk); err != nil {
		return ctrl.Result{}, r.fail(ctx, &sc, ReasonControllerFailed, err)
	}
	// The kind is served again, whatever it was before.
	if err := r.markOrphaned(ctx, &sc, ""); err != nil {
		return ctrl.Result{}, r.fail(ctx, &sc, ReasonApplyFailed, err)
	}

	sc.Status.ObservedChartVersion = sc.Spec.Chart.Version
	return ctrl.Result{}, r.record(ctx, &sc, metav1.ConditionTrue, ReasonEstablished,
		fmt.Sprintf("serving %s from chart %s:%s", gvk.Kind, sc.Spec.Chart.Name, sc.Spec.Chart.Version))
}

// serve makes the engine run a controller for gvk against the class as it is
// now.
//
// Start on its own is idempotent, and the controller it already started holds
// the class it was built from — so a class whose chart version has moved would
// serve the new schema while every tenant's release went on installing the old
// version, with status claiming otherwise. Replacing the controller is what
// carries the bump through to tenants.
//
// Both are attempted on that path: if the stop half fails there is no informer
// state left worth preserving, and the joined error still fails the reconcile.
func (r *Reconciler) serve(ctx context.Context, sc *v1alpha1.ServiceClass, gvk schema.GroupVersionKind) error {
	if sc.Status.ObservedChartVersion == sc.Spec.Chart.Version {
		return r.Engine.Start(ctx, gvk)
	}
	return errors.Join(r.Engine.Stop(ctx, gvk), r.Engine.Start(ctx, gvk))
}

// finalize stops the kind's controller and leaves its CRD standing.
//
// Deleting the CRD would delete every tenant's object of that kind and, through
// the ownership chain, their data — and Platform prunes a ServiceClass dropped
// from a release, so that would happen during an ordinary upgrade. An orphaned
// CRD nothing reconciles is a bad day someone notices; silent data destruction
// is a catastrophe nobody notices until it is irreversible.
//
// Both steps are attempted even when the first fails, and the finalizer is
// released only once neither has anything left to report.
func (r *Reconciler) finalize(ctx context.Context, sc *v1alpha1.ServiceClass, gvk schema.GroupVersionKind) error {
	orphaned := sc.DeletionTimestamp.UTC().Format(time.RFC3339)
	if err := errors.Join(r.Engine.Stop(ctx, gvk), r.markOrphaned(ctx, sc, orphaned)); err != nil {
		return err
	}
	return r.removeFinalizer(ctx, sc)
}

// markOrphaned records on the generated CRD whether anything is still
// reconciling its kind. An empty since clears the mark.
//
// Both directions, because Platform prunes a class out of one release and a
// later one puts it back: a CRD left permanently marked would make the query
// the annotation exists for — which kinds have nothing behind them — answer
// with kinds that are being served. Writing nothing when the value already
// matches keeps the ordinary reconcile down to a cached read.
//
// The class's deletion timestamp rather than the current time, so a retried
// finalize writes the same value instead of churning the object.
func (r *Reconciler) markOrphaned(ctx context.Context, sc *v1alpha1.ServiceClass, since string) error {
	name := sc.Spec.Plural + "." + Group
	var existing apiextensionsv1.CustomResourceDefinition
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
		// A class deleted before it ever generated a CRD has nothing to mark,
		// and must not be held open over it.
		return client.IgnoreNotFound(err)
	}
	if existing.Annotations[OrphanedAnnotation] == since {
		return nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	if since == "" {
		delete(existing.Annotations, OrphanedAnnotation)
	} else {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[OrphanedAnnotation] = since
	}
	if err := r.Patch(ctx, &existing, patch, client.FieldOwner(FieldManager)); err != nil {
		return fmt.Errorf("mark crd %s orphaned=%q: %w", name, since, err)
	}
	return nil
}

func (r *Reconciler) addFinalizer(ctx context.Context, sc *v1alpha1.ServiceClass) error {
	if controllerutil.ContainsFinalizer(sc, Finalizer) {
		return nil
	}
	patch := client.MergeFrom(sc.DeepCopy())
	controllerutil.AddFinalizer(sc, Finalizer)
	if err := r.Patch(ctx, sc, patch); err != nil {
		return fmt.Errorf("add finalizer to serviceclass %s: %w", sc.Name, err)
	}
	return nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, sc *v1alpha1.ServiceClass) error {
	if !controllerutil.ContainsFinalizer(sc, Finalizer) {
		return nil
	}
	patch := client.MergeFrom(sc.DeepCopy())
	controllerutil.RemoveFinalizer(sc, Finalizer)
	if err := r.Patch(ctx, sc, patch); err != nil {
		return fmt.Errorf("remove finalizer from serviceclass %s: %w", sc.Name, err)
	}
	return nil
}

// record writes the Ready condition and returns only the status write's own
// error, so a caller can decide whether the cause is worth requeueing over.
func (r *Reconciler) record(ctx context.Context, sc *v1alpha1.ServiceClass, status metav1.ConditionStatus, reason, message string) error {
	sc.Status.ObservedGeneration = sc.Generation
	apimeta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sc.Generation,
	})
	if err := r.Status().Update(ctx, sc); err != nil {
		return fmt.Errorf("update status of serviceclass %s: %w", sc.Name, err)
	}
	return nil
}

// fail records why the kind is not being served and returns the cause, so the
// controller requeues with its own backoff. The status write's own failure must
// not mask the cause.
func (r *Reconciler) fail(ctx context.Context, sc *v1alpha1.ServiceClass, reason string, cause error) error {
	if err := r.record(ctx, sc, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		return fmt.Errorf("%w (and recording it failed: %w)", cause, err)
	}
	return cause
}
