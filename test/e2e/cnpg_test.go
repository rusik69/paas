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

	"github.com/rusik69/paas/pkg/wait"
)

var clusterGVR = schema.GroupVersionResource{
	Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters",
}

// CNPG arrives as a platform package, so this also proves the delivery
// machinery carries a real upstream component and not only the fixtures phase 1
// was proven with.
func TestCNPG_OperatorIsDeliveredByThePlatform(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	var last string
	err := wait.For(ctx, 5*time.Second, "cnpg operator available", func(ctx context.Context) (bool, error) {
		list, err := clientset.AppsV1().Deployments("flux-system").List(ctx, metav1.ListOptions{})
		if err != nil {
			if ctx.Err() == nil {
				last = err.Error()
			}
			return false, nil
		}
		for _, d := range list.Items {
			if d.Name != "cnpg-cloudnative-pg" {
				continue
			}
			last = fmt.Sprintf("available=%d/%d", d.Status.AvailableReplicas, d.Status.Replicas)
			return d.Status.AvailableReplicas > 0, nil
		}
		last = "no cnpg deployment yet"
		return false, nil
	})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}

	// The CRD the tenant-facing Postgres kind will be built on in phase 3.
	if _, err := dynClient.Resource(clusterGVR).Namespace("default").
		List(t.Context(), metav1.ListOptions{}); err != nil {
		t.Errorf("the cnpg Cluster kind is not served: %v", err)
	}
}

// A real database on replicated storage, which is what Keycloak needs and what
// makes CNPG worth delivering at all. Asserting the operator is running proves
// far less: an operator that reconciles nothing looks identical.
func TestCNPG_ClusterBecomesReadyOnReplicatedStorage(t *testing.T) {
	ns := namespace(t, "e2e-cnpg")

	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "db", "namespace": ns},
		"spec": map[string]any{
			// One instance: this asserts that a database comes up on replicated
			// storage, and DRBD replication is already proven by the storage
			// suite. Three would triple the slowest test here for no new claim.
			"instances": int64(1),
			"storage": map[string]any{
				"size":         "1Gi",
				"storageClass": "replicated-3",
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
			},
		},
	}}

	if _, err := dynClient.Resource(clusterGVR).Namespace(ns).
		Create(t.Context(), cluster, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create cnpg cluster: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := dynClient.Resource(clusterGVR).Namespace(ns).
			Delete(ctx, "db", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete cluster: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	var last string
	err := wait.For(ctx, 10*time.Second, "cnpg cluster db ready", func(ctx context.Context) (bool, error) {
		got, err := dynClient.Resource(clusterGVR).Namespace(ns).
			Get(ctx, "db", metav1.GetOptions{})
		if err != nil {
			if ctx.Err() == nil {
				last = err.Error()
			}
			return false, nil
		}
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		ready, _, _ := unstructured.NestedInt64(got.Object, "status", "readyInstances")
		last = fmt.Sprintf("phase=%q readyInstances=%d", phase, ready)
		return ready >= 1, nil
	})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}

	// Reported ready is the operator's opinion. Connecting is the fact.
	if !postgresAnswers(t, ns) {
		t.Error("the database reports ready but refuses connections")
	}
}

// postgresAnswers runs a query as the generated application user.
func postgresAnswers(t *testing.T, ns string) bool {
	t.Helper()

	// CNPG generates this Secret for the app database; using it also proves the
	// credentials it hands out are the ones that work.
	secret := "db-app"

	// The image the database itself runs, rather than a pinned one: it needs no
	// entry in hack/versions.sh, and a client that matches the server cannot
	// fail for a version reason the test would then have to explain away.
	image := databaseImage(t, ns)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "psql", Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "main",
				Image: image,
				Command: []string{
					"sh", "-c",
					`PGPASSWORD="$PGPASS" psql -h "$PGHOST" -U "$PGUSER" -d "$PGDB" -c 'select 1' `,
				},
				Env: []corev1.EnvVar{
					{Name: "PGHOST", ValueFrom: secretKey(secret, "host")},
					{Name: "PGUSER", ValueFrom: secretKey(secret, "username")},
					{Name: "PGPASS", ValueFrom: secretKey(secret, "password")},
					{Name: "PGDB", ValueFrom: secretKey(secret, "dbname")},
				},
			}},
		},
	}
	createPod(t, ns, pod)

	phase, err := waitPodTerminated(t, ns, "psql", 5*time.Minute)
	if err != nil {
		t.Errorf("psql pod never terminated: %v", err)
		return false
	}
	return phase == corev1.PodSucceeded
}

// databaseImage returns the image CNPG chose for the cluster's instances.
func databaseImage(t *testing.T, ns string) string {
	t.Helper()

	pods, err := clientset.CoreV1().Pods(ns).List(t.Context(), metav1.ListOptions{
		LabelSelector: "cnpg.io/cluster=db",
	})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("find a database pod in %s: %v", ns, err)
	}
	for _, c := range pods.Items[0].Spec.Containers {
		if c.Name == "postgres" {
			return c.Image
		}
	}
	return pods.Items[0].Spec.Containers[0].Image
}

func secretKey(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Key:                  key,
		},
	}
}
