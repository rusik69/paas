// Package operator wires the platform's controllers onto a manager.
//
// It exists so cmd/paas-operator stays flag wiring: everything here is
// reachable from a test, and main() is not.
package operator

import (
	"context"
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/controller/packagesource"
	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
	"github.com/rusik69/paas/internal/controller/platform"
)

// Scheme carries every kind the operator reads or writes. Registered once into
// a fresh scheme, which cannot fail: a duplicate registration here is a
// programmer error, not a runtime one.
var Scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextensionsv1.AddToScheme,
		v1alpha1.AddToScheme,
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
	} {
		utilruntime.Must(add(s))
	}
	return s
}()

// Options configures the manager.
type Options struct {
	// MetricsAddress is the metrics bind address; "0" disables the server.
	MetricsAddress string
	// Fetcher resolves a platform version into its release.
	Fetcher platform.Fetcher
}

// NewManager builds a manager with every platform reconciler registered.
//
// Separate from Run so a test can assert the wiring without starting anything:
// registration is where a missing scheme entry or a watch on an unserved kind
// surfaces.
func NewManager(cfg *rest.Config, opts Options) (manager.Manager, error) {
	mgr, err := manager.New(cfg, ctrl.Options{
		Scheme:  Scheme,
		Metrics: metricsserver.Options{BindAddress: opts.MetricsAddress},
	})
	if err != nil {
		return nil, fmt.Errorf("build manager: %w", err)
	}

	// Table rather than three near-identical blocks: one error path to get
	// right, and the reconciler's name in the message comes from the same place
	// every time.
	setups := []struct {
		name  string
		setup func(ctrl.Manager) error
	}{
		{"packagesource", (&packagesource.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager},
		{"package", (&pkgctl.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager},
		{"platform", (&platform.Reconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Fetcher: opts.Fetcher,
		}).SetupWithManager},
	}
	for _, s := range setups {
		if err := s.setup(mgr); err != nil {
			return nil, fmt.Errorf("register %s reconciler: %w", s.name, err)
		}
	}
	return mgr, nil
}

// Run starts the manager and blocks until ctx is cancelled.
func Run(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}
