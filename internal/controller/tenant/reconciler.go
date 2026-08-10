// Package tenant reconciles a Tenant into the namespace, quota and limits that
// back it.
package tenant

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
	"github.com/rusik69/paas/pkg/tenancy"
)

// FieldManager owns every field this reconciler applies. API: changing it
// orphans ownership of every tenant namespace already created.
const FieldManager = "paas-operator/tenant"

// Condition types, with the standard meanings.
const (
	ConditionReady    = "Ready"
	ConditionDegraded = "Degraded"
)

// TenantLabel marks the objects backing one tenant, and is what the isolation
// policies will select on.
const TenantLabel = "core.paas.io/tenant"

// Finalizer holds a Tenant open until its namespace is gone.
//
// A finalizer rather than owner references, because Kubernetes permits neither
// reference this would need: a cluster-scoped Namespace may not have a
// namespace-scoped owner, and a ResourceQuota in tenant-acme may not be owned by
// a Tenant in tenant-root. Both are rejected outright, which is worth knowing
// before reaching for the usual pattern.
const Finalizer = "core.paas.io/tenant-namespace"

// Reconciler renders a Tenant into its namespace, quota and limits.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Tenant{}).
		Owns(&corev1.Namespace{}).
		Complete(r)
}

// Reconcile converges a tenant's namespace, quota and limits.
//
// Every depth gets its own quota and its own limits. ADR 0004: a child is not
// trusted because its parent is.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t corev1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	namespace, err := tenancy.NamespaceOf(&t)
	if !t.DeletionTimestamp.IsZero() {
		if err != nil {
			// Nothing was ever created for a path that never derived, so there
			// is nothing to reclaim and no reason to hold the object open.
			return ctrl.Result{}, r.removeFinalizer(ctx, &t)
		}
		return r.finalize(ctx, &t, namespace)
	}
	if err := r.addFinalizer(ctx, &t); err != nil {
		return ctrl.Result{}, err
	}

	if err != nil {
		// Terminal: the path is derived from where the object lives, so this
		// cannot become true on a retry. Reported and dropped rather than
		// requeued forever.
		return ctrl.Result{}, r.degraded(ctx, &t, "InvalidPath", err)
	}

	limits, ok := LimitsFor(t.Spec.Plan)
	if !ok {
		return ctrl.Result{}, r.degraded(ctx, &t, "UnknownPlan",
			fmt.Errorf("plan %q has no limits defined", t.Spec.Plan))
	}

	if err := r.apply(ctx, &t, namespace, limits); err != nil {
		return ctrl.Result{}, r.degraded(ctx, &t, "ApplyFailed", err)
	}

	modules, err := r.resolveModules(ctx, &t)
	if err != nil {
		return ctrl.Result{}, r.degraded(ctx, &t, "ResolveFailed", err)
	}
	return ctrl.Result{}, r.ready(ctx, &t, namespace, modules)
}

func (r *Reconciler) apply(ctx context.Context, t *corev1alpha1.Tenant, namespace string, limits Limits) error {
	objects := []client.Object{
		&corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{TenantLabel: t.Name}},
		},
		&corev1.ResourceQuota{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
			ObjectMeta: metav1.ObjectMeta{
				Name: "tenant", Namespace: namespace,
				Labels: map[string]string{TenantLabel: t.Name},
			},
			Spec: corev1.ResourceQuotaSpec{Hard: limits.quota()},
		},
		&corev1.LimitRange{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"},
			ObjectMeta: metav1.ObjectMeta{
				Name: "tenant", Namespace: namespace,
				Labels: map[string]string{TenantLabel: t.Name},
			},
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				Default:        limits.defaults(),
				DefaultRequest: limits.defaults(),
			}}},
		},
	}

	// Isolation does not inherit: every namespace gets its own policies at every
	// depth, because a child is not trusted for being someone's child.
	for _, policy := range policiesFor(namespace, t.Name) {
		objects = append(objects, policy)
	}

	for _, obj := range objects {
		if err := r.Patch(ctx, obj, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s: %w",
				obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

// resolveModules records where each module a tenant declares resolved to.
//
// Every module named anywhere in the tenant's own spec is resolved, including
// ones it sets to false — false means "use my parent's", so those are exactly
// the interesting entries.
func (r *Reconciler) resolveModules(ctx context.Context, t *corev1alpha1.Tenant) (map[string]corev1alpha1.ModuleStatus, error) {
	if len(t.Spec.Modules) == 0 {
		return nil, nil
	}

	out := make(map[string]corev1alpha1.ModuleStatus, len(t.Spec.Modules))
	for name := range t.Spec.Modules {
		provider, found, err := tenancy.Resolve(ctx, r.Client, t, name)
		if err != nil {
			return nil, fmt.Errorf("resolve module %q: %w", name, err)
		}
		if !found {
			continue
		}
		providerNamespace, err := tenancy.NamespaceOf(provider)
		if err != nil {
			return nil, fmt.Errorf("namespace of module %q provider: %w", name, err)
		}
		out[name] = corev1alpha1.ModuleStatus{
			Tenant:    provider.Name,
			Namespace: providerNamespace,
			Inherited: provider.Name != t.Name || provider.Namespace != t.Namespace,
		}
	}
	return out, nil
}

func (r *Reconciler) ready(ctx context.Context, t *corev1alpha1.Tenant, namespace string, modules map[string]corev1alpha1.ModuleStatus) error {
	t.Status.ObservedGeneration = t.Generation
	t.Status.Namespace = namespace
	t.Status.Modules = modules
	setCondition(t, ConditionReady, metav1.ConditionTrue, "Reconciled",
		"namespace "+namespace+" is provisioned")
	setCondition(t, ConditionDegraded, metav1.ConditionFalse, "Reconciled", "")
	return r.Status().Update(ctx, t)
}

// degraded records why and returns the cause, so the status write's own failure
// cannot mask it.
func (r *Reconciler) degraded(ctx context.Context, t *corev1alpha1.Tenant, reason string, cause error) error {
	t.Status.ObservedGeneration = t.Generation
	setCondition(t, ConditionDegraded, metav1.ConditionTrue, reason, cause.Error())
	setCondition(t, ConditionReady, metav1.ConditionFalse, reason, cause.Error())
	if err := r.Status().Update(ctx, t); err != nil {
		return fmt.Errorf("%w (and recording it failed: %w)", cause, err)
	}
	return cause
}

// finalize deletes the tenant's namespace and holds the object open until it is
// gone.
//
// Deleting the namespace reclaims everything inside it, including descendant
// Tenants — whose own finalizers then delete their namespaces, so the tree
// cascades. The finalizer is released only once the namespace has actually
// disappeared, or a failed delete would orphan it.
func (r *Reconciler) finalize(ctx context.Context, t *corev1alpha1.Tenant, namespace string) (ctrl.Result, error) {
	var ns corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns)
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, r.removeFinalizer(ctx, t)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get namespace %s: %w", namespace, err)
	}

	// Only namespaces this reconciler created. A Tenant naming a namespace it
	// does not own must not be able to delete it.
	if ns.Labels[TenantLabel] != t.Name {
		return ctrl.Result{}, r.removeFinalizer(ctx, t)
	}

	if ns.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete namespace %s: %w", namespace, err)
		}
	}
	// Terminating: descendants are still being reclaimed. Requeue rather than
	// release, so the tenant outlives what it owns.
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *Reconciler) addFinalizer(ctx context.Context, t *corev1alpha1.Tenant) error {
	if controllerutil.ContainsFinalizer(t, Finalizer) {
		return nil
	}
	patch := client.MergeFrom(t.DeepCopy())
	controllerutil.AddFinalizer(t, Finalizer)
	if err := r.Patch(ctx, t, patch); err != nil {
		return fmt.Errorf("add finalizer to %s/%s: %w", t.Namespace, t.Name, err)
	}
	return nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, t *corev1alpha1.Tenant) error {
	if !controllerutil.ContainsFinalizer(t, Finalizer) {
		return nil
	}
	patch := client.MergeFrom(t.DeepCopy())
	controllerutil.RemoveFinalizer(t, Finalizer)
	if err := r.Patch(ctx, t, patch); err != nil {
		return fmt.Errorf("remove finalizer from %s/%s: %w", t.Namespace, t.Name, err)
	}
	return nil
}

func setCondition(t *corev1alpha1.Tenant, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: t.Generation,
	})
}
