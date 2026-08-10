// Package platform reconciles a Platform into the PackageSource and Packages
// one release installs.
package platform

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
)

// FieldManager is shared with the other platform reconcilers, which write
// disjoint objects.
const FieldManager = "paas-operator/platform"

// SourceName is the PackageSource every release is pulled through. One per
// cluster, because Platform is a singleton.
const SourceName = "platform"

// Condition types, following the standard meanings.
const (
	ConditionAvailable   = "Available"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"
)

// Reconciler renders a Platform into the objects one release installs.
type Reconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Fetcher Fetcher
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Platform{}).
		Owns(&v1alpha1.PackageSource{}).
		Owns(&v1alpha1.Package{}).
		Complete(r)
}

// Reconcile converges the cluster on the release named by spec.version.
//
// Symmetric in version: it applies whatever the spec says, in either
// direction, so a rollback is an ordinary reconcile rather than a special case.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Platform
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	release, err := r.Fetcher.Fetch(ctx, p.Spec.Registry, p.Spec.Version)
	if err != nil {
		return ctrl.Result{}, r.degraded(ctx, &p, "FetchFailed", err)
	}

	if err := r.applySource(ctx, &p); err != nil {
		return ctrl.Result{}, r.degraded(ctx, &p, "ApplyFailed", err)
	}
	if err := r.applyPackages(ctx, &p, release); err != nil {
		return ctrl.Result{}, r.degraded(ctx, &p, "ApplyFailed", err)
	}
	if err := r.prune(ctx, &p, release); err != nil {
		return ctrl.Result{}, r.degraded(ctx, &p, "PruneFailed", err)
	}

	return ctrl.Result{}, r.available(ctx, &p, release)
}

func (r *Reconciler) applySource(ctx context.Context, p *v1alpha1.Platform) error {
	src := &v1alpha1.PackageSource{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "PackageSource",
		},
		ObjectMeta: metav1.ObjectMeta{Name: SourceName},
		Spec: v1alpha1.PackageSourceSpec{
			URL: p.Spec.Registry,
			// The phase-0 in-cluster registry speaks plain HTTP, which is why
			// no CA has to reach every node.
			Insecure: true,
		},
	}
	if err := controllerutil.SetControllerReference(p, src, r.Scheme); err != nil {
		return fmt.Errorf("set owner on packagesource: %w", err)
	}
	if err := r.Patch(ctx, src, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply packagesource: %w", err)
	}
	return nil
}

func (r *Reconciler) applyPackages(ctx context.Context, p *v1alpha1.Platform, release *Release) error {
	for _, e := range release.Packages {
		obj := &v1alpha1.Package{
			TypeMeta: metav1.TypeMeta{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "Package",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:   e.Name,
				Labels: map[string]string{pkgctl.PlatformLabel: p.Name},
			},
			Spec: v1alpha1.PackageSpec{
				SourceRef: v1alpha1.LocalRef{Name: SourceName},
				Chart:     e.Chart,
				Version:   e.Version,
				Stage:     e.Stage,
				Values:    e.Values,
			},
		}
		if err := controllerutil.SetControllerReference(p, obj, r.Scheme); err != nil {
			return fmt.Errorf("set owner on package %s: %w", e.Name, err)
		}
		if err := r.Patch(ctx, obj, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply package %s: %w", e.Name, err)
		}
	}
	return nil
}

// prune deletes Packages this platform owns that the new release no longer
// declares.
//
// Without it an upgrade only ever adds, and "roll out a complete platform
// version" quietly means "roll out a superset of every version so far".
func (r *Reconciler) prune(ctx context.Context, p *v1alpha1.Platform, release *Release) error {
	var owned v1alpha1.PackageList
	if err := r.List(ctx, &owned, client.MatchingLabels{pkgctl.PlatformLabel: p.Name}); err != nil {
		return fmt.Errorf("list owned packages: %w", err)
	}

	wanted := make(map[string]bool, len(release.Packages))
	for _, e := range release.Packages {
		wanted[e.Name] = true
	}

	for i := range owned.Items {
		obj := &owned.Items[i]
		if wanted[obj.Name] {
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete package %s: %w", obj.Name, err)
		}
	}
	return nil
}

func (r *Reconciler) available(ctx context.Context, p *v1alpha1.Platform, release *Release) error {
	p.Status.ObservedGeneration = p.Generation
	p.Status.Current = &v1alpha1.ReleaseRef{Version: release.Version, Digest: release.Digest}
	setCondition(p, ConditionAvailable, metav1.ConditionTrue, "RolloutComplete",
		fmt.Sprintf("release %s is applied", release.Version))
	setCondition(p, ConditionProgressing, metav1.ConditionFalse, "RolloutComplete", "")
	setCondition(p, ConditionDegraded, metav1.ConditionFalse, "RolloutComplete", "")
	return r.Status().Update(ctx, p)
}

// degraded records why and returns the original error, so the caller requeues.
// The status write's own failure must not mask the cause.
func (r *Reconciler) degraded(ctx context.Context, p *v1alpha1.Platform, reason string, cause error) error {
	p.Status.ObservedGeneration = p.Generation
	setCondition(p, ConditionDegraded, metav1.ConditionTrue, reason, cause.Error())
	setCondition(p, ConditionAvailable, metav1.ConditionFalse, reason, cause.Error())
	if err := r.Status().Update(ctx, p); err != nil {
		return fmt.Errorf("%w (and recording it failed: %w)", cause, err)
	}
	return cause
}

func setCondition(p *v1alpha1.Platform, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: p.Generation,
	})
}
