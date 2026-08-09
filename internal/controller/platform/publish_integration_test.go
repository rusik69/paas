//go:build integration

package platform

import (
	"os/exec"
	"testing"
)

// The full pipeline: hack/publish.sh pushes, the fetcher reads. Everything
// between the two is what a cluster depends on.
func TestPublishScriptOutputIsFetchable(t *testing.T) {
	for _, tool := range []string{"helm", "flux"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed; run 'make deps-install'", tool)
		}
	}

	host := startRegistry(t)
	registry := "oci://" + host + "/paas"

	cmd := exec.CommandContext(t.Context(), "../../../hack/publish.sh", "all", "v9.9.9")
	cmd.Env = append(cmd.Environ(), "REGISTRY="+registry)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("publish.sh: %v\n%s", err, out)
	}

	got, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), registry, "v9.9.9")
	if err != nil {
		t.Fatalf("fetch what publish.sh pushed: %v", err)
	}
	if len(got.Packages) == 0 {
		t.Fatal("the published release declares no packages")
	}

	// The charts the release names must be pullable from where publish.sh put
	// them, or the HelmReleases resolve to nothing.
	for _, e := range got.Packages {
		ref := registry + "/" + e.Chart
		out, err := exec.CommandContext(t.Context(), "helm", "show", "chart",
			ref, "--version", e.Version, "--plain-http").CombinedOutput()
		if err != nil {
			t.Errorf("chart %s %s is not pullable from %s: %v\n%s", e.Chart, e.Version, ref, err, out)
		}
	}
}
