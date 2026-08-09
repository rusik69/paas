//go:build integration

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rusik69/paas/api/v1alpha1"
)

// The producer/consumer contract. hack/publish.sh pushes release artifacts with
// `flux push artifact`, and the fetcher has to read exactly what that produces
// — a gzipped tar under a Flux-specific media type. Asserting it here means a
// change to either side fails in seconds rather than on a cluster.
func TestOCIFetcher_ReadsWhatFluxPushes(t *testing.T) {
	if _, err := exec.LookPath("flux"); err != nil {
		t.Skip("flux not installed; run 'make deps-install'")
	}

	host := startRegistry(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PackagesFile), []byte(validPackages), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "flux", "push", "artifact",
		"oci://"+host+"/paas:v1.4.2",
		"--path="+dir,
		"--source=test",
		"--revision=test@sha1:0000000000000000000000000000000000000000",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("flux push artifact: %v\n%s", err, out)
	}

	got, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "v1.4.2")
	if err != nil {
		t.Fatalf("Fetch what flux pushed: %v", err)
	}

	want := []Entry{
		{Name: "cnpg-migrate", Chart: "cnpg-migrations", Version: "1.27.0", Stage: v1alpha1.StageMigration},
		{Name: "cnpg", Chart: "cnpg", Version: "1.27.0", Stage: v1alpha1.StageComponent},
	}
	if diff := cmp.Diff(want, got.Packages); diff != "" {
		t.Errorf("packages differ (-want +got):\n%s", diff)
	}
	if got.Digest == "" {
		t.Error("digest is empty")
	}
}
