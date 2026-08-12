// Package chart pulls a Helm chart from an OCI registry and reads the schema
// that becomes a generated CRD's structural schema.
package chart

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// SchemaFile is the file within a chart that defines its writable surface.
const SchemaFile = "values.schema.json"

// maxSchemaFile bounds what is read out of a chart, for the reason
// maxPackagesFile does in the platform fetcher.
const maxSchemaFile = 1 << 20

// ErrNoSchema means the chart carries no values.schema.json.
var ErrNoSchema = errors.New("chart contains no " + SchemaFile)

// Source is a registry and the transport it speaks.
//
// The two travel together because they are one fact: the schema pull here and
// the HelmRepository a tenant's release resolves against must name the same
// registry over the same transport, and a plain-HTTP registry pulled over TLS
// fails in a way no test that does not pull a real chart can see. Passing them
// as a pair leaves no place for a second, disagreeing transport to be
// configured.
type Source struct {
	Registry string
	Insecure bool
}

// Fetcher resolves a chart reference into its values schema.
type Fetcher interface {
	Schema(ctx context.Context, src Source, chart, version string) ([]byte, error)
}

// OCIFetcher reads the schema out of a chart in an OCI registry.
type OCIFetcher struct{}

// Schema pulls <registry>/<chart>:<version> and returns its values.schema.json.
func (*OCIFetcher) Schema(ctx context.Context, src Source, chart, version string) ([]byte, error) {
	repo := strings.TrimPrefix(src.Registry, "oci://")
	if repo == "" {
		return nil, errors.New("registry is empty")
	}

	var opts []name.Option
	if src.Insecure {
		opts = append(opts, name.Insecure)
	}
	ref, err := name.ParseReference(path.Join(repo, chart)+":"+version, opts...)
	if err != nil {
		return nil, fmt.Errorf("parse reference %s/%s:%s: %w", repo, chart, version, err)
	}

	img, err := remote.Image(ref, remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	b, err := schemaFromImage(img)
	if err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", SchemaFile, ref, err)
	}
	return b, nil
}

// schemaFromImage searches layers newest first, so a later layer overrides an
// earlier one, as image layering means everywhere else.
func schemaFromImage(img v1.Image) ([]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("read layers: %w", err)
	}
	for i := len(layers) - 1; i >= 0; i-- {
		rc, err := layers[i].Compressed()
		if err != nil {
			return nil, fmt.Errorf("open layer: %w", err)
		}
		b, err := schemaFromTGZ(rc)
		_ = rc.Close()
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, ErrNoSchema) {
			return nil, err
		}
	}
	return nil, ErrNoSchema
}

// schemaFromTGZ reads values.schema.json out of a chart tarball. Helm packages
// a chart with its own name as the top directory, so the file is matched by
// base name rather than by full path.
func schemaFromTGZ(r io.Reader) ([]byte, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open chart archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoSchema
		}
		if err != nil {
			return nil, fmt.Errorf("read chart archive: %w", err)
		}
		if path.Base(hdr.Name) != SchemaFile {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxSchemaFile+1))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", SchemaFile, err)
		}
		if len(b) > maxSchemaFile {
			return nil, fmt.Errorf("%s exceeds %d bytes", SchemaFile, maxSchemaFile)
		}
		return b, nil
	}
}
