// Package service reconciles a tenant's generated-kind object into the
// HelmRelease that installs it.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/jsonpath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

// FieldManager is this reconciler's alone. The platform reconcilers write
// disjoint objects under their own, and one manager per writer keeps ownership
// legible.
const FieldManager = "paas-operator/service"

// Labels every chart in the catalog stamps on everything it creates, so a
// status watch on an underlying kind maps back to the CR that asked for it.
//
// LabelServiceName carries the release name — {{ .Release.Name }} to a chart —
// which is the CR's name plus its kind, not the CR's name alone. See
// Reconciler.releaseName.
const (
	LabelServiceName      = "paas.io/service-name"
	LabelServiceNamespace = "paas.io/service-namespace"
)

// MaxReleaseName is Helm's own limit on a release name, which helm-controller
// repeats on spec.releaseName. Nothing sets that field, so the derived
// HelmRelease's object name is the release name and has to fit inside it — as
// does the kind suffix releaseName appends.
const MaxReleaseName = 53

// ReleaseInterval matches the platform reconciler's.
const ReleaseInterval = 10 * time.Minute

// InstallRetries matches the platform reconciler's: helm-controller's default
// remediation is none, so a release whose first install fails once — a
// dependency's admission webhook not serving yet, say — would otherwise stay
// failed forever with nothing retrying it.
const InstallRetries = 3

// SourceName is the HelmRepository every generated kind's chart resolves
// against. It is created once per tenant namespace rather than once per
// cluster: a cross-namespace SourceRef makes source-controller name the
// derived HelmChart "<namespace>-<name>" in the *source's* namespace, and two
// tenants can pick a namespace/name pair that collides there, letting one
// tenant's release resolve another tenant's chart. Same-namespace avoids that
// by construction, is reclaimed with the namespace, and matches Flux's own
// multi-tenancy guidance.
const SourceName = "service-charts"

// Reconciler renders one generated kind. One instance runs per kind.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	GVK    schema.GroupVersionKind
	Class  *v1alpha1.ServiceClass
	// Registry is the OCI URL every chart in the catalog is pulled from.
	Registry string
	// Insecure is the transport that registry speaks. Both fields come from
	// serviceclass.SourceFrom, which reads the platform's own PackageSource —
	// the same pair the catalog's chart schemas are pulled with, so the
	// HelmRepository rendered here cannot disagree with them.
	Insecure bool
}

// SetupWithManager registers a controller for this kind on an already-running
// manager, which is why it takes a context: the engine owns its lifetime.
//
// This does not use ctrl.NewControllerManagedBy(mgr).Complete(r): Complete
// adds the controller to the manager, which runs it on the manager's own
// context rather than ctx. The engine's per-kind cancel would then do
// nothing — the controller keeps running while Stop removes its informer out
// from under it. Building it unmanaged and starting it on ctx directly makes
// the controller's lifetime actually the one the engine owns.
//
// done is called exactly once, when the controller's run loop returns — on a
// clean shutdown via ctx, but also on an internal failure such as a
// cache-sync timeout that ctx never asked for. It matches engine.Builder's
// contract so a Builder can wrap this directly.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, done func()) error {
	ctrl.LoggerFrom(ctx).Info("registering service controller", "kind", r.GVK.Kind)

	// Discarded because controllerOptions sets SkipNameValidation, which leaves
	// no reachable failure. Turning that off would make c nil on a name
	// collision and panic in the first Watch below.
	c, _ := controller.NewUnmanaged("service-"+r.Class.Name, r.controllerOptions(mgr))

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(r.GVK)
	_ = c.Watch(source.Kind(mgr.GetCache(), client.Object(cr), &handler.EnqueueRequestForObject{}))
	_ = c.Watch(source.Kind(mgr.GetCache(), client.Object(&helmv2.HelmRelease{}),
		handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), cr, handler.OnlyControllerOwner())))
	for _, s := range r.Class.Spec.StatusFrom {
		src := &unstructured.Unstructured{}
		src.SetAPIVersion(s.From.APIVersion)
		src.SetKind(s.From.Kind)
		_ = c.Watch(source.Kind(mgr.GetCache(), client.Object(src), handler.EnqueueRequestsFromMapFunc(r.byServiceLabels)))
	}

	go func() {
		defer done()
		if err := c.Start(ctx); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "service controller stopped", "kind", r.GVK.Kind)
		}
	}()
	return nil
}

// controllerOptions builds the options an unmanaged controller needs.
// controller.NewUnmanaged skips the defaulting mgr.Add would otherwise do —
// that is where MaxConcurrentReconciles, CacheSyncTimeout and, most
// importantly, the manager's logger come from. Without it, every
// "Reconciler error" for every generated kind is silently discarded: a
// tenant's object failing to reconcile would produce no log line at all.
//
// DefaultFromConfig alone is enough: manager.New's own setOptionsDefaults
// always forces Controller.Logger to a non-nil sink, so there is no real
// manager for which the result here would still need a fallback.
func (r *Reconciler) controllerOptions(mgr ctrl.Manager) controller.Options {
	skipNameValidation := true
	options := controller.Options{
		Reconciler:         r,
		SkipNameValidation: &skipNameValidation,
	}
	options.DefaultFromConfig(mgr.GetControllerOptions())
	return options
}

// byServiceLabels maps an underlying object back to the CR whose status it
// belongs in, using the labels the chart contract requires.
//
// The label carries the release name, so the kind suffix comes back off to get
// the CR's own name. An object whose label does not carry this kind's suffix
// belongs to a different generated kind and is not this controller's to
// enqueue.
func (r *Reconciler) byServiceLabels(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	release, ns := l[LabelServiceName], l[LabelServiceNamespace]
	suffix := r.nameSuffix()
	if ns == "" || !strings.HasSuffix(release, suffix) {
		return nil
	}
	name := strings.TrimSuffix(release, suffix)
	if name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}}}
}

// releaseName is the derived HelmRelease's name, and so the Helm release name
// every chart sees as {{ .Release.Name }} and stamps into the chart-contract
// labels.
//
// The CR's name alone is not enough. Two generated kinds can carry the same CR
// name in one namespace, and both would render that one HelmRelease: the API
// server refuses the second controller owner reference, and helm-controller,
// watching the chart name flip between reconciles, upgrades the release to the
// other chart — which deletes the first kind's underlying object and, with it,
// every PVC owner-referenced to it. A tenant loses a database by naming a
// cache after it.
func (r *Reconciler) releaseName(cr *unstructured.Unstructured) string {
	return cr.GetName() + r.nameSuffix()
}

func (r *Reconciler) nameSuffix() string {
	return "-" + strings.ToLower(r.GVK.Kind)
}

// Reconcile renders the derived HelmRelease and copies status back.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(r.GVK)
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.Patch(ctx, r.desiredSource(cr.GetNamespace()), client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply helmrepository %s/%s: %w", cr.GetNamespace(), SourceName, err)
	}

	hr, err := r.desired(cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, hr, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply helmrelease %s/%s: %w", hr.Namespace, hr.Name, err)
	}

	return ctrl.Result{}, r.syncStatus(ctx, cr, hr)
}

// desiredSource is the HelmRepository backing every generated kind's chart in
// one tenant namespace. It has no owner reference: several kinds' releases in
// the same namespace resolve against it, so no single CR's lifecycle should
// delete it — the namespace going away reclaims it instead.
func (r *Reconciler) desiredSource(namespace string) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		TypeMeta: metav1.TypeMeta{APIVersion: sourcev1.GroupVersion.String(), Kind: sourcev1.HelmRepositoryKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SourceName,
			Namespace: namespace,
		},
		Spec: sourcev1.HelmRepositorySpec{
			Type:     sourcev1.HelmRepositoryTypeOCI,
			URL:      r.Registry,
			Interval: metav1.Duration{Duration: ReleaseInterval},
			Insecure: r.Insecure,
		},
	}
}

func (r *Reconciler) desired(cr *unstructured.Unstructured) (*helmv2.HelmRelease, error) {
	values, found, err := unstructured.NestedMap(cr.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("read spec of %s/%s: %w", cr.GetNamespace(), cr.GetName(), err)
	}
	if !found {
		values = map[string]any{}
	}
	raw, _ := json.Marshal(values)

	name := r.releaseName(cr)
	if len(name) > MaxReleaseName {
		return nil, fmt.Errorf("release name %q for %s/%s exceeds Helm's %d-character limit; name the %s at most %d characters",
			name, cr.GetNamespace(), cr.GetName(), MaxReleaseName, r.GVK.Kind, MaxReleaseName-len(r.nameSuffix()))
	}

	yes := true
	return &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{APIVersion: helmv2.GroupVersion.String(), Kind: helmv2.HelmReleaseKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.GetNamespace(),
			Labels: map[string]string{
				LabelServiceName:      name,
				LabelServiceNamespace: cr.GetNamespace(),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: r.GVK.GroupVersion().String(),
				Kind:       r.GVK.Kind,
				Name:       cr.GetName(),
				UID:        cr.GetUID(),
				Controller: &yes,
			}},
		},
		Spec: helmv2.HelmReleaseSpec{
			Interval: metav1.Duration{Duration: ReleaseInterval},
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   r.Class.Spec.Chart.Name,
					Version: r.Class.Spec.Chart.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      SourceName,
						Namespace: cr.GetNamespace(),
					},
				},
			},
			// Retried, not terminal — see InstallRetries.
			Install: &helmv2.Install{
				Remediation: &helmv2.InstallRemediation{Retries: InstallRetries},
			},
			Upgrade: &helmv2.Upgrade{
				Remediation: &helmv2.UpgradeRemediation{Retries: InstallRetries},
			},
			Values: &apiextensionsv1.JSON{Raw: raw},
		},
	}, nil
}

func (r *Reconciler) syncStatus(ctx context.Context, cr *unstructured.Unstructured, hr *helmv2.HelmRelease) error {
	var live helmv2.HelmRelease
	if err := r.Get(ctx, client.ObjectKeyFromObject(hr), &live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read helmrelease %s/%s: %w", hr.Namespace, hr.Name, err)
	}

	ready := metav1.ConditionUnknown
	var reason, message string
	for _, c := range live.Status.Conditions {
		if c.Type == "Ready" {
			ready, reason, message = c.Status, c.Reason, c.Message
		}
	}
	if reason == "" {
		reason = "Pending"
	}

	conditions, err := conditionsFrom(cr)
	if err != nil {
		return fmt.Errorf("read conditions of %s/%s: %w", cr.GetNamespace(), cr.GetName(), err)
	}
	// SetStatusCondition only moves LastTransitionTime when Status actually
	// changes. Stamping it on every reconcile — what the brief's own syncStatus
	// did — records the last reconcile rather than the last transition, and
	// churns resourceVersion, which re-triggers this reconciler's own For()
	// watch on every pass.
	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               "Ready",
		Status:             ready,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cr.GetGeneration(),
	})

	patch := cr.DeepCopy()
	_ = unstructured.SetNestedField(patch.Object, cr.GetGeneration(), "status", "observedGeneration")
	condsOut := make([]any, 0, len(conditions))
	for _, c := range conditions {
		m, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		condsOut = append(condsOut, m)
	}
	_ = unstructured.SetNestedSlice(patch.Object, condsOut, "status", "conditions")

	for _, s := range r.Class.Spec.StatusFrom {
		v, err := r.readStatusFrom(ctx, cr, s)
		if err != nil {
			return err
		}
		// An absent value is early, not broken: the release may not have
		// created its object yet.
		if v == "" {
			continue
		}
		_ = unstructured.SetNestedField(patch.Object, v, fieldPath(s.Path)...)
	}

	// The CR can be gone by the time this lands — reconcile is level-triggered
	// and races deletion constantly — and that is not a failure to report.
	return client.IgnoreNotFound(r.Status().Patch(ctx, patch, client.MergeFrom(cr)))
}

// conditionsFrom reads the CR's existing status.conditions back into typed
// form, so meta.SetStatusCondition can compare against them and decide
// whether a transition actually happened.
func conditionsFrom(cr *unstructured.Unstructured) ([]metav1.Condition, error) {
	raw, found, err := unstructured.NestedSlice(cr.Object, "status", "conditions")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	conditions := make([]metav1.Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c metav1.Condition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c); err != nil {
			return nil, err
		}
		conditions = append(conditions, c)
	}
	return conditions, nil
}

// fieldPath turns ".status.primary" into the segments SetNestedField wants.
func fieldPath(p string) []string {
	return strings.Split(strings.TrimPrefix(p, "."), ".")
}

// readStatusFrom finds the object a chart created for this CR and reads one
// field out of it. An empty return means "not there yet", which is early
// rather than broken.
func (r *Reconciler) readStatusFrom(ctx context.Context, cr *unstructured.Unstructured, s v1alpha1.StatusSource) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(s.From.APIVersion)
	list.SetKind(s.From.Kind + "List")
	if err := r.List(ctx, list,
		client.InNamespace(cr.GetNamespace()),
		client.MatchingLabels{
			LabelServiceName:      r.releaseName(cr),
			LabelServiceNamespace: cr.GetNamespace(),
		},
	); err != nil {
		// The kind may not be served at all if the chart has not installed its
		// operator yet, which is the same "early" case as an empty list.
		if meta.IsNoMatchError(err) {
			return "", nil
		}
		return "", fmt.Errorf("list %s in %s: %w", s.From.Kind, cr.GetNamespace(), err)
	}
	if len(list.Items) == 0 {
		return "", nil
	}

	jp := jsonpath.New("statusFrom")
	if err := jp.Parse("{" + s.JSONPath + "}"); err != nil {
		return "", fmt.Errorf("parse jsonPath %q: %w", s.JSONPath, err)
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, list.Items[0].Object); err != nil {
		return "", nil
	}
	return buf.String(), nil
}
