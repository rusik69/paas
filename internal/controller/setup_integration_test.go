//go:build integration

package controller

import (
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rusik69/paas/internal/controller/packagesource"
	pkgctl "github.com/rusik69/paas/internal/controller/pkg"
	platformctl "github.com/rusik69/paas/internal/controller/platform"
)

// Registration is where a missing scheme entry or a watch on a kind the cluster
// does not serve surfaces. The manager is built but never started: this asserts
// the wiring, and the behaviour is asserted by the reconcile tests.
func TestSetupWithManager_RegistersBothReconcilers(t *testing.T) {
	mgr, err := manager.New(restCfg, ctrl.Options{
		Scheme: scheme,
		// Off, or two test binaries racing for the same port fail for a reason
		// that has nothing to do with the controllers.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	if err := (&packagesource.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
		SetupWithManager(mgr); err != nil {
		t.Errorf("register packagesource reconciler: %v", err)
	}
	if err := (&pkgctl.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
		SetupWithManager(mgr); err != nil {
		t.Errorf("register package reconciler: %v", err)
	}
	if err := (&platformctl.Reconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Fetcher: &fakeFetcher{},
	}).SetupWithManager(mgr); err != nil {
		t.Errorf("register platform reconciler: %v", err)
	}
}
