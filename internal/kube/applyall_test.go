package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Error propagation only. Every claim about server-side apply semantics is made
// against envtest, because testing SSA against a fake tests the fake.
func TestApplyAll_NamesTheObjectThatFailed(t *testing.T) {
	t.Parallel()

	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			},
		}).Build()

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ServiceAccount")
	obj.SetNamespace("flux-system")
	obj.SetName("source-controller")

	err := ApplyAll(t.Context(), c, []*unstructured.Unstructured{obj}, "test")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	for _, want := range []string{"ServiceAccount", "flux-system", "source-controller"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

func TestApplyAll_EmptyInputIsANoOp(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	if err := ApplyAll(t.Context(), c, nil, "test"); err != nil {
		t.Errorf("ApplyAll(nil) = %v, want nil", err)
	}
}
