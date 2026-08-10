// Package packagesource reconciles a PackageSource into the Flux source object
// the charts are pulled through.
package packagesource

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/flux"
)

// FieldManager owns every field these reconcilers apply. API: changing it
// orphans field ownership on every derived object in the fleet.
const FieldManager = "paas-operator/platform"

// DefaultInterval is used when a PackageSource omits one. The CRD defaults the
// field too; this covers an object written before that default existed.
const DefaultInterval = 5 * time.Minute

// Reconciler renders a PackageSource into a Flux HelmRepository.
//
// A HelmRepository of type oci rather than an OCIRepository: charts are
// addressed by name and version, which is what a Package carries, and an
// OCIRepository would pin one artifact per source instead.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PackageSource{}).
		Owns(&sourcev1.HelmRepository{}).
		Complete(r)
}

// Reconcile renders the derived HelmRepository. Level-triggered: it reads the
// whole desired state from the PackageSource every time and applies it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var src v1alpha1.PackageSource
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	repo, err := r.desired(&src)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, repo, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply helmrepository %s: %w", src.Name, err)
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) desired(src *v1alpha1.PackageSource) (*sourcev1.HelmRepository, error) {
	interval := DefaultInterval
	if src.Spec.Interval != nil {
		interval = src.Spec.Interval.Duration
	}

	repo := &sourcev1.HelmRepository{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sourcev1.GroupVersion.String(),
			Kind:       sourcev1.HelmRepositoryKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      src.Name,
			Namespace: flux.Namespace,
		},
		Spec: sourcev1.HelmRepositorySpec{
			Type:     sourcev1.HelmRepositoryTypeOCI,
			URL:      src.Spec.URL,
			Interval: metav1.Duration{Duration: interval},
			Insecure: src.Spec.Insecure,
		},
	}
	if src.Spec.SecretRef != nil {
		repo.Spec.SecretRef = &meta.LocalObjectReference{Name: src.Spec.SecretRef.Name}
	}

	// A namespaced dependent may name a cluster-scoped owner, so deleting the
	// PackageSource garbage-collects the HelmRepository in flux-system.
	if err := controllerutil.SetControllerReference(src, repo, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner on helmrepository %s: %w", src.Name, err)
	}
	return repo, nil
}
