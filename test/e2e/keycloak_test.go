//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rusik69/paas/pkg/wait"
)

// Must agree with the API server's --oidc-* flags and the realm the chart
// imports. Written out here rather than read from either side: a test that
// derived these from the thing under test could not catch the two disagreeing,
// which is the failure mode that produces no error anywhere.
const (
	oidcIssuerHost   = "https://10.77.0.11:8443"
	oidcIssuerURL    = oidcIssuerHost + "/realms/paas"
	oidcClientID     = "kubernetes"
	oidcTestUser     = "alice"
	oidcTestPassword = "alice-password"
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

// The last link of phase 2: a token Keycloak issued, accepted by the API server,
// mapping through its groups claim to the tenant role the Tenant reconciler
// bound.
//
// Everything before this proved the pieces separately — bindings exist, Keycloak
// runs, the API server has an issuer configured. None of it proved they agree,
// and the ways they can silently fail to are the reason this test exists: a
// missing group mapper, or a claim carrying "/paas:tenant:acme" where the
// binding says "paas:tenant:acme", produce no error anywhere and authorize
// nothing.
func TestKeycloak_TokenAuthenticatesAndMapsToTheTenantRole(t *testing.T) {
	setPlatformVersion(t, "v0.1.0")
	ensureRootNamespace(t)

	// The tenant whose group the fixture user belongs to.
	applyTenant(t, rootNamespace, "acme", "business", true)
	waitNamespace(t, "tenant-acme", 3*time.Minute)

	token := keycloakToken(t)

	// The API server's own endpoint, with the token as the only credential: what
	// is under test is whether the API server accepts it and what it maps to.
	cfg := &rest.Config{
		Host:            adminHost(t),
		BearerToken:     token,
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}

	user, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build a client from the Keycloak token: %v", err)
	}

	// Positive half. A token that authenticates nobody yields 401 everywhere,
	// which would pass the negative assertion below on its own.
	if _, err := user.CoreV1().ConfigMaps("tenant-acme").
		List(t.Context(), metav1.ListOptions{}); err != nil {
		t.Fatalf("the token cannot read its own tenant's namespace: %v", err)
	}

	// And it must reach no further.
	_, err = user.CoreV1().Secrets("kube-system").List(t.Context(), metav1.ListOptions{})
	if err == nil {
		t.Fatal("a tenant token read kube-system")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("err = %v, want a 403 — a 401 would mean the token never authenticated at all", err)
	}
}

// keycloakToken fetches an access token by password grant, from inside the
// cluster, and hands it back through a ConfigMap.
//
// From inside the cluster because that is where the platform's own clients will
// ask — the dashboard in phase 4 among them. The issuer binds the control-plane
// node's host network, so a pod reaches it by a routable node address rather
// than through anything Cilium has to translate.
func keycloakToken(t *testing.T) string {
	t.Helper()

	ns := namespace(t, "e2e-kctoken")
	// curl, not busybox's wget: wget here cannot speak TLS at all, and the token
	// endpoint is only reachable over HTTPS.
	//
	// -k because the issuer's certificate is signed by the CA generated at
	// bring-up, which this pod has no copy of. What the certificate proves is
	// asserted where it matters — the API server validates it against the CA it
	// was configured with, and that is what the rest of this test exercises.
	//
	// The response comes back whole and is parsed below rather than sliced up
	// here. A sed that reaches for access_token leaves an error response almost
	// intact, and the fragment it yields is a non-empty string that no check
	// short of parsing recognises as not being a token — which is how "Account
	// is not fully set up" once arrived as a bare 401 from the API server.
	script := fmt.Sprintf(
		`curl -sk -X POST `+
			`-d 'grant_type=password' -d 'client_id=%s' -d 'username=%s' -d 'password=%s' `+
			`%s/protocol/openid-connect/token`,
		oidcClientID, oidcTestUser, oidcTestPassword, oidcIssuerURL)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   curlImageRef(t),
				Command: []string{"sh", "-c", script},
			}},
		},
	}
	createPod(t, ns, pod)

	phase, err := waitPodTerminated(t, ns, "token", 5*time.Minute)
	if err != nil || phase != corev1.PodSucceeded {
		dumpNamespace(t, ns)
		t.Fatalf("could not obtain a token from Keycloak (phase %s): %v", phase, err)
	}

	logs, err := clientset.CoreV1().Pods(ns).GetLogs("token", &corev1.PodLogOptions{}).
		DoRaw(t.Context())
	if err != nil {
		t.Fatalf("read the token response: %v", err)
	}

	var body struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(logs, &body); err != nil {
		t.Fatalf("Keycloak's token response is not JSON: %v (%q)", err, strings.TrimSpace(string(logs)))
	}
	if body.Error != "" {
		t.Fatalf("Keycloak refused to issue a token: %s: %s", body.Error, body.ErrorDescription)
	}
	if body.AccessToken == "" {
		t.Fatalf("Keycloak's token response carries no access_token: %q", strings.TrimSpace(string(logs)))
	}
	return body.AccessToken
}

// curlImageRef returns the pinned curl image.
func curlImageRef(t *testing.T) string {
	t.Helper()

	if *curlImage != "" {
		return *curlImage
	}
	return "curlimages/curl:" + pinnedVersion(t, "CURL_VERSION")
}

// adminHost returns the API server address the admin kubeconfig uses, which is
// reachable from the test process.
func adminHost(t *testing.T) string {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	return cfg.Host
}
