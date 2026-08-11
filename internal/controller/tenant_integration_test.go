//go:build integration

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
	"github.com/rusik69/paas/internal/controller/service"
	tenantctl "github.com/rusik69/paas/internal/controller/tenant"
	"github.com/rusik69/paas/pkg/tenancy"
)

func tenantReconciler() *tenantctl.Reconciler {
	return &tenantctl.Reconciler{Client: k8sClient, Scheme: scheme}
}

// reconcileIn drives the reconciler for a namespaced object.
func reconcileIn(t *testing.T, ns, name string, r reconciler) {
	t.Helper()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
}

func ensureNamespace(t *testing.T, name string) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// newTenant creates a Tenant and removes it, plus its namespace, afterwards.
func newTenant(t *testing.T, ns, name string, plan corev1alpha1.Plan) *corev1alpha1.Tenant {
	t.Helper()

	ensureNamespace(t, ns)
	obj := &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1alpha1.TenantSpec{Plan: plan},
	}
	if err := k8sClient.Create(t.Context(), obj); err != nil {
		t.Fatalf("create tenant %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() { cleanupTenant(t, ns, name) })
	return obj
}

// cleanupTenant drops the finalizer before deleting, so a test that never
// reconciled a deletion cannot wedge the suite on a namespace nothing will
// reclaim.
func cleanupTenant(t *testing.T, ns, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var obj corev1alpha1.Tenant
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &obj); err != nil {
		return
	}
	if len(obj.Finalizers) > 0 {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.Finalizers = nil
		if err := k8sClient.Patch(ctx, &obj, patch); err != nil {
			t.Logf("cleanup: drop finalizers on %s/%s: %v", ns, name, err)
		}
	}
	if err := k8sClient.Delete(ctx, &obj); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("cleanup: delete tenant %s/%s: %v", ns, name, err)
	}
}

func tenantCondition(t *testing.T, ns, name, condType string) *metav1.Condition {
	t.Helper()

	var got corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	return apimeta.FindStatusCondition(got.Status.Conditions, condType)
}

func TestTenant_RootProducesItsNamespaceAndQuota(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "acme", corev1alpha1.PlanBusiness)
	reconcileIn(t, tenancy.RootNamespace, "acme", tenantReconciler())

	var ns corev1.Namespace
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "tenant-acme"}, &ns); err != nil {
		t.Fatalf("get namespace tenant-acme: %v", err)
	}
	if ns.Labels[tenantctl.TenantLabel] != "acme" {
		t.Errorf("namespace label = %q, want acme", ns.Labels[tenantctl.TenantLabel])
	}

	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-acme", Name: "tenant"}
	if err := k8sClient.Get(t.Context(), key, &quota); err != nil {
		t.Fatalf("get resourcequota: %v", err)
	}
	if got := quota.Spec.Hard[corev1.ResourceRequestsCPU]; got.String() != "8" {
		t.Errorf("quota requests.cpu = %s, want 8 for the business plan", got.String())
	}
	if got := quota.Spec.Hard[corev1.ResourceLimitsCPU]; got.String() != "8" {
		t.Errorf("quota limits.cpu = %s, want 8 — bounding requests alone lets a tenant burst past its plan", got.String())
	}

	var limits corev1.LimitRange
	if err := k8sClient.Get(t.Context(), key, &limits); err != nil {
		t.Fatalf("get limitrange: %v", err)
	}
	if len(limits.Spec.Limits) == 0 || limits.Spec.Limits[0].DefaultRequest.Cpu().IsZero() {
		t.Error("limitrange sets no default cpu request; pods with no request would be rejected by the quota")
	}

	if c := tenantCondition(t, tenancy.RootNamespace, "acme", tenantctl.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True", c)
	}
}

// The tree is expressed by containment, so a child's namespace name comes from
// where it lives rather than from a field it could contradict.
func TestTenant_ChildNamespaceIsPathDerived(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "acme", corev1alpha1.PlanBusiness)
	reconcileIn(t, tenancy.RootNamespace, "acme", tenantReconciler())

	newTenant(t, "tenant-acme", "beta", corev1alpha1.PlanTrial)
	reconcileIn(t, "tenant-acme", "beta", tenantReconciler())

	var ns corev1.Namespace
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "tenant-acme-beta"}, &ns); err != nil {
		t.Fatalf("get namespace tenant-acme-beta: %v", err)
	}

	// Isolation does not inherit: the child gets its own quota, at its own plan.
	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-acme-beta", Name: "tenant"}
	if err := k8sClient.Get(t.Context(), key, &quota); err != nil {
		t.Fatalf("get child resourcequota: %v", err)
	}
	if got := quota.Spec.Hard[corev1.ResourceRequestsCPU]; got.String() != "2" {
		t.Errorf("child quota requests.cpu = %s, want 2 for the trial plan", got.String())
	}
}

// A truncated name would let two tenants collide on one namespace, so the
// reconciler must refuse rather than create anything.
func TestTenant_OverlongNameIsRejectedWithoutCreatingANamespace(t *testing.T) {
	long := strings.Repeat("a", 60)
	newTenant(t, tenancy.RootNamespace, long, corev1alpha1.PlanTrial)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: tenancy.RootNamespace, Name: long}}
	if _, err := tenantReconciler().Reconcile(t.Context(), req); err == nil {
		t.Fatal("an overlong tenant name was accepted")
	}

	c := tenantCondition(t, tenancy.RootNamespace, long, tenantctl.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", c)
	}
	if !strings.Contains(c.Message, "63") {
		t.Errorf("Degraded message = %q, want it to name the limit", c.Message)
	}

	var ns corev1.Namespace
	err := k8sClient.Get(t.Context(), types.NamespacedName{Name: tenancy.Prefix + long}, &ns)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a namespace was created for an overlong name (err %v)", err)
	}
}

func TestTenant_PlanChangeUpdatesTheQuota(t *testing.T) {
	obj := newTenant(t, tenancy.RootNamespace, "grow", corev1alpha1.PlanTrial)
	reconcileIn(t, tenancy.RootNamespace, "grow", tenantReconciler())

	var fresh corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(obj), &fresh); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	fresh.Spec.Plan = corev1alpha1.PlanEnterprise
	if err := k8sClient.Update(t.Context(), &fresh); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	reconcileIn(t, tenancy.RootNamespace, "grow", tenantReconciler())

	var quota corev1.ResourceQuota
	key := types.NamespacedName{Namespace: "tenant-grow", Name: "tenant"}
	if err := k8sClient.Get(t.Context(), key, &quota); err != nil {
		t.Fatalf("get resourcequota: %v", err)
	}
	if got := quota.Spec.Hard[corev1.ResourceRequestsCPU]; got.String() != "32" {
		t.Errorf("quota requests.cpu = %s, want 32 after moving to enterprise", got.String())
	}
}

// ADR 0004 requires deletion to reclaim descendants, and the namespace going is
// what does it. envtest runs no namespace controller, so the namespace stays
// Terminating forever here — which makes this the right place to assert that the
// tenant is *held open*, and the release path a unit test with a fake client.
func TestTenant_DeletionAsksTheNamespaceToGoAndHoldsTheTenant(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "leaving", corev1alpha1.PlanTrial)
	reconcileIn(t, tenancy.RootNamespace, "leaving", tenantReconciler())

	key := types.NamespacedName{Namespace: tenancy.RootNamespace, Name: "leaving"}
	var withFinalizer corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), key, &withFinalizer); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if len(withFinalizer.Finalizers) == 0 {
		t.Fatal("no finalizer was added; deletion would orphan the namespace")
	}

	if err := k8sClient.Delete(t.Context(), &withFinalizer); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	req := ctrl.Request{NamespacedName: key}
	if _, err := tenantReconciler().Reconcile(t.Context(), req); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	var ns corev1.Namespace
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: "tenant-leaving"}, &ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if ns.DeletionTimestamp.IsZero() {
		t.Error("namespace was not asked to delete")
	}

	var stillHere corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), key, &stillHere); err != nil {
		t.Errorf("tenant was released before its namespace was gone: %v", err)
	}
}

func TestTenant_MissingObjectIsNotAnError(t *testing.T) {
	reconcileIn(t, tenancy.RootNamespace, "never-existed", tenantReconciler())
}

// policy reads one of the tenant's CiliumNetworkPolicies.
func policy(t *testing.T, namespace, name string) *unstructured.Unstructured {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy",
	})
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := k8sClient.Get(t.Context(), key, obj); err != nil {
		t.Fatalf("get ciliumnetworkpolicy %s/%s: %v", namespace, name, err)
	}
	return obj
}

// Every namespace gets its own policies at every depth. ADR 0004: isolation
// does not inherit.
func TestTenant_EveryDepthGetsItsOwnPolicies(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "walled", corev1alpha1.PlanBusiness)
	reconcileIn(t, tenancy.RootNamespace, "walled", tenantReconciler())

	newTenant(t, "tenant-walled", "inner", corev1alpha1.PlanTrial)
	reconcileIn(t, "tenant-walled", "inner", tenantReconciler())

	for _, ns := range []string{"tenant-walled", "tenant-walled-inner"} {
		deny := policy(t, ns, tenantctl.PolicyDefaultDeny)

		// An empty endpointSelector is what makes the policy select every pod,
		// and therefore what makes Cilium deny by default in this namespace.
		selector, found, err := unstructured.NestedMap(deny.Object, "spec", "endpointSelector")
		if err != nil || !found || len(selector) != 0 {
			t.Errorf("%s: endpointSelector = %v (found %t, err %v), want empty so it selects every pod",
				ns, selector, found, err)
		}

		// Egress must be an allow-list, not an empty rule that permits
		// everything.
		egress, found, err := unstructured.NestedSlice(deny.Object, "spec", "egress")
		if err != nil || !found || len(egress) == 0 {
			t.Errorf("%s: egress = %v, want an allow-list", egress, err)
		}

		// Nothing may name the API server in the deny policy; that is what the
		// opt-in exists for.
		if strings.Contains(fmt.Sprint(deny.Object), "kube-apiserver") {
			t.Errorf("%s: the default-deny policy grants API server access", ns)
		}

		opt := policy(t, ns, tenantctl.PolicyAllowAPIServer)
		labels, _, _ := unstructured.NestedStringMap(opt.Object, "spec", "endpointSelector", "matchLabels")
		if labels[tenantctl.AllowAPIServerLabel] != "true" {
			t.Errorf("%s: opt-in selects %v, want %s=true",
				ns, labels, tenantctl.AllowAPIServerLabel)
		}
		if !strings.Contains(fmt.Sprint(opt.Object), "kube-apiserver") {
			t.Errorf("%s: the opt-in policy grants nothing", ns)
		}

		// The platform's reach into the namespace is its own object at every
		// depth too, and it selects the managed instances rather than the
		// namespace.
		fromPlatform := policy(t, ns, tenantctl.PolicyAllowPlatform)
		expressions, found, err := unstructured.NestedSlice(fromPlatform.Object,
			"spec", "endpointSelector", "matchExpressions")
		if err != nil || !found || len(expressions) != 1 {
			t.Errorf("%s: matchExpressions = %v (found %t, err %v), want one selecting the chart-contract label",
				ns, expressions, found, err)
		}
		if !strings.Contains(fmt.Sprint(fromPlatform.Object), service.LabelServiceName) {
			t.Errorf("%s: the platform allowance does not select %s, so it reaches every pod or none",
				ns, service.LabelServiceName)
		}
		if _, found, _ := unstructured.NestedSlice(fromPlatform.Object, "spec", "egress"); found {
			t.Errorf("%s: the platform allowance carries egress; it was meant to stay ingress-only", ns)
		}
	}
}

// The point of the hierarchy, made observable: a child with monitoring off
// reports its parent as the provider.
func TestTenant_StatusReportsWhereModulesResolved(t *testing.T) {
	parent := newTenant(t, tenancy.RootNamespace, "org", corev1alpha1.PlanBusiness)
	parent.Spec.Modules = map[string]corev1alpha1.Module{"monitoring": {Enabled: true}}
	if err := k8sClient.Update(t.Context(), parent); err != nil {
		t.Fatalf("enable monitoring on the parent: %v", err)
	}
	reconcileIn(t, tenancy.RootNamespace, "org", tenantReconciler())

	child := newTenant(t, "tenant-org", "team", corev1alpha1.PlanTrial)
	child.Spec.Modules = map[string]corev1alpha1.Module{"monitoring": {Enabled: false}}
	if err := k8sClient.Update(t.Context(), child); err != nil {
		t.Fatalf("disable monitoring on the child: %v", err)
	}
	reconcileIn(t, "tenant-org", "team", tenantReconciler())

	var got corev1alpha1.Tenant
	key := types.NamespacedName{Namespace: "tenant-org", Name: "team"}
	if err := k8sClient.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get child tenant: %v", err)
	}

	resolved, ok := got.Status.Modules["monitoring"]
	if !ok {
		t.Fatalf("status.modules = %v, want monitoring resolved", got.Status.Modules)
	}
	if resolved.Tenant != "org" {
		t.Errorf("monitoring resolved to %q, want org — false on a child means use the parent's", resolved.Tenant)
	}
	if resolved.Namespace != "tenant-org" {
		t.Errorf("monitoring namespace = %q, want tenant-org", resolved.Namespace)
	}
	if !resolved.Inherited {
		t.Error("monitoring is not marked inherited, but the child does not provide it")
	}

	// The parent provides its own, and must not be marked inherited.
	var parentGot corev1alpha1.Tenant
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(parent), &parentGot); err != nil {
		t.Fatalf("get parent tenant: %v", err)
	}
	if own := parentGot.Status.Modules["monitoring"]; own.Tenant != "org" || own.Inherited {
		t.Errorf("parent monitoring = %+v, want org and not inherited", own)
	}
}

// Access objects, against a real API server: RBAC has admission-time validation
// a fake client does not run, so a malformed Role or a binding naming a
// nonexistent kind fails here rather than on a cluster.
func TestTenant_ProvisionsRolesBindingsAndCIAccount(t *testing.T) {
	obj := newTenant(t, tenancy.RootNamespace, "access", corev1alpha1.PlanBusiness)
	obj.Spec.Admins = []string{"alice@acme.com"}
	if err := k8sClient.Update(t.Context(), obj); err != nil {
		t.Fatalf("set admins: %v", err)
	}
	reconcileIn(t, tenancy.RootNamespace, "access", tenantReconciler())

	for _, name := range []string{tenantctl.RoleAdmin, tenantctl.RoleViewer} {
		var role rbacv1.Role
		key := types.NamespacedName{Namespace: "tenant-access", Name: name}
		if err := k8sClient.Get(t.Context(), key, &role); err != nil {
			t.Fatalf("get role %s: %v", name, err)
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				if group == "rbac.authorization.k8s.io" || group == "*" {
					t.Errorf("role %s grants %q, letting a tenant escalate itself", name, group)
				}
			}
		}

		var rb rbacv1.RoleBinding
		if err := k8sClient.Get(t.Context(), key, &rb); err != nil {
			t.Fatalf("get rolebinding %s: %v", name, err)
		}
		var users, groups []string
		for _, s := range rb.Subjects {
			switch s.Kind {
			case rbacv1.UserKind:
				users = append(users, s.Name)
			case rbacv1.GroupKind:
				groups = append(groups, s.Name)
			}
		}
		if len(users) == 0 || users[0] != "alice@acme.com" {
			t.Errorf("%s binds users %v, want the tenant's admin", name, users)
		}
		if len(groups) == 0 || groups[0] != tenantctl.GroupPrefix+"access" {
			t.Errorf("%s binds groups %v, want the tenant's OIDC group", name, groups)
		}
	}

	var sa corev1.ServiceAccount
	key := types.NamespacedName{Namespace: "tenant-access", Name: tenantctl.ServiceAccountCI}
	if err := k8sClient.Get(t.Context(), key, &sa); err != nil {
		t.Errorf("get CI service account: %v", err)
	}
}

// Administration flows down and not up — ADR 0004. A child's binding must name
// the parent's admin; the parent's must not name the child's.
func TestTenant_ParentAdminsAdministerDescendants(t *testing.T) {
	parent := newTenant(t, tenancy.RootNamespace, "chain", corev1alpha1.PlanBusiness)
	parent.Spec.Admins = []string{"parent@acme.com"}
	if err := k8sClient.Update(t.Context(), parent); err != nil {
		t.Fatalf("set parent admins: %v", err)
	}
	reconcileIn(t, tenancy.RootNamespace, "chain", tenantReconciler())

	child := newTenant(t, "tenant-chain", "link", corev1alpha1.PlanTrial)
	child.Spec.Admins = []string{"child@acme.com"}
	if err := k8sClient.Update(t.Context(), child); err != nil {
		t.Fatalf("set child admins: %v", err)
	}
	reconcileIn(t, "tenant-chain", "link", tenantReconciler())

	subjects := func(namespace string) []string {
		var rb rbacv1.RoleBinding
		key := types.NamespacedName{Namespace: namespace, Name: tenantctl.RoleAdmin}
		if err := k8sClient.Get(t.Context(), key, &rb); err != nil {
			t.Fatalf("get rolebinding in %s: %v", namespace, err)
		}
		var names []string
		for _, s := range rb.Subjects {
			if s.Kind == rbacv1.UserKind {
				names = append(names, s.Name)
			}
		}
		return names
	}

	childAdmins := subjects("tenant-chain-link")
	if !slices.Contains(childAdmins, "parent@acme.com") {
		t.Errorf("child admins = %v, want the parent's admin among them", childAdmins)
	}
	if !slices.Contains(childAdmins, "child@acme.com") {
		t.Errorf("child admins = %v, want its own admin", childAdmins)
	}

	parentAdmins := subjects("tenant-chain")
	if slices.Contains(parentAdmins, "child@acme.com") {
		t.Errorf("parent admins = %v; administration must not flow up", parentAdmins)
	}
}

// envtest runs an apiserver and etcd and nothing else — no token controller —
// so the token is supplied here rather than minted. What that leaves testable is
// exactly our half: a token not yet present is not a failure, and once one
// exists the kubeconfig is written and pinned. Real minting is asserted in
// test/e2e, on a cluster that has the controller.
func TestTenant_KubeconfigIsWrittenOnceATokenExists(t *testing.T) {
	newTenant(t, tenancy.RootNamespace, "ci", corev1alpha1.PlanTrial)

	r := &tenantctl.Reconciler{
		Client: k8sClient, Scheme: scheme,
		Endpoint: tenantctl.APIEndpoint{URL: "https://api.example:6443", CA: []byte("ca")},
	}
	reconcileIn(t, tenancy.RootNamespace, "ci", r)

	if c := tenantCondition(t, tenancy.RootNamespace, "ci", tenantctl.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True — a token not yet minted is not a failure", c)
	}
	var absent corev1.Secret
	key := types.NamespacedName{Namespace: "tenant-ci", Name: tenantctl.KubeconfigSecret}
	if err := k8sClient.Get(t.Context(), key, &absent); err == nil {
		t.Error("a kubeconfig was written with no token; it would authenticate as nobody")
	}

	// Stand in for the token controller.
	var tokenSecret corev1.Secret
	tokenKey := types.NamespacedName{Namespace: "tenant-ci", Name: tenantctl.ServiceAccountCI}
	if err := k8sClient.Get(t.Context(), tokenKey, &tokenSecret); err != nil {
		t.Fatalf("get token secret: %v", err)
	}
	tokenSecret.Data = map[string][]byte{"token": []byte("minted-token")}
	if err := k8sClient.Update(t.Context(), &tokenSecret); err != nil {
		t.Fatalf("supply a token: %v", err)
	}

	reconcileIn(t, tenancy.RootNamespace, "ci", r)

	var got corev1.Secret
	if err := k8sClient.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get kubeconfig secret: %v", err)
	}
	kubeconfig := string(got.Data["kubeconfig"])
	if !strings.Contains(kubeconfig, "namespace: tenant-ci") {
		t.Error("the kubeconfig is not pinned to the tenant's namespace")
	}
	if !strings.Contains(kubeconfig, "minted-token") {
		t.Error("the kubeconfig does not carry the service account token")
	}
}
