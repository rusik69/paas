// Package crd installs the CustomResourceDefinitions this operator owns.
//
// The manifests are embedded rather than fetched, so a binary carries the exact
// schemas it was built against. An operator running against CRDs it does not
// recognise, or the reverse, is the stale-CRD problem this exists to remove.
package crd

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// FieldManager owns every field this package applies.
//
// This string is API. Changing it orphans field ownership on every CRD in every
// cluster already running, leaving fields nothing will ever update again.
const FieldManager = "paas-operator/crd"

//go:embed manifests/*.yaml
var manifests embed.FS

// ErrNoManifests means the embed pattern matched nothing.
var ErrNoManifests = errors.New("no embedded CRD manifests")

// Load parses the embedded CRD manifests.
func Load() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	return load(manifests)
}

// Split from Load so the failure paths are reachable from a test without an
// embed.FS, which is fixed at compile time.
func load(fsys fs.FS) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	names, err := fs.Glob(fsys, "manifests/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("glob embedded manifests: %w", err)
	}
	if len(names) == 0 {
		return nil, ErrNoManifests
	}

	out := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(names))
	for _, name := range names {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(b, crd); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out = append(out, crd)
	}
	return out, nil
}
