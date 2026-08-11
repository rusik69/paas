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
	"k8s.io/client-go/rest"
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

	_, err := Apply(t.Context(), c)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
	// Whichever CRD it reached first, not a hardcoded name: the set grows, and
	// a test pinned to one member fails for the wrong reason when it does.
	crds, loadErr := Load()
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if !strings.Contains(err.Error(), crds[0].Name) {
		t.Errorf("err = %q, want it to name %s, the CRD it failed on", err, crds[0].Name)
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

// A transient API error must not abort the wait. Returning it would turn a
// one-second blip during startup into a failed CRD install.
func TestWaitEstablished_SurvivesATransientGetError(t *testing.T) {
	t.Parallel()

	var calls int
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
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if calls++; calls == 1 {
					return errors.New("connection refused")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := waitEstablished(ctx, c, "platforms.platform.paas.io"); err != nil {
		t.Errorf("waitEstablished: %v — a transient error ended the wait", err)
	}
	if calls < 2 {
		t.Errorf("Get called %d times, want the failure to have been retried", calls)
	}
}

// Apply must surface a CRD that applies cleanly but never establishes, rather
// than returning success once every Patch has gone through.
func TestApply_FailsWhenACRDNeverEstablishes(t *testing.T) {
	t.Parallel()

	crds, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Seeded with an unrelated condition, so the wait sees a live object and
	// still finds no Established among its conditions.
	stuck := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: crds[0].Name},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
				Type:   apiextensionsv1.NamesAccepted,
				Status: apiextensionsv1.ConditionTrue,
			}},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(stuck).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return nil
			},
		}).Build()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if _, err := Apply(ctx, c); err == nil {
		t.Error("a CRD that never established was reported as installed")
	}
}

// A rest.Config the client cannot be built from at all, which is a different
// failure from an apiserver that is merely unreachable: nothing is dialled.
func TestInstall_ReportsAnUnusableConfig(t *testing.T) {
	t.Parallel()

	cfg := &rest.Config{
		Host:            "https://127.0.0.1:6443",
		TLSClientConfig: rest.TLSClientConfig{CAFile: "/nonexistent/ca.crt"},
	}

	_, err := Install(t.Context(), cfg, time.Second)
	if err == nil {
		t.Fatal("an unusable rest.Config was reported as a successful install")
	}
	if !strings.Contains(err.Error(), "build client") {
		t.Errorf("err = %q, want it to name the step that failed", err)
	}
}
