//go:build integration

// Package controller holds the envtest harness shared by the platform
// reconcilers. Each reconciler's assertions live beside it in this package, so
// one API server is started for all of them rather than one per package.
package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	"github.com/rusik69/paas/internal/crd"
	"github.com/rusik69/paas/internal/flux"
)

var (
	k8sClient client.Client
	scheme    *runtime.Scheme
	restCfg   *rest.Config
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	restCfg = cfg
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"start envtest: %v\nrun 'make test-integration', which sets KUBEBUILDER_ASSETS\n", err)
		os.Exit(1)
	}

	scheme = runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextensionsv1.AddToScheme,
		v1alpha1.AddToScheme,
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			fmt.Fprintf(os.Stderr, "build scheme: %v\n", err)
			os.Exit(1)
		}
	}

	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Our own CRDs, then Flux's, then the namespace Flux objects live in.
	// Installing them through the same code paths the operator uses means a
	// break in either shows up here rather than only on a cluster.
	if _, err := crd.Apply(ctx, k8sClient); err != nil {
		fmt.Fprintf(os.Stderr, "install paas CRDs: %v\n", err)
		os.Exit(1)
	}
	if err := flux.Bootstrap(ctx, k8sClient); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap flux: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()

	if err := k8sClient.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := k8sClient.Delete(ctx, obj); err != nil {
			t.Logf("cleanup: delete %T %s: %v", obj, obj.GetName(), err)
		}
	})
}
