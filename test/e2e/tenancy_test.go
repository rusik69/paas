//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rusik69/paas/pkg/wait"
)

var tenantGVR = schema.GroupVersionResource{
	Group: "core.paas.io", Version: "v1alpha1", Resource: "tenants",
}

const rootNamespace = "tenant-root"

// applyTenant creates a Tenant, and removes it plus its namespace afterwards.
func applyTenant(t *testing.T, namespace, name, plan string, monitoring bool) {
	t.Helper()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.paas.io/v1alpha1",
		"kind":       "Tenant",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"plan": plan,
			"modules": map[string]any{
				"monitoring": map[string]any{"enabled": monitoring},
			},
		},
	}}

	if _, err := dynClient.Resource(tenantGVR).Namespace(namespace).
		Create(t.Context(), obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create tenant %s/%s: %v", namespace, name, err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := dynClient.Resource(tenantGVR).Namespace(namespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete tenant %s/%s: %v", namespace, name, err)
		}
		// The finalizer holds the tenant until its namespace is gone, so this
		// also waits out the cascade rather than racing the next test.
		_ = wait.For(ctx, 5*time.Second, "tenant "+name+" reclaimed",
			func(ctx context.Context) (bool, error) {
				_, err := dynClient.Resource(tenantGVR).Namespace(namespace).
					Get(ctx, name, metav1.GetOptions{})
				return apierrors.IsNotFound(err), nil
			})
	})
}

func waitNamespace(t *testing.T, name string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	err := wait.For(ctx, 3*time.Second, "namespace "+name, func(ctx context.Context) (bool, error) {
		_, err := clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		return err == nil, nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// probe opens one TCP connection and reports whether it was allowed.
//
// nc -z rather than an HTTP request, because what a network policy decides is
// whether the connection happens. An HTTP probe conflates the two: the first
// version of this test used wget and its positive control failed on a 404 from
// a connection the policy had allowed.
func probe(t *testing.T, namespace, name, host string, port int, labels map[string]string) bool {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "main",
				Image: *busyboxImage,
				// -w bounds a drop, which otherwise hangs until the pod
				// deadline and makes a denial indistinguishable from a slow
				// cluster.
				Command: []string{"nc", "-z", "-w", "10", host, fmt.Sprint(port)},
			}},
		},
	}
	if _, err := clientset.CoreV1().Pods(namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create probe %s/%s: %v", namespace, name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		grace := int64(0)
		if err := clientset.CoreV1().Pods(namespace).Delete(ctx, name,
			metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete probe %s: %v", name, err)
		}
	})

	phase, err := waitPodTerminated(t, namespace, name, 4*time.Minute)
	if err != nil {
		t.Fatalf("probe %s never terminated: %v", name, err)
	}
	return phase == corev1.PodSucceeded
}

// The phase-2 done-when: two nested tenants, the child inheriting the parent's
// monitoring, and negative network tests proving cross-tenant traffic and
// pod-to-apiserver both fail.
//
// One test, because the isolation assertions need the tree that the first half
// builds, and building it twice would double a slow test for no new information.
func TestTenancy_NestedTenantsAreIsolatedAndInheritModules(t *testing.T) {
	ensureRootNamespace(t)

	// The parent's namespace has to exist before a child can be created inside
	// it — the tree is expressed by containment, so the child object has
	// nowhere to live until the operator has reconciled the parent.
	applyTenant(t, rootNamespace, "acme", "business", true)
	waitNamespace(t, "tenant-acme", 3*time.Minute)

	applyTenant(t, "tenant-acme", "beta", "trial", false)
	waitNamespace(t, "tenant-acme-beta", 3*time.Minute)

	// Inheritance: the child declares monitoring off, which means "use my
	// parent's", and the platform records which one it chose.
	waitModuleResolved(t, "tenant-acme", "beta", "monitoring", "acme", 2*time.Minute)

	// A listener in the parent, so the cross-tenant probe has something real to
	// fail to reach.
	target := serveInNamespace(t, "tenant-acme", "listener")

	// Positive control first. If this fails the negative results below mean
	// nothing, because everything would be unreachable.
	if !probe(t, "tenant-acme-beta", "same-ns", sameNamespaceTarget(t, "tenant-acme-beta"), 8080, nil) {
		t.Fatal("a pod could not reach another pod in its own namespace; the negative results below prove nothing")
	}

	// Cross-tenant traffic must be denied, even parent to child.
	if probe(t, "tenant-acme-beta", "cross-tenant", target, 8080, nil) {
		t.Error("a pod reached another tenant's namespace")
	}

	// pod to apiserver must be denied without the opt-in.
	if probe(t, "tenant-acme-beta", "apiserver", "kubernetes.default.svc", 443, nil) {
		t.Error("a pod reached the API server without the opt-in label")
	}

	// And permitted with it, or the opt-in is decoration and the deny is
	// untestable in the other direction.
	if !probe(t, "tenant-acme-beta", "apiserver-optin", "kubernetes.default.svc", 443,
		map[string]string{"policy.paas.io/allow-to-apiserver": "true"}) {
		t.Error("the allow-to-apiserver label granted nothing")
	}
}

// The allowance defaultDenyPolicy grants paas-system: the platform's own
// operators (CNPG among them) provision workloads inside a tenant namespace
// and then have to poll them, which no per-pod label can express — the source
// is the operator's own namespace, not anything the tenant runs.
//
// Cross-tenant traffic is already asserted denied in
// TestTenancy_NestedTenantsAreIsolatedAndInheritModules; nothing here repeats
// it.
func TestTenancy_PlatformNamespaceReachesTenantWorkloads(t *testing.T) {
	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "svcreach", "trial", false)
	waitNamespace(t, "tenant-svcreach", 3*time.Minute)

	target := serveInNamespace(t, "tenant-svcreach", "listener")

	// Positive control: the listener answers something before concluding
	// paas-system's own reach to it is what the policy decides.
	if !probe(t, "tenant-svcreach", "same-ns", sameNamespaceTarget(t, "tenant-svcreach"), 8080, nil) {
		t.Fatal("a pod could not reach another pod in its own namespace; the platform-reach assertion below would prove nothing")
	}

	if !probe(t, platformNamespace, "from-platform", target, 8080, nil) {
		t.Error("a pod in paas-system could not reach a listener in a tenant namespace; " +
			"the operator-to-workload ingress the platform's managed services depend on is not allowed")
	}
}

// The other half of the same fix: the allowance is ingress-only, and nothing
// else proves the egress direction — a tenant pod dialling into paas-system —
// stayed shut.
func TestTenancy_TenantCannotReachPlatformNamespace(t *testing.T) {
	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "svcegress", "trial", false)
	waitNamespace(t, "tenant-svcegress", 3*time.Minute)

	// Positive control: this tenant's own connectivity works at all, so a
	// failure to reach paas-system below is the policy, not a broken pod or a
	// dead listener.
	if !probe(t, "tenant-svcegress", "same-ns", sameNamespaceTarget(t, "tenant-svcegress"), 8080, nil) {
		t.Fatal("a pod could not reach another pod in its own namespace; the egress denial below would prove nothing")
	}

	target := serveInNamespace(t, platformNamespace, "e2e-egress-target")

	if probe(t, "tenant-svcegress", "to-platform", target, 8080, nil) {
		t.Error("a tenant pod reached paas-system; the fix for operator ingress was meant to stay ingress-only")
	}
}

func ensureRootNamespace(t *testing.T) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rootNamespace}}
	if _, err := clientset.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s: %v", rootNamespace, err)
	}
}

func waitModuleResolved(t *testing.T, namespace, name, module, wantTenant string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var last string
	err := wait.For(ctx, 3*time.Second, fmt.Sprintf("module %s resolved to %s", module, wantTenant),
		func(ctx context.Context) (bool, error) {
			got, err := dynClient.Resource(tenantGVR).Namespace(namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = err.Error()
				}
				return false, nil
			}
			provider, _, _ := unstructured.NestedString(got.Object, "status", "modules", module, "tenant")
			inherited, _, _ := unstructured.NestedBool(got.Object, "status", "modules", module, "inherited")
			last = fmt.Sprintf("tenant=%q inherited=%t", provider, inherited)
			return provider == wantTenant && inherited, nil
		})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}
}

// serveInNamespace starts a listener and returns its pod IP.
func serveInNamespace(t *testing.T, namespace, name string) string {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   *busyboxImage,
				Command: []string{"sh", "-c", "echo ok >/tmp/index.html && httpd -f -p 8080 -h /tmp"},
			}},
		},
	}
	if _, err := clientset.CoreV1().Pods(namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("create listener %s/%s: %v", namespace, name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		grace := int64(0)
		if err := clientset.CoreV1().Pods(namespace).Delete(ctx, name,
			metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete listener %s: %v", name, err)
		}
	})

	if err := waitPodReady(t, namespace, name, 4*time.Minute); err != nil {
		t.Fatalf("listener %s/%s never became ready: %v", namespace, name, err)
	}
	return getPod(t, namespace, name).Status.PodIP
}

// sameNamespaceTarget starts a listener in the given namespace and returns its
// IP, for the positive control.
func sameNamespaceTarget(t *testing.T, namespace string) string {
	t.Helper()

	return serveInNamespace(t, namespace, "local-listener")
}

// The half envtest cannot reach: a real cluster runs the token controller, so
// this proves the generated kubeconfig is one somebody could actually use.
func TestTenancy_GeneratedKubeconfigWorks(t *testing.T) {
	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "ci", "trial", false)
	waitNamespace(t, "tenant-ci", 3*time.Minute)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	var kubeconfig []byte
	err := wait.For(ctx, 5*time.Second, "generated kubeconfig", func(ctx context.Context) (bool, error) {
		s, err := clientset.CoreV1().Secrets("tenant-ci").
			Get(ctx, "tenant-kubeconfig", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		kubeconfig = s.Data["kubeconfig"]
		return len(kubeconfig) > 0, nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	// Used, not merely parsed: a kubeconfig that does not authenticate is worse
	// than none, because it fails when someone depends on it.
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		t.Fatalf("the generated kubeconfig does not load: %v", err)
	}
	tenantClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build client from the generated kubeconfig: %v", err)
	}

	if _, err := tenantClient.CoreV1().ConfigMaps("tenant-ci").
		List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("the generated kubeconfig cannot read its own namespace: %v", err)
	}

	// And it must not reach outside the tenant, or the RBAC is decoration.
	_, err = tenantClient.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
	if err == nil {
		t.Fatal("the tenant kubeconfig read kube-system")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("err = %v, want a 403 — a different failure would pass even with RBAC removed", err)
	}
}
