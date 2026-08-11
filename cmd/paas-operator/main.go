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
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/rusik69/paas/internal/chart"
	"github.com/rusik69/paas/internal/controller/platform"
	"github.com/rusik69/paas/internal/controller/tenant"
	"github.com/rusik69/paas/internal/crd"
	"github.com/rusik69/paas/internal/flux"
	"github.com/rusik69/paas/internal/operator"
)

func main() {
	installTimeout := flag.Duration("install-timeout", 5*time.Minute,
		"how long to wait for the CRDs and Flux to install before giving up")
	// zap's own flags, so verbosity is a deployment concern rather than a
	// rebuild: -zap-log-level=debug turns on the V(1) reconcile detail
	// go-guidelines reserves for it.
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)

	metricsAddress := flag.String("metrics-bind-address", ":8080",
		`address the metrics endpoint binds to; "0" disables it`)
	// The address a tenant's CI kubeconfig should dial. The operator's own
	// connection is the in-cluster ClusterIP, which is correct for pods and
	// useless to anything outside — so an externally reachable URL has to be
	// supplied, and there is nowhere the operator could infer it from.
	apiEndpoint := flag.String("api-endpoint-url", "",
		"externally reachable API server URL for generated tenant kubeconfigs; "+
			"empty disables kubeconfig generation")
	insecureRegistry := flag.Bool("insecure-registry", true,
		"pull release artifacts over plain HTTP; the in-cluster registry speaks it")
	flag.Parse()

	// Before anything that might log. Without it controller-runtime discards
	// every line and says so once, which is how an operator ends up with no
	// logs at all during an incident.
	ctrllog.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if err := run(*installTimeout, *metricsAddress, *apiEndpoint, *insecureRegistry); err != nil {
		fmt.Fprintf(os.Stderr, "paas-operator: %v\n", err)
		os.Exit(1)
	}
}

func run(installTimeout time.Duration, metricsAddress, apiEndpoint string, insecureRegistry bool) error {
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
		APIEndpoint:    tenant.APIEndpoint{URL: apiEndpoint, CA: apiServerCA(cfg)},
		Fetcher:        &platform.OCIFetcher{Insecure: insecureRegistry},
		SchemaFetcher:  &chart.OCIFetcher{Insecure: insecureRegistry},
	})
	if err != nil {
		return err
	}
	return operator.Run(ctx, mgr)
}

// apiServerCA returns the CA a generated kubeconfig needs to trust the API
// server, from whichever of the two places the in-cluster config carries it.
func apiServerCA(cfg *rest.Config) []byte {
	if len(cfg.CAData) > 0 {
		return cfg.CAData
	}
	if cfg.CAFile == "" {
		return nil
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		// Not fatal: the reconciler refuses to write a kubeconfig it cannot
		// make usable, and the operator's other work is unaffected.
		fmt.Fprintf(os.Stderr, "paas-operator: read API server CA: %v\n", err)
		return nil
	}
	return ca
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
