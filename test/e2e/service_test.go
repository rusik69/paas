//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/pkg/wait"
)

var (
	serviceClassGVR = schema.GroupVersionResource{
		Group: "platform.paas.io", Version: "v1alpha1", Resource: "serviceclasses",
	}
	postgresGVR = schema.GroupVersionResource{
		Group: "apps.paas.io", Version: "v1alpha1", Resource: "postgreses",
	}
)

// waitServiceClassReady waits for the postgres ServiceClass to report the CRD
// it generates as Established, at the chart version the catalog pins.
func waitServiceClassReady(t *testing.T, name string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var last string
	err := wait.For(ctx, 5*time.Second, "serviceclass "+name+" established",
		func(ctx context.Context) (bool, error) {
			got, err := dynClient.Resource(serviceClassGVR).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = err.Error()
				}
				return false, nil
			}
			status, reason := conditionStatus(got, "Ready")
			version, _, _ := unstructured.NestedString(got.Object, "status", "observedChartVersion")
			last = fmt.Sprintf("ready=%s reason=%s observedChartVersion=%q", status, reason, version)
			return status == "True" && reason == "Established", nil
		})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}
}

// conditionStatus reads one condition off an unstructured object's
// status.conditions, the shape both ServiceClass and every generated kind
// share.
func conditionStatus(obj *unstructured.Unstructured, condType string) (status, reason string) {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != condType {
			continue
		}
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		return s, r
	}
	return "", ""
}

// releaseNameFor is the name the service reconciler derives for a CR's
// HelmRelease, and so the Helm release name every object the chart creates is
// named after — the CNPG Cluster included. Two generated kinds can carry the
// same CR name in one namespace, and one release cannot install two charts.
func releaseNameFor(name, kind string) string {
	return name + "-" + strings.ToLower(kind)
}

func postgresFixture(ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps.paas.io/v1alpha1",
		"kind":       "Postgres",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       spec,
	}}
}

// waitPostgresReady waits for the Postgres CR to report Ready, with a primary
// that agrees with the CNPG Cluster's own opinion of who that is, AND every
// instance the fixture asked for actually ready — all three folded into one
// wait, so "this Postgres is up" has one definition and nothing downstream
// asserts a stronger condition than this waited for. The primary-equality
// check is the point of the test that calls this: a hardcoded status.primary
// would pass a check that only looked for non-emptiness. wantInstances is not
// loosened either — instance 2 can still be mid pg_basebackup onto a
// DRBD-replicated volume well after instance 1 alone reports a primary.
func waitPostgresReady(t *testing.T, ns, name string, wantInstances int64, timeout time.Duration) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var last, primary string
	err := wait.For(ctx, 10*time.Second,
		fmt.Sprintf("postgres %s ready with a primary and %d instance(s)", name, wantInstances),
		func(ctx context.Context) (bool, error) {
			got, err := dynClient.Resource(postgresGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = err.Error()
				}
				return false, nil
			}
			status, _ := conditionStatus(got, "Ready")
			p, _, _ := unstructured.NestedString(got.Object, "status", "primary")

			cluster, err := dynClient.Resource(clusterGVR).Namespace(ns).
				Get(ctx, releaseNameFor(name, "Postgres"), metav1.GetOptions{})
			if err != nil {
				if ctx.Err() == nil {
					last = fmt.Sprintf("postgres ready=%s primary=%q; cnpg cluster: %v", status, p, err)
				}
				return false, nil
			}
			clusterPrimary, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
			ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
			last = fmt.Sprintf("postgres ready=%s primary=%q cluster.currentPrimary=%q readyInstances=%d",
				status, p, clusterPrimary, ready)

			primary = p
			return status == "True" && p != "" && p == clusterPrimary && ready == wantInstances, nil
		})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}
	return primary
}

func pvcCount(ctx context.Context, t *testing.T, ns, cluster string) int {
	t.Helper()

	list, err := clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "cnpg.io/cluster=" + cluster,
	})
	if err != nil {
		t.Fatalf("list pvcs in %s: %v", ns, err)
	}
	return len(list.Items)
}

// The phase-3 done-when: a tenant's Postgres CR becomes a real, HA CNPG
// cluster, and the CR's own status reports the actual elected primary rather
// than a value the reconciler could get away with faking.
func TestService_PostgresBecomesReadyAndReportsItsPrimary(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "postgres", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "pg", "business", false)
	waitNamespace(t, "tenant-pg", 3*time.Minute)
	ns := "tenant-pg"
	const name = "db"

	obj := postgresFixture(ns, name, map[string]any{
		"instances": int64(2),
		"storage":   map[string]any{"size": "1Gi", "class": "replicated-3"},
		"resources": map[string]any{"cpu": "100m", "memory": "256Mi"},
	})
	if _, err := dynClient.Resource(postgresGVR).Namespace(ns).
		Create(t.Context(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create postgres %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := dynClient.Resource(postgresGVR).Namespace(ns).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete postgres %s/%s: %v", ns, name, err)
		}
	})

	primary := waitPostgresReady(t, ns, name, 2, 15*time.Minute)
	if primary == "" {
		t.Fatal("postgres reports no primary")
	}

	cluster, err := dynClient.Resource(clusterGVR).Namespace(ns).
		Get(t.Context(), releaseNameFor(name, "Postgres"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cnpg cluster %s/%s: %v", ns, name, err)
	}
	clusterPrimary, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	if primary != clusterPrimary {
		t.Errorf("postgres status.primary = %q, cnpg cluster status.currentPrimary = %q; want equal", primary, clusterPrimary)
	}

	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	if ready != 2 {
		t.Errorf("cnpg cluster readyInstances = %d, want 2", ready)
	}
}

// The negative half of the done-when. Per non-negotiable #6 each case asserts
// the specific denial: a generator built with
// x-kubernetes-preserve-unknown-fields would still error on nothing here and
// pass a looser test.
func TestService_OffSchemaFieldIsRejectedWithItsOwnMessage(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "postgres", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "pgschema", "business", false)
	waitNamespace(t, "tenant-pgschema", 3*time.Minute)
	ns := "tenant-pgschema"

	t.Run("out of range instances", func(t *testing.T) {
		const name = "too-big"
		obj := postgresFixture(ns, name, map[string]any{"instances": int64(99)})
		_, err := dynClient.Resource(postgresGVR).Namespace(ns).Create(t.Context(), obj, metav1.CreateOptions{})
		if err == nil {
			_ = dynClient.Resource(postgresGVR).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{})
			t.Fatal("instances: 99 was accepted; the schema's maximum of 3 is not being enforced")
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("err = %v, want a validation (Invalid) error", err)
		}
		const want = "spec.instances in body should be less than or equal to 3"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	})

	t.Run("undeclared field rejected by strict decoding", func(t *testing.T) {
		const name = "evil-image"
		obj := postgresFixture(ns, name, map[string]any{"instances": int64(1), "image": "evil:latest"})
		_, err := dynClient.Resource(postgresGVR).Namespace(ns).Create(t.Context(), obj,
			metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
		if err == nil {
			_ = dynClient.Resource(postgresGVR).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{})
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

	// The stronger property: even if strict decoding were somehow skipped, the
	// field can never reach Helm values, because the API server prunes what
	// the structural schema does not declare.
	t.Run("undeclared field pruned when strict decoding is bypassed", func(t *testing.T) {
		const name = "pruned"
		obj := postgresFixture(ns, name, map[string]any{"instances": int64(1), "image": "evil:latest"})
		if _, err := dynClient.Resource(postgresGVR).Namespace(ns).Create(t.Context(), obj,
			metav1.CreateOptions{FieldValidation: metav1.FieldValidationIgnore}); err != nil {
			t.Fatalf("create with fieldValidation=Ignore: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := dynClient.Resource(postgresGVR).Namespace(ns).
				Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("cleanup: delete postgres %s/%s: %v", ns, name, err)
			}
		})

		got, err := dynClient.Resource(postgresGVR).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get postgres %s/%s: %v", ns, name, err)
		}
		spec, _, err := unstructured.NestedMap(got.Object, "spec")
		if err != nil {
			t.Fatalf("read spec: %v", err)
		}
		if want := map[string]any{"instances": int64(1)}; !reflect.DeepEqual(spec, want) {
			t.Errorf("stored spec = %#v, want exactly %#v — an undeclared field must never reach Helm values",
				spec, want)
		}
	})
}

// The reclaim chain the design flags as assumed rather than known: deleting
// the CR must garbage-collect the HelmRelease, which uninstalls the release,
// which deletes the CNPG Cluster, which deletes its PVCs.
func TestService_DeleteReclaimsEverything(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	waitServiceClassReady(t, "postgres", 10*time.Minute)

	ensureRootNamespace(t)
	applyTenant(t, rootNamespace, "pgdelete", "business", false)
	waitNamespace(t, "tenant-pgdelete", 3*time.Minute)
	ns := "tenant-pgdelete"
	const name = "db"

	obj := postgresFixture(ns, name, map[string]any{"instances": int64(1)})
	if _, err := dynClient.Resource(postgresGVR).Namespace(ns).
		Create(t.Context(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create postgres %s/%s: %v", ns, name, err)
	}

	waitPostgresReady(t, ns, name, 1, 15*time.Minute)

	if n := pvcCount(t.Context(), t, ns, releaseNameFor(name, "Postgres")); n == 0 {
		t.Fatal("no PVCs exist before deletion; the reclaim assertion below would prove nothing")
	}

	if err := dynClient.Resource(postgresGVR).Namespace(ns).
		Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete postgres %s/%s: %v", ns, name, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	release := releaseNameFor(name, "Postgres")
	if err := wait.For(ctx, 5*time.Second, "helmrelease "+release+" reclaimed",
		func(ctx context.Context) (bool, error) {
			_, err := dynClient.Resource(helmReleaseGVR).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
		t.Fatalf("%v", err)
	}

	if err := wait.For(ctx, 5*time.Second, "cnpg cluster "+release+" reclaimed",
		func(ctx context.Context) (bool, error) {
			_, err := dynClient.Resource(clusterGVR).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
		dumpNamespace(t, ns)
		t.Fatalf("%v", err)
	}

	var lastPVCs int
	if err := wait.For(ctx, 5*time.Second, "pvcs for "+release+" reclaimed",
		func(ctx context.Context) (bool, error) {
			lastPVCs = pvcCount(ctx, t, ns, release)
			return lastPVCs == 0, nil
		}); err != nil {
		dumpNamespace(t, ns)
		t.Fatalf("%v (last: %d pvcs remain)", err, lastPVCs)
	}
}
