package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
	"github.com/rusik69/paas/pkg/tenancy"
)

// Error propagation and the branches the CRD's own validation makes
// unreachable through a real API server. Everything about applied objects is
// asserted against envtest.
func live(name string, plan corev1alpha1.Plan) *corev1alpha1.Tenant {
	return &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  tenancy.RootNamespace,
			Name:       name,
			Finalizers: []string{Finalizer},
		},
		Spec: corev1alpha1.TenantSpec{Plan: plan},
	}
}

func builder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()

	return fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).WithStatusSubresource(&corev1alpha1.Tenant{})
}

func degradedMessage(t *testing.T, c client.Client, name string) string {
	t.Helper()

	var got corev1alpha1.Tenant
	key := types.NamespacedName{Namespace: tenancy.RootNamespace, Name: name}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
	if cond == nil {
		t.Fatal("no Degraded condition")
	}
	return cond.Message
}

// The enum makes this unreachable through the API, but a reconciler that
// indexed the table blindly would apply a zero quota — which forbids every pod
// — if the enum and the table ever disagreed.
func TestReconcile_UnknownPlanIsDegradedNotAZeroQuota(t *testing.T) {
	t.Parallel()

	c := builder(t, live("gold", corev1alpha1.Plan("gold"))).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("gold"))
	if err == nil {
		t.Fatal("an unknown plan was accepted")
	}
	if msg := degradedMessage(t, c, "gold"); !strings.Contains(msg, "gold") {
		t.Errorf("Degraded message = %q, want it to name the plan", msg)
	}

	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-gold", Name: "tenant"}
	if err := c.Get(t.Context(), key, &quota); err == nil {
		t.Error("a quota was applied for a plan with no limits")
	}
}

func TestReconcile_ApplyFailureIsDegraded(t *testing.T) {
	t.Parallel()

	boom := errors.New("apiserver said no")
	c := builder(t, live("acme", corev1alpha1.PlanBusiness)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption,
			) error {
				if _, ok := obj.(*corev1alpha1.Tenant); ok {
					return c.Patch(ctx, obj, patch, opts...)
				}
				return boom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("acme"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the apply failure", err)
	}
	if msg := degradedMessage(t, c, "acme"); !strings.Contains(msg, "tenant-acme") {
		t.Errorf("Degraded message = %q, want it to name what failed", msg)
	}
}

func TestReconcile_ReturnsAReadFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := builder(t).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("acme")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the read failure", err)
	}
}

func TestReconcile_FinalizerPatchFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("patch refused")
	obj := live("acme", corev1alpha1.PlanBusiness)
	obj.Finalizers = nil

	c := builder(t, obj).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		},
	}).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("acme")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the finalizer patch failure", err)
	}
}

// A status write that fails must not hide why the reconcile failed.
func TestReconcile_StatusWriteFailureDoesNotMaskTheCause(t *testing.T) {
	t.Parallel()

	statusBoom := errors.New("status write refused")
	c := builder(t, live("gold", corev1alpha1.Plan("gold"))).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return statusBoom
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("gold"))
	if err == nil {
		t.Fatal("a failing reconcile reported success")
	}
	if !strings.Contains(err.Error(), "gold") {
		t.Errorf("err = %q, want it to still name the original cause", err)
	}
	if !errors.Is(err, statusBoom) {
		t.Errorf("err = %v, want it to also mention the failed status write", err)
	}
}

func TestReconcile_ReadyStatusWriteFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("status write refused")
	c := builder(t, live("acme", corev1alpha1.PlanBusiness)).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return boom
			},
		}).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("acme")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the status write failure", err)
	}
}

func TestReconcile_NamespaceReadFailureDuringDeletionIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := builder(t, deleting("leaving")).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("leaving")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the namespace read failure", err)
	}
}

// A namespace that cannot be deleted must keep the tenant open rather than
// release it and orphan the namespace.
func TestReconcile_NamespaceDeleteFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("delete refused")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "tenant-leaving",
		Labels: map[string]string{TenantLabel: "leaving"},
	}}

	c := builder(t, deleting("leaving"), ns).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return boom
		},
	}).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("leaving")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the delete failure", err)
	}
}

// Reconciling a deletion twice must not fail the second time.
func TestRemoveFinalizer_IsIdempotent(t *testing.T) {
	t.Parallel()

	// Not seeded into the client: removeFinalizer short-circuits before any
	// call when the finalizer is absent, and the fake client refuses to hold an
	// object that is deleting with no finalizers at all.
	obj := deleting("leaving")
	obj.Finalizers = nil

	r := &Reconciler{Client: builder(t).Build(), Scheme: testScheme(t)}
	if err := r.removeFinalizer(t.Context(), obj); err != nil {
		t.Errorf("removeFinalizer with none present: %v", err)
	}
}

func TestAddFinalizer_IsIdempotent(t *testing.T) {
	t.Parallel()

	obj := live("acme", corev1alpha1.PlanBusiness)
	c := builder(t, obj).Build()

	r := &Reconciler{Client: c, Scheme: testScheme(t)}
	if err := r.addFinalizer(t.Context(), obj); err != nil {
		t.Errorf("addFinalizer with one already present: %v", err)
	}
}

func TestRemoveFinalizer_PatchFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("patch refused")
	obj := deleting("leaving")
	c := builder(t, obj).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		},
	}).Build()

	r := &Reconciler{Client: c, Scheme: testScheme(t)}
	if err := r.removeFinalizer(t.Context(), obj.DeepCopy()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the patch failure", err)
	}
}

// A tenant whose parent is missing cannot have its ancestors walked, and both
// the things that walk them — inherited admins and module resolution — are
// answers the platform would otherwise have to invent. Reporting Ready would
// claim an access list and a module owner that were never resolved.
func TestReconcile_MissingAncestorIsDegraded(t *testing.T) {
	t.Parallel()

	// A child in tenant-acme whose parent object does not exist.
	orphan := &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "tenant-acme",
			Name:       "beta",
			Finalizers: []string{Finalizer},
		},
		Spec: corev1alpha1.TenantSpec{
			Plan:    corev1alpha1.PlanTrial,
			Modules: map[string]corev1alpha1.Module{"monitoring": {Enabled: false}},
		},
	}
	c := builder(t, orphan).Build()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-acme", Name: "beta"}}
	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), req); err == nil {
		t.Fatal("an unresolvable module reported success")
	}

	var got corev1alpha1.Tenant
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(orphan), &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", cond)
	}
	if !strings.Contains(cond.Message, "acme") {
		t.Errorf("Degraded message = %q, want it to name the ancestor it could not find", cond.Message)
	}
}

// A module no ancestor enables is simply absent, not an error: reporting it as
// one would make every tenant without monitoring permanently Degraded.
func TestReconcile_ModuleNoAncestorProvidesIsOmitted(t *testing.T) {
	t.Parallel()

	obj := live("solo", corev1alpha1.PlanTrial)
	obj.Spec.Modules = map[string]corev1alpha1.Module{"monitoring": {Enabled: false}}
	c := builder(t, obj).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("solo")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got corev1alpha1.Tenant
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if _, ok := got.Status.Modules["monitoring"]; ok {
		t.Errorf("status.modules = %v, want monitoring omitted when nothing provides it", got.Status.Modules)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True — an absent module is not a failure", cond)
	}
}

// The access objects a tenant depends on. Failing to write them must not leave
// the tenant reported Ready with nobody able to reach it.
func TestReconcile_AccessApplyFailureIsDegraded(t *testing.T) {
	t.Parallel()

	boom := errors.New("rbac rejected")
	c := builder(t, live("acme", corev1alpha1.PlanBusiness)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption,
			) error {
				switch obj.(type) {
				case *rbacv1.Role, *rbacv1.RoleBinding, *corev1.ServiceAccount:
					return boom
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	_, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("acme"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the access failure", err)
	}
	if msg := degradedMessage(t, c, "acme"); !strings.Contains(msg, "tenant-") {
		t.Errorf("Degraded message = %q, want it to name what failed", msg)
	}
}

// A token that cannot be read is not the same as one not yet minted: the first
// hides a real failure behind a state that looks like waiting.
func TestReconcile_TokenReadFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("etcd is unavailable")
	c := builder(t, live("acme", corev1alpha1.PlanBusiness)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if s, ok := obj.(*corev1.Secret); ok && key.Name == ServiceAccountCI {
					_ = s
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	r := &Reconciler{
		Client: c, Scheme: testScheme(t),
		Endpoint: APIEndpoint{URL: "https://api", CA: []byte("ca")},
	}
	if _, err := r.Reconcile(t.Context(), request("acme")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the token read failure", err)
	}
}

// With a token present the kubeconfig is written; with the write refused the
// tenant must not be Ready.
func TestReconcile_KubeconfigWriteFailureIsDegraded(t *testing.T) {
	t.Parallel()

	boom := errors.New("secret rejected")
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: ServiceAccountCI},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	c := builder(t, live("acme", corev1alpha1.PlanBusiness), token).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption,
			) error {
				if s, ok := obj.(*corev1.Secret); ok && s.Name == KubeconfigSecret {
					return boom
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	r := &Reconciler{
		Client: c, Scheme: testScheme(t),
		Endpoint: APIEndpoint{URL: "https://api", CA: []byte("ca")},
	}
	if _, err := r.Reconcile(t.Context(), request("acme")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the kubeconfig write failure", err)
	}
}

// No endpoint means no kubeconfig, rather than one pointing nowhere.
func TestReconcile_NoEndpointWritesNoKubeconfig(t *testing.T) {
	t.Parallel()

	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: ServiceAccountCI},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	c := builder(t, live("acme", corev1alpha1.PlanBusiness), token).Build()

	if _, err := (&Reconciler{Client: c, Scheme: testScheme(t)}).Reconcile(t.Context(), request("acme")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var s corev1.Secret
	key := types.NamespacedName{Namespace: "tenant-acme", Name: KubeconfigSecret}
	if err := c.Get(t.Context(), key, &s); err == nil {
		t.Error("a kubeconfig was written with no endpoint configured")
	}
}
