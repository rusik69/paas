package platform

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PackagesFile is the path the release artifact carries its package list at.
const PackagesFile = "packages.yaml"

// maxPackagesFile bounds what is read out of a layer. A platform release lists
// tens of charts; anything at this size is a malformed or hostile artifact, and
// reading it into memory unbounded is how an operator gets killed by the OOM
// killer instead of reporting a bad release.
const maxPackagesFile = 1 << 20

// ErrNoPackagesFile means the artifact carried no packages.yaml.
var ErrNoPackagesFile = errors.New("release artifact contains no " + PackagesFile)

// OCIFetcher resolves a platform version by pulling its OCI artifact.
type OCIFetcher struct {
	// Insecure permits plain HTTP, which the in-cluster registry speaks.
	Insecure bool
}

// Fetch pulls registry:version and reads its packages.yaml.
//
// The digest is read from the manifest rather than assumed from the tag, so
// status records what the tag actually resolved to.
func (f *OCIFetcher) Fetch(ctx context.Context, registry, version string) (*Release, error) {
	repo := strings.TrimPrefix(registry, "oci://")
	if repo == "" {
		return nil, errors.New("registry is empty")
	}

	var opts []name.Option
	if f.Insecure {
		opts = append(opts, name.Insecure)
	}
	ref, err := name.ParseReference(repo+":"+version, opts...)
	if err != nil {
		return nil, fmt.Errorf("parse reference %s:%s: %w", repo, version, err)
	}

	img, err := remote.Image(ref, remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("read digest of %s: %w", ref, err)
	}

	b, err := packagesFromImage(img)
	if err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", PackagesFile, ref, err)
	}
	entries, err := ParsePackages(b)
	if err != nil {
		return nil, fmt.Errorf("%s in %s: %w", PackagesFile, ref, err)
	}

	return &Release{Version: version, Digest: digest.String(), Packages: entries}, nil
}

// packagesFromImage returns the contents of packages.yaml, searching layers
// newest first so a later layer overrides an earlier one, as image layering
// means everywhere else.
func packagesFromImage(img v1.Image) ([]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("read layers: %w", err)
	}

	for i := len(layers) - 1; i >= 0; i-- {
		b, err := packagesFromLayer(layers[i])
		if err != nil {
			return nil, err
		}
		if b != nil {
			return b, nil
		}
	}
	return nil, ErrNoPackagesFile
}

// packagesFromLayer returns nil, nil when the layer simply does not carry the
// file, which is the ordinary case for every layer but one.
func packagesFromLayer(layer v1.Layer) ([]byte, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return nil, fmt.Errorf("open layer: %w", err)
	}
	// Read-only: a close failure tells us nothing we can act on, and returning
	// it would mask the read error that actually matters.
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read layer: %w", err)
		}
		if strings.TrimPrefix(hdr.Name, "./") != PackagesFile {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxPackagesFile+1))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", PackagesFile, err)
		}
		if len(b) > maxPackagesFile {
			return nil, fmt.Errorf("%s exceeds %d bytes", PackagesFile, maxPackagesFile)
		}
		return b, nil
	}
}
