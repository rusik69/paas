package crd

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// The embed pattern silently matching nothing is the failure this guards: it
// produces a binary that installs zero CRDs and reports success.
func TestLoad_ReturnsEveryEmbeddedCRD(t *testing.T) {
	t.Parallel()

	crds, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got []string
	for _, c := range crds {
		got = append(got, c.Name)
	}
	want := []string{"platforms.platform.paas.io"}

	less := func(a, b string) bool { return a < b }
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(less)); diff != "" {
		t.Errorf("loaded CRDs differ (-want +got):\n%s", diff)
	}
}

func TestLoad_CRDsAreWellFormed(t *testing.T) {
	t.Parallel()

	crds, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(crds) == 0 {
		t.Fatal("Load returned no CRDs")
	}

	for _, c := range crds {
		if c.Spec.Group != "platform.paas.io" {
			t.Errorf("%s group = %q, want platform.paas.io", c.Name, c.Spec.Group)
		}
		if len(c.Spec.Versions) == 0 {
			t.Errorf("%s declares no versions", c.Name)
			continue
		}
		for _, v := range c.Spec.Versions {
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				t.Errorf("%s version %s has no schema — controller-gen produced a stub", c.Name, v.Name)
			}
		}
	}
}

func TestLoad_EmptyFilesystemIsAnError(t *testing.T) {
	t.Parallel()

	_, err := load(fstest.MapFS{})
	if !errors.Is(err, ErrNoManifests) {
		t.Errorf("err = %v, want ErrNoManifests — an empty embed must not read as success", err)
	}
}

func TestLoad_MalformedYAMLNamesTheFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"manifests/broken.yaml": &fstest.MapFile{Data: []byte("this: [is: not: a: crd")},
	}
	_, err := load(fsys)
	if err == nil {
		t.Fatal("malformed YAML was accepted")
	}
	if !strings.Contains(err.Error(), "manifests/broken.yaml") {
		t.Errorf("err = %q, want it to name the offending file", err)
	}
}

// UnmarshalStrict, not Unmarshal: a manifest with a field the CRD type does not
// know is a controller-gen version skew, and silently dropping it would ship a
// schema nobody asked for.
func TestLoad_UnknownFieldIsRejected(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"manifests/extra.yaml": &fstest.MapFile{Data: []byte(
			"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: x\nnotAField: 1\n")},
	}
	_, err := load(fsys)
	if err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if !strings.Contains(err.Error(), "notAField") {
		t.Errorf("err = %q, want it to name the unknown field", err)
	}
}

func TestLoad_ReadFailureIsReported(t *testing.T) {
	t.Parallel()

	fsys := unreadableFS{fstest.MapFS{"manifests/boom.yaml": &fstest.MapFile{}}}
	_, err := load(fsys)
	if err == nil {
		t.Fatal("a read failure was swallowed")
	}
	if !strings.Contains(err.Error(), "manifests/boom.yaml") {
		t.Errorf("err = %q, want it to name the file that could not be read", err)
	}
}

// Globs to one entry that then refuses to open. Embedding the fs.FS interface
// rather than fstest.MapFS is deliberate: it hides MapFS's own ReadFile, so
// fs.ReadFile falls back to Open and takes the failing path.
type unreadableFS struct{ fs.FS }

func (u unreadableFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".yaml") {
		return nil, fs.ErrPermission
	}
	return u.FS.Open(name)
}

// fs.Glob delegates to the filesystem when it implements GlobFS, which is what
// makes this branch reachable at all. A filesystem that fails to enumerate must
// surface that rather than read as an empty embed.
type globErrFS struct{ fs.FS }

func (globErrFS) Glob(string) ([]string, error) { return nil, fs.ErrInvalid }

func TestLoad_GlobFailureIsReported(t *testing.T) {
	t.Parallel()

	_, err := load(globErrFS{fstest.MapFS{}})
	if err == nil {
		t.Fatal("a glob failure was swallowed")
	}
	if errors.Is(err, ErrNoManifests) {
		t.Error("a glob failure was reported as an empty embed")
	}
	if !strings.Contains(err.Error(), "glob") {
		t.Errorf("err = %q, want it to name the failing step", err)
	}
}
