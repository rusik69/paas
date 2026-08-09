package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// The repository's own release manifest, checked against the parser that will
// read it from the artifact. A packages.yaml that only fails once published is
// one nobody sees until a cluster is mid-rollout.
func TestRepositoryPackagesManifestIsValid(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "packages", "packages.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	entries, err := ParsePackages(b)
	if err != nil {
		t.Fatalf("packages/packages.yaml is not valid: %v", err)
	}

	// Every chart named must exist under packages/, or publishing pushes a
	// release that references something nothing can pull.
	for _, e := range entries {
		matches, err := filepath.Glob(filepath.Join("..", "..", "..", "packages", "*", e.Chart, "Chart.yaml"))
		if err != nil {
			t.Fatalf("glob for chart %s: %v", e.Chart, err)
		}
		if len(matches) == 0 {
			t.Errorf("package %q names chart %q, which has no directory under packages/", e.Name, e.Chart)
		}
	}

	// The two-stage machinery is only exercised if a release actually has both.
	var migrations, components int
	for _, e := range entries {
		switch e.Stage {
		case "migration":
			migrations++
		case "component":
			components++
		}
	}
	if migrations == 0 || components == 0 {
		t.Errorf("release has %d migrations and %d components; both are needed for the ordering to mean anything",
			migrations, components)
	}
}
