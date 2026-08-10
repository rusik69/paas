//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/pkg/wait"
)

// Keycloak on a database the platform provisioned for it. Together with the
// CNPG suite this is the whole reason CloudNativePG was pulled forward: the
// identity provider phase 2 owes needs somewhere to keep its state.
func TestKeycloak_ServesItsDiscoveryDocument(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	// The StatefulSet, not a Deployment: the vendored chart runs Keycloak as
	// one, and its dbchecker init container is what holds it until the database
	// accepts connections.
	var last string
	err := wait.For(ctx, 10*time.Second, "keycloak ready", func(ctx context.Context) (bool, error) {
		sets, err := clientset.AppsV1().StatefulSets(platformNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=keycloakx",
		})
		if err != nil {
			if ctx.Err() == nil {
				last = err.Error()
			}
			return false, nil
		}
		for _, s := range sets.Items {
			last = fmt.Sprintf("%s ready=%d/%d", s.Name, s.Status.ReadyReplicas, s.Status.Replicas)
			if s.Status.ReadyReplicas > 0 {
				return true, nil
			}
		}
		if len(sets.Items) == 0 {
			last = "no keycloak statefulset yet"
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}

	// Ready is the StatefulSet's opinion. Serving OIDC discovery is the fact,
	// and it is the exact document the API server will fetch when its OIDC
	// trust is wired up — so a Keycloak that runs but cannot answer it would be
	// useless in precisely the way that matters.
	svc := keycloakService(t)
	if !probeSucceeds(t, fmt.Sprintf(
		"http://%s.%s.svc.cluster.local/realms/master/.well-known/openid-configuration",
		svc, platformNamespace)) {
		t.Error("keycloak is ready but does not serve its OIDC discovery document")
	}
}

func keycloakService(t *testing.T) string {
	t.Helper()

	svcs, err := clientset.CoreV1().Services(platformNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=keycloakx",
	})
	if err != nil || len(svcs.Items) == 0 {
		t.Fatalf("find the keycloak service: %v", err)
	}
	// The headless Service has no cluster IP and is not what a client dials.
	for _, s := range svcs.Items {
		if s.Spec.ClusterIP != corev1.ClusterIPNone && s.Spec.ClusterIP != "" {
			return s.Name
		}
	}
	t.Fatalf("keycloak has no routable Service among %d", len(svcs.Items))
	return ""
}

// probeSucceeds fetches a URL from inside the cluster and reports whether it
// answered.
func probeSucceeds(t *testing.T, url string) bool {
	t.Helper()

	ns := namespace(t, "e2e-keycloak")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fetch", Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "main",
				Image: *busyboxImage,
				// -O- so a body that is not JSON still fails loudly rather than
				// being written away to a file nobody reads.
				Command: []string{
					"sh", "-c",
					"wget -q -T 20 -O- " + url + " | grep -q authorization_endpoint",
				},
			}},
		},
	}
	createPod(t, ns, pod)

	phase, err := waitPodTerminated(t, ns, "fetch", 5*time.Minute)
	if err != nil {
		t.Errorf("discovery probe never terminated: %v", err)
		return false
	}
	return phase == corev1.PodSucceeded
}
