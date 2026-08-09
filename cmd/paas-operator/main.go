// Command paas-operator installs the platform CRDs, bootstraps Flux, and runs
// the Platform, PackageSource and Package reconcilers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/rusik69/paas/internal/controller/platform"
	"github.com/rusik69/paas/internal/crd"
	"github.com/rusik69/paas/internal/flux"
	"github.com/rusik69/paas/internal/operator"
)

func main() {
	installTimeout := flag.Duration("install-timeout", 5*time.Minute,
		"how long to wait for the CRDs and Flux to install before giving up")
	metricsAddress := flag.String("metrics-bind-address", ":8080",
		`address the metrics endpoint binds to; "0" disables it`)
	insecureRegistry := flag.Bool("insecure-registry", true,
		"pull release artifacts over plain HTTP; the in-cluster registry speaks it")
	flag.Parse()

	if err := run(*installTimeout, *metricsAddress, *insecureRegistry); err != nil {
		fmt.Fprintf(os.Stderr, "paas-operator: %v\n", err)
		os.Exit(1)
	}
}

func run(installTimeout time.Duration, metricsAddress string, insecureRegistry bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	// Install before the manager starts: its caches watch kinds that do not
	// exist until the CRDs are Established, and starting first makes that a
	// race the operator loses on a clean cluster.
	if err := install(ctx, cfg, installTimeout); err != nil {
		return err
	}

	mgr, err := operator.NewManager(cfg, operator.Options{
		MetricsAddress: metricsAddress,
		Fetcher:        &platform.OCIFetcher{Insecure: insecureRegistry},
	})
	if err != nil {
		return err
	}
	return operator.Run(ctx, mgr)
}

func install(ctx context.Context, cfg *rest.Config, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := client.New(cfg, client.Options{Scheme: operator.Scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	n, err := crd.Apply(ctx, c)
	if err != nil {
		return err
	}
	if err := flux.Bootstrap(ctx, c); err != nil {
		return fmt.Errorf("bootstrap flux: %w", err)
	}
	fmt.Printf("installed %d CRDs and the Flux controllers\n", n)
	return nil
}
