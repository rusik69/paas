package platform

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

// Entry is one component of a platform release, as declared in the artifact's
// packages.yaml.
type Entry struct {
	Name    string                `json:"name"`
	Chart   string                `json:"chart"`
	Version string                `json:"version"`
	Stage   v1alpha1.PackageStage `json:"stage"`
	Values  *runtime.RawExtension `json:"values,omitempty"`
}

// Release is a platform version and everything it installs.
type Release struct {
	// Version is the tag that was requested.
	Version string
	// Digest is what that tag resolved to. Keeping both is what makes a
	// rollback to a tag auditable rather than hopeful.
	Digest string
	// Packages is the parsed packages.yaml, in file order.
	Packages []Entry
}

// Fetcher resolves a platform version into the release it names.
//
// An interface because the reconciler's behaviour — what it creates, updates
// and prunes — is worth testing exhaustively against a real API server, and
// standing up a registry to do that would test the registry instead.
type Fetcher interface {
	Fetch(ctx context.Context, registry, version string) (*Release, error)
}

// ParsePackages reads an artifact's packages.yaml.
//
// Exported because both the OCI fetcher and the fixtures in tests need exactly
// this, and two parsers would drift.
func ParsePackages(b []byte) ([]Entry, error) {
	var doc struct {
		Packages []Entry `json:"packages"`
	}
	if err := yaml.UnmarshalStrict(b, &doc); err != nil {
		return nil, fmt.Errorf("parse packages.yaml: %w", err)
	}
	if len(doc.Packages) == 0 {
		return nil, fmt.Errorf("packages.yaml declares no packages")
	}

	seen := make(map[string]bool, len(doc.Packages))
	for _, e := range doc.Packages {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("package entry has no name")
		case e.Chart == "":
			return nil, fmt.Errorf("package %q has no chart", e.Name)
		case e.Version == "":
			return nil, fmt.Errorf("package %q has no version", e.Name)
		case e.Stage != v1alpha1.StageMigration && e.Stage != v1alpha1.StageComponent:
			return nil, fmt.Errorf("package %q has stage %q, want migration or component", e.Name, e.Stage)
		case seen[e.Name]:
			// Two entries with one name would each overwrite the other's
			// Package on every reconcile, and the release would never settle.
			return nil, fmt.Errorf("package %q is declared twice", e.Name)
		}
		seen[e.Name] = true
	}
	return doc.Packages, nil
}
