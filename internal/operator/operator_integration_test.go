//go:build integration

package operator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/rusik69/paas/internal/controller/platform"
)

var restCfg *rest.Config

func TestMain(m *testing.M) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"start envtest: %v\nrun 'make test-integration', which sets KUBEBUILDER_ASSETS\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	code := m.Run()

	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

type nopFetcher struct{}

func (nopFetcher) Fetch(context.Context, string, string) (*platform.Release, error) {
	return nil, errors.New("not implemented")
}

// One manager per process: controller-runtime rejects a second controller with
// the same name, so registration and shutdown are asserted against one.
//
// Registration is where a missing scheme entry or a watch on an unserved kind
// surfaces. Shutdown matters because a Run that ignores cancellation leaves the
// operator alive after SIGTERM until the kubelet kills it.
func TestManager_RegistersEveryReconcilerAndStopsOnCancel(t *testing.T) {
	mgr, err := NewManager(restCfg, Options{MetricsAddress: "0", Fetcher: nopFetcher{}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, mgr) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("Run did not return after its context was cancelled")
	}

	// A manager cannot be restarted, so this is also the only place the failure
	// path out of Run is reachable.
	if err := Run(t.Context(), mgr); err == nil {
		t.Error("restarting a stopped manager was accepted")
	} else if !strings.Contains(err.Error(), "run manager") {
		t.Errorf("err = %q, want it to name the step that failed", err)
	}

	// Last, and in this test rather than its own: controller-runtime keeps the
	// controller-name registry for the whole process, so a second NewManager
	// always fails once the first has registered. Asserting it here keeps the
	// ordering explicit instead of leaving it to test-declaration order.
	if _, err := NewManager(restCfg, Options{MetricsAddress: "0", Fetcher: nopFetcher{}}); err == nil {
		t.Error("registering the same controllers twice was accepted")
	} else if !strings.Contains(err.Error(), "register packagesource reconciler") {
		t.Errorf("err = %q, want it to name the reconciler that could not register", err)
	}
}

func TestNewManager_ReportsAnUnusableConfig(t *testing.T) {
	bad := *restCfg
	bad.TLSClientConfig = rest.TLSClientConfig{CAFile: "/nonexistent/ca.crt"}

	if _, err := NewManager(&bad, Options{MetricsAddress: "0"}); err == nil {
		t.Error("an unusable rest.Config produced a manager")
	}
}
