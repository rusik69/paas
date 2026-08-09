package crd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The fake client is used here only for error propagation. Every claim about
// server-side apply semantics is made against envtest instead, because testing
// SSA against a fake tests the fake.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return s
}

func TestApply_ReportsWhichCRDFailed(t *testing.T) {
	t.Parallel()

	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			},
		}).Build()

	err := Apply(t.Context(), c)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	if !strings.Contains(err.Error(), "platforms.platform.paas.io") {
		t.Errorf("err = %q, want it to name the CRD that failed", err)
	}
}

// Names already claimed by another CRD are never granted, so this must report
// the conflict rather than spend the whole deadline and report a timeout.
func TestWaitEstablished_NamesRejectedIsTerminal(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "platforms.platform.paas.io"},
			Status: apiextensionsv1.CustomResourceDefinitionStatus{
				Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
					Type:    apiextensionsv1.NamesAccepted,
					Status:  apiextensionsv1.ConditionFalse,
					Message: `"platforms" is already in use`,
				}},
			},
		}).Build()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := waitEstablished(ctx, c, "platforms.platform.paas.io")
	if err == nil {
		t.Fatal("names rejected was treated as still converging")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("err = %q, want the apiserver's reason", err)
	}
}

func TestWaitEstablished_ReturnsWhenEstablished(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "platforms.platform.paas.io"},
			Status: apiextensionsv1.CustomResourceDefinitionStatus{
				Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
					Type:   apiextensionsv1.Established,
					Status: apiextensionsv1.ConditionTrue,
				}},
			},
		}).Build()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := waitEstablished(ctx, c, "platforms.platform.paas.io"); err != nil {
		t.Errorf("waitEstablished: %v", err)
	}
}

// A CRD that never appears must expire rather than block forever.
func TestWaitEstablished_MissingCRDExpires(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if err := waitEstablished(ctx, c, "absent.platform.paas.io"); err == nil {
		t.Error("a CRD that never appeared was reported as Established")
	}
}
