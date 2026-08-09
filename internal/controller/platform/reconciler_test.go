package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/rusik69/paas/api/v1alpha1"
)

// Error propagation only; the rollout behaviour is asserted against envtest.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return s
}

type staticFetcher struct{ rel *Release }

func (f staticFetcher) Fetch(context.Context, string, string) (*Release, error) {
	return f.rel, nil
}

func oneEntryRelease() *Release {
	return &Release{
		Version: "v1", Digest: "sha256:v1",
		Packages: []Entry{{Name: "a", Chart: "a", Version: "1", Stage: v1alpha1.StageComponent}},
	}
}

func cluster() *v1alpha1.Platform {
	return &v1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       v1alpha1.PlatformSpec{Version: "v1", Registry: "oci://r/paas"},
	}
}

func request() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}
}

func TestReconcile_ReturnsAReadFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t), Fetcher: staticFetcher{oneEntryRelease()}}).
		Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
}

func TestReconcile_ApplyFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("apiserver said no")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cluster()).WithStatusSubresource(&v1alpha1.Platform{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t), Fetcher: staticFetcher{oneEntryRelease()}}).
		Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the client error", err)
	}
}

// The prune list is what makes a removal possible; failing it must not read as
// a completed rollout.
func TestReconcile_PruneListFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("list refused")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cluster()).WithStatusSubresource(&v1alpha1.Platform{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t), Fetcher: staticFetcher{oneEntryRelease()}}).
		Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the list failure", err)
	}
}

// A scheme that cannot express the owner reference must fail rather than emit
// orphans that nothing reclaims.
func TestReconcile_FailsWhenTheOwnerCannotBeSet(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cluster()).WithStatusSubresource(&v1alpha1.Platform{}).Build()

	_, err := (&Reconciler{Client: c, Scheme: runtime.NewScheme(), Fetcher: staticFetcher{oneEntryRelease()}}).
		Reconcile(t.Context(), request())
	if err == nil {
		t.Fatal("orphaned objects were rendered")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("err = %q, want it to name the owner reference", err)
	}
}

// When recording a failure also fails, the reported error must still name the
// original cause — otherwise an incident starts from the wrong symptom.
func TestReconcile_StatusWriteFailureDoesNotMaskTheCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("registry unreachable")
	statusBoom := errors.New("status write refused")

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cluster()).WithStatusSubresource(&v1alpha1.Platform{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return statusBoom
			},
		}).Build()

	_, err := (&Reconciler{
		Client: c, Scheme: testScheme(t),
		Fetcher: failingFetcher{cause},
	}).Reconcile(t.Context(), request())

	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to still wrap the original cause", err)
	}
	if !errors.Is(err, statusBoom) {
		t.Errorf("err = %v, want it to also mention the failed status write", err)
	}
}

type failingFetcher struct{ err error }

func (f failingFetcher) Fetch(context.Context, string, string) (*Release, error) {
	return nil, f.err
}

// The PackageSource applies cleanly and a Package does not, so the failure is
// reached after the source write rather than instead of it.
func TestReconcile_PackageApplyFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("package rejected")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cluster()).WithStatusSubresource(&v1alpha1.Platform{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*v1alpha1.Package); ok {
					return boom
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t), Fetcher: staticFetcher{oneEntryRelease()}}).
		Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the package apply failure", err)
	}
	if !strings.Contains(err.Error(), "package a") {
		t.Errorf("err = %q, want it to name the package", err)
	}
}

// A prune that cannot delete must not report the rollout complete: the removed
// component would still be running with nothing saying so.
func TestReconcile_PruneDeleteFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("delete refused")
	stale := &v1alpha1.Package{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "dropped",
			Labels: map[string]string{"platform.paas.io/platform": "cluster"},
		},
		Spec: v1alpha1.PackageSpec{
			SourceRef: v1alpha1.LocalRef{Name: "platform"},
			Chart:     "dropped", Version: "1", Stage: v1alpha1.StageComponent,
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cluster(), stale).WithStatusSubresource(&v1alpha1.Platform{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t), Fetcher: staticFetcher{oneEntryRelease()}}).
		Reconcile(t.Context(), request())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the delete failure", err)
	}
	if !strings.Contains(err.Error(), "dropped") {
		t.Errorf("err = %q, want it to name the package it could not remove", err)
	}
}
