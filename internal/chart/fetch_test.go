package chart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func chartTGZ(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestSchemaFromTGZ(t *testing.T) {
	want := `{"type":"object"}`
	tgz := chartTGZ(t, map[string]string{
		"postgres/Chart.yaml":         "name: postgres\n",
		"postgres/values.schema.json": want,
	})

	got, err := schemaFromTGZ(bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("schemaFromTGZ: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSchemaFromTGZ_Missing(t *testing.T) {
	tgz := chartTGZ(t, map[string]string{"postgres/Chart.yaml": "name: postgres\n"})

	_, err := schemaFromTGZ(bytes.NewReader(tgz))
	if !errors.Is(err, ErrNoSchema) {
		t.Fatalf("err = %v, want ErrNoSchema — a chart with no schema has no security boundary and must not yield a CRD", err)
	}
}

func TestSchemaFromTGZ_TooLarge(t *testing.T) {
	tgz := chartTGZ(t, map[string]string{
		"postgres/values.schema.json": string(bytes.Repeat([]byte("x"), maxSchemaFile+1)),
	})

	if _, err := schemaFromTGZ(bytes.NewReader(tgz)); err == nil {
		t.Fatal("schemaFromTGZ accepted an oversized schema; reading one unbounded is how the operator gets OOM-killed instead of reporting a bad chart")
	}
}

func TestSchemaFromTGZ_NotGzip(t *testing.T) {
	_, err := schemaFromTGZ(bytes.NewReader([]byte("not a gzip stream")))
	if err == nil || errors.Is(err, ErrNoSchema) {
		t.Fatalf("err = %v, want a non-ErrNoSchema failure to open the archive", err)
	}
	if !strings.Contains(err.Error(), "open chart archive") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

// Valid gzip wrapping bytes that are not a tar header at all: a genuine
// corruption, distinct from a chart that simply has no schema.
func TestSchemaFromTGZ_CorruptTar(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(bytes.Repeat([]byte{0x01}, 600)); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	_, err := schemaFromTGZ(bytes.NewReader(buf.Bytes()))
	if err == nil || errors.Is(err, ErrNoSchema) {
		t.Fatalf("err = %v, want a non-ErrNoSchema failure reading the archive", err)
	}
	if !strings.Contains(err.Error(), "read chart archive") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

// helmChartContentLayerMediaType is what Helm gives a chart layer when it
// pushes to OCI: the layer's stored blob is the .tgz itself, not a tar of it.
const helmChartContentLayerMediaType = types.MediaType("application/vnd.cncf.helm.chart.content.v1.tar+gzip")

// chartLayer wraps tgz as a layer the way Helm's OCI push does: the blob a
// puller reads back is the .tgz bytes verbatim, so Compressed() must return
// them unchanged rather than re-wrapping them in another tar+gzip.
func chartLayer(tgz []byte) v1.Layer {
	return static.NewLayer(tgz, helmChartContentLayerMediaType)
}

func TestSchemaFromImage_FindsSchemaInLayer(t *testing.T) {
	want := `{"type":"object"}`
	tgz := chartTGZ(t, map[string]string{"postgres/values.schema.json": want})

	img, err := mutate.AppendLayers(empty.Image, chartLayer(tgz))
	if err != nil {
		t.Fatalf("build image: %v", err)
	}

	got, err := schemaFromImage(img)
	if err != nil {
		t.Fatalf("schemaFromImage: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Image layering means a later layer wins, and schemaFromImage must agree
// with that rather than returning whichever layer it happens to read first.
func TestSchemaFromImage_NewestLayerWins(t *testing.T) {
	oldTGZ := chartTGZ(t, map[string]string{"postgres/values.schema.json": `{"type":"old"}`})
	newTGZ := chartTGZ(t, map[string]string{"postgres/values.schema.json": `{"type":"new"}`})

	base, err := mutate.AppendLayers(empty.Image, chartLayer(oldTGZ))
	if err != nil {
		t.Fatalf("build base: %v", err)
	}
	top, err := mutate.AppendLayers(base, chartLayer(newTGZ))
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}

	got, err := schemaFromImage(top)
	if err != nil {
		t.Fatalf("schemaFromImage: %v", err)
	}
	if string(got) != `{"type":"new"}` {
		t.Errorf("got %q, want the topmost layer's schema", got)
	}
}

func TestSchemaFromImage_NoSchemaAnywhere(t *testing.T) {
	tgz := chartTGZ(t, map[string]string{"postgres/Chart.yaml": "name: postgres\n"})

	img, err := mutate.AppendLayers(empty.Image, chartLayer(tgz))
	if err != nil {
		t.Fatalf("build image: %v", err)
	}

	_, err = schemaFromImage(img)
	if !errors.Is(err, ErrNoSchema) {
		t.Errorf("err = %v, want ErrNoSchema", err)
	}
}

// errImage overrides Layers() on top of a real image, to exercise the failure
// of a step a well-formed in-memory image can never actually fail at.
type errImage struct{ v1.Image }

func (errImage) Layers() ([]v1.Layer, error) { return nil, errors.New("boom") }

func TestSchemaFromImage_LayersError(t *testing.T) {
	_, err := schemaFromImage(errImage{empty.Image})
	if err == nil {
		t.Fatal("a failure listing layers was accepted")
	}
	if !strings.Contains(err.Error(), "read layers") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

// errLayer overrides Compressed() on top of a real layer, the same way
// errImage overrides Layers().
type errLayer struct{ v1.Layer }

func (errLayer) Compressed() (io.ReadCloser, error) { return nil, errors.New("boom") }

func TestSchemaFromImage_LayerOpenError(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image, errLayer{chartLayer(chartTGZ(t, nil))})
	if err != nil {
		t.Fatalf("build image: %v", err)
	}

	_, err = schemaFromImage(img)
	if err == nil {
		t.Fatal("a layer that fails to open was accepted")
	}
	if !strings.Contains(err.Error(), "open layer") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

// A real registry, in-process, following internal/controller/platform's
// pattern: the fetcher's whole job is talking to one, and a mocked transport
// would test the mock.
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

func pushChart(t *testing.T, host, chart, version string, tgz []byte) {
	t.Helper()

	img, err := mutate.AppendLayers(empty.Image, chartLayer(tgz))
	if err != nil {
		t.Fatalf("build chart image: %v", err)
	}
	ref, err := name.ParseReference(host+"/"+chart+":"+version, name.Insecure)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push chart: %v", err)
	}
}

func TestOCIFetcher_Schema_ReadsWhatWasPushed(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)
	want := `{"type":"object"}`
	pushChart(t, host, "postgres", "v1.0.0", chartTGZ(t, map[string]string{"postgres/values.schema.json": want}))

	got, err := (&OCIFetcher{Insecure: true}).Schema(t.Context(), "oci://"+host, "postgres", "v1.0.0")
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOCIFetcher_Schema_MissingSchemaIsNamed(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)
	pushChart(t, host, "postgres", "v1.0.0", chartTGZ(t, map[string]string{"postgres/Chart.yaml": "name: postgres\n"}))

	_, err := (&OCIFetcher{Insecure: true}).Schema(t.Context(), "oci://"+host, "postgres", "v1.0.0")
	if !errors.Is(err, ErrNoSchema) {
		t.Errorf("err = %v, want ErrNoSchema", err)
	}
	if !strings.Contains(err.Error(), "postgres:v1.0.0") {
		t.Errorf("err = %q, want it to name the chart reference", err)
	}
}

func TestOCIFetcher_Schema_PullFailure(t *testing.T) {
	t.Parallel()

	host := startRegistry(t)

	_, err := (&OCIFetcher{Insecure: true}).Schema(t.Context(), "oci://"+host, "postgres", "v0.0.0")
	if err == nil {
		t.Fatal("a version that was never published was accepted")
	}
	if !strings.Contains(err.Error(), "pull") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}

func TestOCIFetcher_Schema_RejectsAnEmptyRegistry(t *testing.T) {
	t.Parallel()

	if _, err := (&OCIFetcher{}).Schema(t.Context(), "oci://", "postgres", "v1"); err == nil {
		t.Error("an empty registry was accepted")
	}
}

func TestOCIFetcher_Schema_RejectsAnUnparseableReference(t *testing.T) {
	t.Parallel()

	_, err := (&OCIFetcher{}).Schema(t.Context(), "oci://NOT A HOST", "postgres", "v1")
	if err == nil {
		t.Fatal("an unparseable reference was accepted")
	}
	if !strings.Contains(err.Error(), "parse reference") {
		t.Errorf("err = %q, want it to name the failed step", err)
	}
}
