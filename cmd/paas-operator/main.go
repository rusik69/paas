// Command paas-operator installs the platform CRDs and reconciles Platform.
//
// Today it does the first half only: it applies the CRDs it was built with and
// exits. The reconcilers arrive with the next phase-1 increment.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/rusik69/paas/internal/crd"
)

func main() {
	timeout := flag.Duration("crd-install-timeout", 2*time.Minute,
		"how long to wait for every CRD to become Established")
	flag.Parse()

	if err := run(*timeout); err != nil {
		fmt.Fprintf(os.Stderr, "paas-operator: %v\n", err)
		os.Exit(1)
	}
}

func run(timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	n, err := crd.Install(ctx, cfg, timeout)
	if err != nil {
		return err
	}
	fmt.Printf("installed %d CRDs, all Established\n", n)
	return nil
}
