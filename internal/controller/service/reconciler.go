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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/flux"
)

// FieldManager is this reconciler's alone. The platform reconcilers write
// disjoint objects under their own, and one manager per writer keeps ownership
// legible.
const FieldManager = "paas-operator/service"

// Labels every chart in the catalog stamps on everything it creates, so a
// status watch on an underlying kind maps back to the CR that asked for it.
const (
	LabelServiceName      = "paas.io/service-name"
	LabelServiceNamespace = "paas.io/service-namespace"
)

// ReleaseInterval matches the platform reconciler's.
const ReleaseInterval = 10 * time.Minute

// SourceName is the HelmRepository every generated kind's chart resolves
// against. One repository serves the whole service catalog regardless of how
// many kinds are running, so every Reconciler instance applies the same
// object rather than each kind owning its own.
const SourceName = "service-charts"

// Reconciler renders one generated kind. One instance runs per kind.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	GVK      schema.GroupVersionKind
	Class    *v1alpha1.ServiceClass
	Registry string
}

// SetupWithManager registers a controller for this kind on an already-running
// manager, which is why it takes a context: the engine owns its lifetime.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	ctrl.LoggerFrom(ctx).Info("registering service controller", "kind", r.GVK.Kind)

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(r.GVK)

	b := ctrl.NewControllerManagedBy(mgr).
		Named("service-" + r.Class.Name).
		For(cr).
		Owns(&helmv2.HelmRelease{})

	for _, s := range r.Class.Spec.StatusFrom {
		src := &unstructured.Unstructured{}
		src.SetAPIVersion(s.From.APIVersion)
		src.SetKind(s.From.Kind)
		b = b.WatchesRawSource(source.Kind(mgr.GetCache(), client.Object(src),
			handler.EnqueueRequestsFromMapFunc(byServiceLabels)))
	}
	return b.Complete(r)
}

// byServiceLabels maps an underlying object back to the CR whose status it
// belongs in, using the labels the chart contract requires.
func byServiceLabels(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	name, ns := l[LabelServiceName], l[LabelServiceNamespace]
	if name == "" || ns == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}}}
}

// Reconcile renders the derived HelmRelease and copies status back.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(r.GVK)
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.Patch(ctx, r.desiredSource(), client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply helmrepository %s: %w", SourceName, err)
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

// desiredSource is the HelmRepository backing every generated kind's chart.
// It has no single owner — many kinds' HelmReleases resolve against it — so
// it is applied here rather than tied to one CR's lifecycle.
func (r *Reconciler) desiredSource() *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		TypeMeta: metav1.TypeMeta{APIVersion: sourcev1.GroupVersion.String(), Kind: sourcev1.HelmRepositoryKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SourceName,
			Namespace: flux.Namespace,
		},
		Spec: sourcev1.HelmRepositorySpec{
			Type:     sourcev1.HelmRepositoryTypeOCI,
			URL:      r.Registry,
			Interval: metav1.Duration{Duration: ReleaseInterval},
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
	// json.Marshal cannot fail here: values came out of unstructured.NestedMap,
	// whose own deep copy already panics on anything that is not JSON-safe, so
	// nothing reaches this point that Marshal would reject.
	raw, _ := json.Marshal(values)

	yes := true
	return &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{APIVersion: helmv2.GroupVersion.String(), Kind: helmv2.HelmReleaseKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.GetName(),
			Namespace: cr.GetNamespace(),
			Labels: map[string]string{
				LabelServiceName:      cr.GetName(),
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
						Namespace: flux.Namespace,
					},
				},
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

	patch := cr.DeepCopy()
	_ = unstructured.SetNestedField(patch.Object, cr.GetGeneration(), "status", "observedGeneration")
	conds := []any{map[string]any{
		"type":               "Ready",
		"status":             string(ready),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
		"observedGeneration": cr.GetGeneration(),
	}}
	_ = unstructured.SetNestedSlice(patch.Object, conds, "status", "conditions")

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

	return r.Status().Patch(ctx, patch, client.MergeFrom(cr))
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
			LabelServiceName:      cr.GetName(),
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
