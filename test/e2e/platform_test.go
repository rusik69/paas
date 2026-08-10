//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/pkg/wait"
)

// The registry as the cluster sees it. Platform.spec.registry is one value used
// by two readers — the operator's fetcher and Flux — and both are in-cluster.
const platformRegistry = "oci://registry.paas-system.svc.cluster.local:5000/paas"

// Where platform components are installed. Not flux-system: Flux's own
// NetworkPolicy there permits ingress on port 8080 alone.
const platformNamespace = "paas-system"

var (
	platformGVR = schema.GroupVersionResource{
		Group: "platform.paas.io", Version: "v1alpha1", Resource: "platforms",
	}
	packageGVR = schema.GroupVersionResource{
		Group: "platform.paas.io", Version: "v1alpha1", Resource: "packages",
	}
	helmReleaseGVR = schema.GroupVersionResource{
		Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases",
	}
)

// setPlatformVersion creates or updates the singleton Platform.
func setPlatformVersion(t *testing.T, version string) {
	t.Helper()

	got, err := dynClient.Resource(platformGVR).Get(t.Context(), "cluster", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj := map[string]any{
			"apiVersion": "platform.paas.io/v1alpha1",
			"kind":       "Platform",
			"metadata":   map[string]any{"name": "cluster"},
			"spec":       map[string]any{"version": version, "registry": platformRegistry},
		}
		if _, err := dynClient.Resource(platformGVR).Create(t.Context(),
			&unstructured.Unstructured{Object: obj}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create platform at %s: %v", version, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("get platform: %v", err)
	}

	if err := unstructured.SetNestedField(got.Object, version, "spec", "version"); err != nil {
		t.Fatalf("set version: %v", err)
	}
	if _, err := dynClient.Resource(platformGVR).Update(t.Context(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update platform to %s: %v", version, err)
	}
}

// waitHelloMessage waits for the ConfigMap the hello chart renders to carry the
// message a release declares. It is the end of the whole chain: artifact →
// Package → HelmRelease → Helm → object in the cluster.
func waitHelloMessage(t *testing.T, want string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var last string
	err := wait.For(ctx, 5*time.Second, fmt.Sprintf("configmap hello message %q", want),
		func(ctx context.Context) (bool, error) {
			cm, err := clientset.CoreV1().ConfigMaps(platformNamespace).
				Get(ctx, "hello", metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = err.Error()
				}
				return false, nil
			}
			last = fmt.Sprintf("message=%q", cm.Data["message"])
			return cm.Data["message"] == want, nil
		})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}
}

func waitPackageAbsent(t *testing.T, name string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	err := wait.For(ctx, 5*time.Second, "package "+name+" removed",
		func(ctx context.Context) (bool, error) {
			_, err := dynClient.Resource(packageGVR).Get(ctx, name, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

func waitPackagePresent(t *testing.T, name string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	err := wait.For(ctx, 5*time.Second, "package "+name+" present",
		func(ctx context.Context) (bool, error) {
			_, err := dynClient.Resource(packageGVR).Get(ctx, name, metav1.GetOptions{})
			return err == nil, nil
		})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// The phase-1 done-when, on real Talos guests: changing one field rolls out a
// platform version, and changing it back rolls it back.
//
// One test rather than three, because the states are sequential: an upgrade
// that has not happened cannot be rolled back, and running them independently
// would mean three full rollouts to assert what one sequence shows.
func TestPlatform_VersionChangeRollsOutAndRollsBack(t *testing.T) {
	const rolloutTimeout = 10 * time.Minute

	// v0.1.0: a migration and a component, so the ordering is exercised.
	setPlatformVersion(t, "v0.1.0")
	waitHelloMessage(t, "v1", rolloutTimeout)
	waitPackagePresent(t, "hello-migrate", time.Minute)

	// The component must not have installed before the migration finished. Its
	// HelmRelease carries the dependency that made that true.
	deps := helmReleaseDependsOn(t, "hello")
	if len(deps) == 0 {
		t.Error("helmrelease hello has no dependsOn; the two-stage ordering is not in effect")
	}

	// v0.2.0 changes the component's values and drops the migration entirely.
	setPlatformVersion(t, "v0.2.0")
	waitHelloMessage(t, "v2", rolloutTimeout)
	waitPackageAbsent(t, "hello-migrate", 2*time.Minute)

	// Rollback. Not a special case in the reconciler, and it must restore what
	// the upgrade removed rather than only what it changed.
	setPlatformVersion(t, "v0.1.0")
	waitHelloMessage(t, "v1", rolloutTimeout)
	waitPackagePresent(t, "hello-migrate", 2*time.Minute)
}

func helmReleaseDependsOn(t *testing.T, name string) []any {
	t.Helper()

	hr, err := dynClient.Resource(helmReleaseGVR).Namespace("flux-system").
		Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get helmrelease %s: %v", name, err)
	}
	deps, _, err := unstructured.NestedSlice(hr.Object, "spec", "dependsOn")
	if err != nil {
		t.Fatalf("read dependsOn: %v", err)
	}
	return deps
}

// The operator reports what it rolled out, and status is what an operator on
// call reads first.
func TestPlatform_StatusReportsTheCurrentRelease(t *testing.T) {
	got, err := dynClient.Resource(platformGVR).Get(t.Context(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get platform: %v", err)
	}

	version, _, err := unstructured.NestedString(got.Object, "status", "current", "version")
	if err != nil || version == "" {
		t.Fatalf("status.current.version = %q (err %v), want the rolled-out version", version, err)
	}
	digest, _, err := unstructured.NestedString(got.Object, "status", "current", "digest")
	if err != nil || digest == "" {
		t.Errorf("status.current.digest = %q (err %v), want what the tag resolved to", digest, err)
	}
}
