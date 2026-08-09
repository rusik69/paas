package platform

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/rusik69/paas/api/v1alpha1"
)

const validPackages = `packages:
  - name: cnpg-migrate
    chart: cnpg-migrations
    version: "1.27.0"
    stage: migration
  - name: cnpg
    chart: cnpg
    version: "1.27.0"
    stage: component
`

// A real registry, in-process. The fetcher's whole job is talking to one, and a
// mocked transport would test the mock.
func startRegistry(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry url: %v", err)
	}
	return u.Host
}

func pushArtifact(t *testing.T, host, repo, tag string, files map[string][]byte) v1.Hash {
	t.Helper()

	img, err := crane.Image(files)
	if err != nil {
		t.Fatalf("build artifact: %v", err)
	}
	ref, err := name.ParseReference(host+"/"+repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push artifact: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}

func TestOCIFetcher_ReadsTheReleaseAndItsDigest(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)
	want := pushArtifact(t, host, "paas", "v1.4.2",
		map[string][]byte{PackagesFile: []byte(validPackages)})

	got, err := (&OCIFetcher{Insecure: true}).
		Fetch(t.Context(), "oci://"+host+"/paas", "v1.4.2")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got.Version != "v1.4.2" {
		t.Errorf("version = %q, want v1.4.2", got.Version)
	}
	// The digest the tag resolved to, not one assumed from the tag: without it
	// a rollback to a moved tag lands on different bytes silently.
	if got.Digest != want.String() {
		t.Errorf("digest = %q, want %q", got.Digest, want.String())
	}

	wantPkgs := []Entry{
		{Name: "cnpg-migrate", Chart: "cnpg-migrations", Version: "1.27.0", Stage: v1alpha1.StageMigration},
		{Name: "cnpg", Chart: "cnpg", Version: "1.27.0", Stage: v1alpha1.StageComponent},
	}
	if diff := cmp.Diff(wantPkgs, got.Packages); diff != "" {
		t.Errorf("packages differ (-want +got):\n%s", diff)
	}
}

// Image layering means a later layer wins, and the fetcher must agree with that
// rather than returning whichever it happened to read first.
func TestOCIFetcher_TakesTheTopmostPackagesFile(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)

	base, err := crane.Image(map[string][]byte{PackagesFile: []byte("packages:\n  - name: old\n    chart: old\n    version: \"1\"\n    stage: component\n")})
	if err != nil {
		t.Fatalf("build base: %v", err)
	}
	top, err := mutate.AppendLayers(base, tarOf(t, PackagesFile, []byte(validPackages)))
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}
	ref, err := name.ParseReference(host+"/paas:layered", name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.Write(ref, top); err != nil {
		t.Fatalf("push: %v", err)
	}

	got, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "layered")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Packages) != 2 || got.Packages[0].Name != "cnpg-migrate" {
		t.Errorf("packages = %+v, want the topmost layer's list", got.Packages)
	}
}

func TestOCIFetcher_MissingPackagesFileIsNamed(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)
	pushArtifact(t, host, "paas", "empty", map[string][]byte{"README.md": []byte("nothing here")})

	_, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "empty")
	if !errors.Is(err, ErrNoPackagesFile) {
		t.Errorf("err = %v, want ErrNoPackagesFile", err)
	}
}

func TestOCIFetcher_MalformedPackagesFileNamesTheArtifact(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)
	pushArtifact(t, host, "paas", "bad",
		map[string][]byte{PackagesFile: []byte("packages: [unterminated\n")})

	_, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "bad")
	if err == nil {
		t.Fatal("a malformed packages.yaml was accepted")
	}
	if !strings.Contains(err.Error(), "paas:bad") {
		t.Errorf("err = %q, want it to name the artifact", err)
	}
}

func TestOCIFetcher_MissingTagIsReported(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)

	_, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "v0.0.0")
	if err == nil {
		t.Fatal("a version that was never published was accepted")
	}
	if !strings.Contains(err.Error(), "pull") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

func TestOCIFetcher_RejectsAnEmptyRegistry(t *testing.T) {
	t.Parallel()

	if _, err := (&OCIFetcher{}).Fetch(t.Context(), "oci://", "v1"); err == nil {
		t.Error("an empty registry was accepted")
	}
}

func TestOCIFetcher_RejectsAnUnparseableReference(t *testing.T) {
	t.Parallel()

	_, err := (&OCIFetcher{}).Fetch(t.Context(), "oci://NOT A HOST/paas", "v1")
	if err == nil {
		t.Fatal("an unparseable reference was accepted")
	}
	if !strings.Contains(err.Error(), "parse reference") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

// A single-file layer, as an uncompressed tarball.
func tarOf(t *testing.T, name string, content []byte) v1.Layer {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	contents := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents)), nil
	})
	if err != nil {
		t.Fatalf("build layer: %v", err)
	}
	return layer
}

// An unbounded read here is how a hostile or malformed artifact kills the
// operator with the OOM killer instead of failing one reconcile.
func TestOCIFetcher_RefusesAnOversizedPackagesFile(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)
	huge := append([]byte("packages:\n"), bytes.Repeat([]byte("# padding\n"), (maxPackagesFile/10)+1)...)
	pushArtifact(t, host, "paas", "huge", map[string][]byte{PackagesFile: huge})

	_, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "huge")
	if err == nil {
		t.Fatal("an oversized packages.yaml was read in full")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %q, want it to name the size limit", err)
	}
}

// A layer that is not a tar at all: the registry served bytes, and they are not
// what the media type claims.
func TestOCIFetcher_ReportsALayerThatIsNotATar(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)

	garbage, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{0xff}, 2048))), nil
	})
	if err != nil {
		t.Fatalf("build garbage layer: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, garbage)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	ref, err := name.ParseReference(host+"/paas:garbage", name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push: %v", err)
	}

	if _, err := (&OCIFetcher{Insecure: true}).Fetch(t.Context(), "oci://"+host+"/paas", "garbage"); err == nil {
		t.Error("a layer that is not a tar was accepted")
	}
}
