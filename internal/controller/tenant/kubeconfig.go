package tenant

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// APIEndpoint is what a generated kubeconfig points at.
type APIEndpoint struct {
	// URL is the API server address reachable from outside the cluster.
	URL string
	// CA is the PEM bundle that signs it.
	CA []byte
}

// serviceAccountTokenSecret asks the token controller to mint a token for the
// tenant's CI account.
//
// An explicitly-created Secret, which still yields a token that does not expire.
// architecture.md calls for a bound token instead, and that is the better
// answer: bound tokens expire, so a leaked one stops working. It needs a refresh
// loop in the operator, which is real work and unwritten — this is the debt, and
// it is named in the roadmap rather than left for someone to find.
func serviceAccountTokenSecret(namespace, tenant string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        ServiceAccountCI,
			Namespace:   namespace,
			Labels:      map[string]string{TenantLabel: tenant},
			Annotations: map[string]string{corev1.ServiceAccountNameKey: ServiceAccountCI},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
}

func serviceAccount(namespace, tenant string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name: ServiceAccountCI, Namespace: namespace,
			Labels: map[string]string{TenantLabel: tenant},
		},
	}
}

// renderKubeconfig builds a kubeconfig authenticating as the tenant's CI
// account, scoped to the tenant's namespace.
func renderKubeconfig(endpoint APIEndpoint, namespace, token string) ([]byte, error) {
	if endpoint.URL == "" {
		return nil, fmt.Errorf("no API endpoint configured; the kubeconfig would point nowhere")
	}
	if token == "" {
		return nil, fmt.Errorf("service account token is empty")
	}

	// Built as a map rather than with clientcmd's types, to keep this package's
	// dependencies to what the API types already pull in.
	cfg := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []any{map[string]any{
			"name": namespace,
			"cluster": map[string]any{
				"server":                     endpoint.URL,
				"certificate-authority-data": endpoint.CA,
			},
		}},
		"users": []any{map[string]any{
			"name": ServiceAccountCI,
			"user": map[string]any{"token": token},
		}},
		"contexts": []any{map[string]any{
			"name": namespace,
			"context": map[string]any{
				"cluster": namespace,
				"user":    ServiceAccountCI,
				// Pinned, so a kubeconfig used without -n cannot act outside
				// the tenant by accident.
				"namespace": namespace,
			},
		}},
		"current-context": namespace,
	}
	return yaml.Marshal(cfg)
}

func kubeconfigSecret(namespace, tenant string, kubeconfig []byte) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: KubeconfigSecret, Namespace: namespace,
			Labels: map[string]string{TenantLabel: tenant},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"kubeconfig": kubeconfig},
	}
}
