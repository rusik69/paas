// Command imgpush packages a single static binary into an OCI image and pushes
// it.
//
// This exists so the e2e harness can put the operator into the cluster without
// a container runtime on the host: the repository already depends on
// go-containerregistry, and requiring Docker to run the tests would add a
// daemon to the list of things that must be installed and working.
package main

import (
	"archive/tar"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func main() {
	binary := flag.String("binary", "", "path to the static binary to package")
	ref := flag.String("ref", "", "image reference to push, e.g. registry.paas.io/paas/operator:test")
	insecure := flag.Bool("insecure", true, "push over plain HTTP")
	flag.Parse()

	if *binary == "" || *ref == "" {
		fmt.Fprintln(os.Stderr, "usage: imgpush -binary <path> -ref <image>")
		os.Exit(2)
	}
	if err := run(*binary, *ref, *insecure); err != nil {
		fmt.Fprintf(os.Stderr, "imgpush: %v\n", err)
		os.Exit(1)
	}
}

func run(binary, ref string, insecure bool) error {
	entrypoint := "/" + path.Base(binary)

	layer, err := layerFor(binary, entrypoint)
	if err != nil {
		return err
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return fmt.Errorf("append layer: %w", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg = cfg.DeepCopy()
	cfg.Config.Entrypoint = []string{entrypoint}
	cfg.OS, cfg.Architecture = "linux", "amd64"
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		return fmt.Errorf("set config: %w", err)
	}

	var opts []name.Option
	if insecure {
		opts = append(opts, name.Insecure)
	}
	parsed, err := name.ParseReference(ref, opts...)
	if err != nil {
		return fmt.Errorf("parse %s: %w", ref, err)
	}
	if err := remote.Write(parsed, img); err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}

	digest, err := img.Digest()
	if err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	fmt.Println(digest.String())
	return nil
}

func layerFor(binary, entrypoint string) (v1.Layer, error) {
	b, err := os.ReadFile(binary)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", binary, err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: entrypoint,
		Mode: 0o755,
		Size: int64(len(b)),
	}); err != nil {
		return nil, fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write(b); err != nil {
		return nil, fmt.Errorf("write tar body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}

	contents := buf.Bytes()
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents)), nil
	})
}
