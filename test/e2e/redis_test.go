//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/pkg/wait"
)

var redisGVR = schema.GroupVersionResource{
	Group: "apps.paas.io", Version: "v1alpha1", Resource: "redises",
}

func redisFixture(ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps.paas.io/v1alpha1",
		"kind":       "Redis",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       spec,
	}}
}

// waitRedisReady waits for the Redis CR to report Ready with a status.ready
// that agrees with the StatefulSet's own status.readyReplicas, read live
// rather than assumed — both folded into one wait so nothing downstream
// asserts a stronger condition than this waited for. The equality is the
// point: a reconciler that hardcoded status.ready to "1" would satisfy a
// non-emptiness check but not this one.
func waitRedisReady(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	release := releaseNameFor(name, "Redis")
	var last string
	err := wait.For(ctx, 5*time.Second,
		fmt.Sprintf("redis %s ready with status.ready matching the statefulset", name),
		func(ctx context.Context) (bool, error) {
			got, err := dynClient.Resource(redisGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = err.Error()
				}
				return false, nil
			}
			status, _ := conditionStatus(got, "Ready")
			ready, _, _ := unstructured.NestedString(got.Object, "status", "ready")

			sts, err := clientset.AppsV1().StatefulSets(ns).Get(ctx, release, metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = fmt.Sprintf("redis ready=%s status.ready=%q; statefulset: %v", status, ready, err)
				}
				return false, nil
			}
			last = fmt.Sprintf("redis ready=%s status.ready=%q statefulset.readyReplicas=%d",
				status, ready, sts.Status.ReadyReplicas)
			return status == "True" && sts.Status.ReadyReplicas > 0 &&
				ready == strconv.Itoa(int(sts.Status.ReadyReplicas)), nil
		})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}
}

// valkeyExec runs valkey-cli inside the instance pod through `kubectl exec`,
// the same route storage_test.go uses for linstor — this suite has no
// in-cluster exec client, and adding one for a single call is more machinery
// than the assertion is worth.
func valkeyExec(t *testing.T, ns, pod string, args ...string) string {
	t.Helper()

	full := append([]string{"exec", "-n", ns, pod, "--", "valkey-cli"}, args...)
	out, err := exec.CommandContext(t.Context(), "kubectl", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("valkey-cli %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func redisPVCCount(ctx context.Context, t *testing.T, ns, release string) int {
	t.Helper()

	list, err := clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "paas.io/service-name=" + release,
	})
	if err != nil {
		t.Fatalf("list pvcs in %s: %v", ns, err)
	}
	return len(list.Items)
}

// The phase-3 done-when for redis: a tenant's Redis CR becomes a real,
// persistent Valkey instance, and the CR's own status reports the actual
// StatefulSet's readyReplicas rather than a value the reconciler could get
// away with faking.
func TestRedis_BecomesReadyAndReportsItsReplicaCount(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "redis", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "redis", "business", false)
	waitNamespace(t, "tenant-redis", 3*time.Minute)
	ns := "tenant-redis"
	const name = "cache"

	obj := redisFixture(ns, name, map[string]any{
		"storage":   map[string]any{"size": "1Gi", "class": "replicated-3"},
		"resources": map[string]any{"cpu": "100m", "memory": "256Mi"},
	})
	if _, err := dynClient.Resource(redisGVR).Namespace(ns).
		Create(t.Context(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create redis %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := dynClient.Resource(redisGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete redis %s/%s: %v", ns, name, err)
		}
	})

	// Generous on purpose: the PVC binds on a DRBD-replicated storage class
	// that has to place a copy on every node it spans before the pod the
	// StatefulSet owns can start, the same cost storage_test.go budgets for.
	waitRedisReady(t, ns, name, 10*time.Minute)
}

// The assertion postgres's suite never makes: a pod that is Running and a
// datastore that actually stores are different claims, and the second is the
// one a tenant is buying.
func TestRedis_StoresWhatWasWritten(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "redis", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "redisstore", "business", false)
	waitNamespace(t, "tenant-redisstore", 3*time.Minute)
	ns := "tenant-redisstore"
	const name = "cache"

	obj := redisFixture(ns, name, map[string]any{})
	if _, err := dynClient.Resource(redisGVR).Namespace(ns).
		Create(t.Context(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create redis %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := dynClient.Resource(redisGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete redis %s/%s: %v", ns, name, err)
		}
	})

	waitRedisReady(t, ns, name, 10*time.Minute)

	pod := releaseNameFor(name, "Redis") + "-0"
	const key, value = "e2e-key", "e2e-value"
	if got := valkeyExec(t, ns, pod, "SET", key, value); got != "OK" {
		t.Fatalf(`SET %s %s = %q, want "OK"`, key, value, got)
	}
	if got := valkeyExec(t, ns, pod, "GET", key); got != value {
		t.Errorf("GET %s = %q, want %q — the value written was not the value read back", key, got, value)
	}
}

// Per non-negotiable #6, each case asserts the specific denial: a generator
// built with x-kubernetes-preserve-unknown-fields would still error on
// nothing here and pass a looser test.
func TestRedis_OffSchemaFieldIsRejectedWithItsOwnMessage(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "redis", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "redisschema", "business", false)
	waitNamespace(t, "tenant-redisschema", 3*time.Minute)
	ns := "tenant-redisschema"

	t.Run("out of range storage size", func(t *testing.T) {
		const name = "too-big"
		obj := redisFixture(ns, name, map[string]any{"storage": map[string]any{"size": "10Gi"}})
		_, err := dynClient.Resource(redisGVR).Namespace(ns).Create(t.Context(), obj, metav1.CreateOptions{})
		if err == nil {
			_ = dynClient.Resource(redisGVR).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{})
			t.Fatal("storage.size: 10Gi was accepted; the schema's 5Gi cap is not being enforced")
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("err = %v, want a validation (Invalid) error", err)
		}
		const want = `spec.storage.size in body should match '^([1-9][0-9]{0,2}Mi|[1-5]Gi)$'`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	})

	t.Run("undeclared field rejected by strict decoding", func(t *testing.T) {
		const name = "evil-image"
		obj := redisFixture(ns, name, map[string]any{"image": "evil:latest"})
		_, err := dynClient.Resource(redisGVR).Namespace(ns).Create(t.Context(), obj,
			metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
		if err == nil {
			_ = dynClient.Resource(redisGVR).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{})
			t.Fatal("spec.image was accepted under strict field validation; the schema does not close over undeclared fields")
		}
		if !apierrors.IsBadRequest(err) {
			t.Fatalf("err = %v, want a 400 Bad Request", err)
		}
		const want = `unknown field "spec.image"`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	})
}

// The reclaim chain the design flags as genuinely uncertain rather than
// assumed: a StatefulSet's volumeClaimTemplate PVCs are not deleted by
// Kubernetes when the StatefulSet itself is deleted, so this proves the
// ownership chain through the HelmRelease reclaims them anyway.
func TestRedis_DeleteReclaimsEverything(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "redis", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "redisdelete", "business", false)
	waitNamespace(t, "tenant-redisdelete", 3*time.Minute)
	ns := "tenant-redisdelete"
	const name = "cache"

	obj := redisFixture(ns, name, map[string]any{})
	if _, err := dynClient.Resource(redisGVR).Namespace(ns).
		Create(t.Context(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create redis %s/%s: %v", ns, name, err)
	}

	waitRedisReady(t, ns, name, 10*time.Minute)

	release := releaseNameFor(name, "Redis")
	if n := redisPVCCount(t.Context(), t, ns, release); n == 0 {
		t.Fatal("no PVCs exist before deletion; the reclaim assertion below would prove nothing")
	}

	if err := dynClient.Resource(redisGVR).Namespace(ns).
		Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete redis %s/%s: %v", ns, name, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	if err := wait.For(ctx, 5*time.Second, "helmrelease "+release+" reclaimed",
		func(ctx context.Context) (bool, error) {
			_, err := dynClient.Resource(helmReleaseGVR).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		dumpNamespace(t, ns)
		t.Fatalf("%v", err)
	}

	if err := wait.For(ctx, 5*time.Second, "statefulset "+release+" reclaimed",
		func(ctx context.Context) (bool, error) {
			_, err := clientset.AppsV1().StatefulSets(ns).Get(ctx, release, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		dumpNamespace(t, ns)
		t.Fatalf("%v", err)
	}

	var lastPVCs int
	if err := wait.For(ctx, 5*time.Second, "pvcs for "+release+" reclaimed",
		func(ctx context.Context) (bool, error) {
			lastPVCs = redisPVCCount(ctx, t, ns, release)
			return lastPVCs == 0, nil
		}); err != nil {
		dumpNamespace(t, ns)
		t.Fatalf("%v (last: %d pvcs remain)", err, lastPVCs)
	}
}
