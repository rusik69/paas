package tenant

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRenderKubeconfig_IsUsableAndScopedToTheTenant(t *testing.T) {
	t.Parallel()

	out, err := renderKubeconfig(
		APIEndpoint{URL: "https://api.example:6443", CA: []byte("ca-bytes")},
		"tenant-acme", "the-token")
	if err != nil {
		t.Fatalf("renderKubeconfig: %v", err)
	}

	var cfg struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     []byte `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Contexts []struct {
			Context struct {
				Namespace string `json:"namespace"`
			} `json:"context"`
		} `json:"contexts"`
		CurrentContext string `json:"current-context"`
	}
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("the rendered kubeconfig does not parse: %v\n%s", err, out)
	}

	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != "https://api.example:6443" {
		t.Errorf("server = %+v, want the endpoint", cfg.Clusters)
	}
	if string(cfg.Clusters[0].Cluster.CA) != "ca-bytes" {
		t.Errorf("CA = %q, want the endpoint's", cfg.Clusters[0].Cluster.CA)
	}
	// Pinned, so a kubeconfig used without -n cannot act outside the tenant.
	if len(cfg.Contexts) != 1 || cfg.Contexts[0].Context.Namespace != "tenant-acme" {
		t.Errorf("context namespace = %+v, want tenant-acme", cfg.Contexts)
	}
	if cfg.CurrentContext != "tenant-acme" {
		t.Errorf("current-context = %q, want tenant-acme", cfg.CurrentContext)
	}
}

// A kubeconfig that points nowhere, or authenticates as nobody, is worse than
// none: it fails at the moment someone depends on it.
func TestRenderKubeconfig_RefusesToBuildAnUnusableOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		endpoint APIEndpoint
		token    string
		want     string
	}{
		{name: "no endpoint", token: "t", want: "point nowhere"},
		{name: "no token", endpoint: APIEndpoint{URL: "https://api"}, want: "token is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := renderKubeconfig(tc.endpoint, "tenant-acme", tc.token)
			if err == nil {
				t.Fatalf("an unusable kubeconfig was rendered for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The generated kubeconfig carries a credential. It must not be reachable
// through anything that is world-readable, which is why it lives in a Secret and
// nothing copies it into status.
func TestKubeconfigSecret_HoldsTheConfigUnderOneKey(t *testing.T) {
	t.Parallel()

	s := kubeconfigSecret("tenant-acme", "acme", []byte("kubeconfig-bytes"))
	if got := string(s.Data["kubeconfig"]); got != "kubeconfig-bytes" {
		t.Errorf("data[kubeconfig] = %q, want the rendered config", got)
	}
	if s.Labels[TenantLabel] != "acme" {
		t.Errorf("labels = %v, want the tenant label", s.Labels)
	}
}

func TestServiceAccountTokenSecret_AsksForTheRightAccount(t *testing.T) {
	t.Parallel()

	s := serviceAccountTokenSecret("tenant-acme", "acme")
	if s.Annotations["kubernetes.io/service-account.name"] != ServiceAccountCI {
		t.Errorf("annotations = %v, want it to name %s", s.Annotations, ServiceAccountCI)
	}
	if s.Type != "kubernetes.io/service-account-token" {
		t.Errorf("type = %q, want a service-account-token", s.Type)
	}
}
