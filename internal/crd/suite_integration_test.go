//go:build integration

package crd

import (
	"fmt"
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/rusik69/paas/api/v1alpha1"
)

var (
	restCfg   *rest.Config
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"start envtest: %v\nrun 'make test-integration', which sets KUBEBUILDER_ASSETS\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextensionsv1.AddToScheme,
		v1alpha1.AddToScheme,
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

	code := m.Run()

	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// Proves the harness itself works before anything is built on it.
func TestEnvtest_ServesTheAPI(t *testing.T) {
	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := k8sClient.List(t.Context(), crds); err != nil {
		t.Fatalf("list crds against envtest: %v", err)
	}
}
