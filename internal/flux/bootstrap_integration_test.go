//go:build integration

package flux

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/rusik69/paas/pkg/wait"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"start envtest: %v\nrun 'make test-integration', which sets KUBEBUILDER_ASSETS\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextensionsv1.AddToScheme,
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

func TestBootstrap_InstallsTheControllersAndTheirCRDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	if err := Bootstrap(ctx, k8sClient); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, name := range []string{"source-controller", "helm-controller"} {
		d := &appsv1.Deployment{}
		key := types.NamespacedName{Namespace: Namespace, Name: name}
		if err := k8sClient.Get(ctx, key, d); err != nil {
			t.Errorf("get deployment %s: %v", name, err)
		}
	}

	// Established, not merely present: the reconcilers apply these kinds, and a
	// CRD the apiserver has not finished accepting fails those applies with a
	// no-matches error that names the kind rather than the cause.
	for _, name := range []string{
		"ocirepositories.source.toolkit.fluxcd.io",
		"helmreleases.helm.toolkit.fluxcd.io",
	} {
		err := wait.For(ctx, time.Second, "crd "+name+" Established",
			func(ctx context.Context) (bool, error) {
				got := &apiextensionsv1.CustomResourceDefinition{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
					return false, nil
				}
				for _, c := range got.Status.Conditions {
					if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
						return true, nil
					}
				}
				return false, nil
			})
		if err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestBootstrap_IsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	if err := Bootstrap(ctx, k8sClient); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	before := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: Namespace, Name: "source-controller"}
	if err := k8sClient.Get(ctx, key, before); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if err := Bootstrap(ctx, k8sClient); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	after := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, key, after); err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if before.Generation != after.Generation {
		t.Errorf("generation moved %d -> %d on a no-op bootstrap",
			before.Generation, after.Generation)
	}
}
