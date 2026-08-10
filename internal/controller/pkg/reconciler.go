// Package pkg reconciles a Package into the Flux HelmRelease that installs it.
package pkg

import (
	"context"
	"fmt"
	"sort"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/flux"
)

// FieldManager is shared with the other platform reconcilers: they write
// disjoint objects, and one manager per writer keeps ownership legible.
const FieldManager = "paas-operator/platform"

// ReleaseInterval is how often helm-controller re-checks a release.
const ReleaseInterval = 10 * time.Minute

// PlatformLabel groups the Packages belonging to one platform release. It is
// what makes "every migration in the same release" answerable with a List.
const PlatformLabel = "platform.paas.io/platform"

// TargetNamespace is where platform components are installed.
//
// Not flux-system. Flux's own install ships a NetworkPolicy selecting every pod
// in its namespace and permitting ingress on port 8080 alone, so a component
// installed there is unreachable on any other port — CNPG's admission webhook
// listens on 9443, and the API server could not call it. The HelmRelease objects
// still live in flux-system, because that is where helm-controller reconciles
// them; only the releases land here.
const TargetNamespace = "paas-system"

// Reconciler renders a Package into a Flux HelmRelease.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Package{}).
		Owns(&helmv2.HelmRelease{}).
		Complete(r)
}

// Reconcile renders the derived HelmRelease.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Package
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	deps, err := r.dependencies(ctx, &p)
	if err != nil {
		return ctrl.Result{}, err
	}

	hr, err := r.desired(&p, deps)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, hr, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply helmrelease %s: %w", p.Name, err)
	}
	return ctrl.Result{}, nil
}

// dependencies returns the migration Packages a component must wait for.
//
// Flux enforces the ordering, not us: a HelmRelease with dependsOn is not
// reconciled until every dependency reports Ready. Expressing it once
// declaratively is why there is no sequencing logic in this reconciler.
func (r *Reconciler) dependencies(ctx context.Context, p *v1alpha1.Package) ([]helmv2.DependencyReference, error) {
	if p.Spec.Stage != v1alpha1.StageComponent {
		return nil, nil
	}

	var all v1alpha1.PackageList
	if err := r.List(ctx, &all, client.MatchingLabels{PlatformLabel: p.Labels[PlatformLabel]}); err != nil {
		return nil, fmt.Errorf("list packages for %s: %w", p.Name, err)
	}

	var deps []helmv2.DependencyReference
	for _, other := range all.Items {
		if other.Spec.Stage == v1alpha1.StageMigration {
			deps = append(deps, helmv2.DependencyReference{
				Name:      other.Name,
				Namespace: flux.Namespace,
			})
		}
	}
	// Sorted, or the rendered dependsOn reorders between reconciles and the
	// object churns for no reason.
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

func (r *Reconciler) desired(p *v1alpha1.Package, deps []helmv2.DependencyReference) (*helmv2.HelmRelease, error) {
	hr := &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{
			APIVersion: helmv2.GroupVersion.String(),
			Kind:       helmv2.HelmReleaseKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: flux.Namespace,
		},
		Spec: helmv2.HelmReleaseSpec{
			Interval:        metav1.Duration{Duration: ReleaseInterval},
			DependsOn:       deps,
			TargetNamespace: TargetNamespace,
			// Kept beside the release rather than in flux-system, so a namespace
			// deleted by hand takes its Helm history with it instead of leaving
			// state that disagrees with the cluster.
			StorageNamespace: TargetNamespace,
			Install:          &helmv2.Install{CreateNamespace: true},
			Chart: &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   p.Spec.Chart,
					Version: p.Spec.Version,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      p.Spec.SourceRef.Name,
						Namespace: flux.Namespace,
					},
				},
			},
		},
	}
	if p.Spec.Values != nil && len(p.Spec.Values.Raw) > 0 {
		hr.Spec.Values = &apiextensionsv1.JSON{Raw: p.Spec.Values.Raw}
	}

	if err := controllerutil.SetControllerReference(p, hr, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner on helmrelease %s: %w", p.Name, err)
	}
	return hr, nil
}
