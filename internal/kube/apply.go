// Package kube holds the server-side-apply helpers shared by everything this
// operator installs.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Decode splits a multi-document YAML stream into unstructured objects.
//
// Empty documents are skipped: `flux install --export` separates every object
// with a leading `---`, so the first document is always empty and a decoder
// that treats that as an object applies a nameless one.
func Decode(b []byte) ([]*unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)

	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, fmt.Errorf("decode manifest stream: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		out = append(out, obj)
	}
}

// ApplyAll server-side applies every object under fieldManager, forcing
// ownership.
//
// Force is deliberate everywhere this is used: the operator owns what it
// installs, and without it a single manual edit leaves it wedged on a conflict
// it can never resolve on its own.
func ApplyAll(ctx context.Context, c client.Client, objs []*unstructured.Unstructured, fieldManager string) error {
	for _, obj := range objs {
		// A copy per apply: Patch writes the server's response back into the
		// object, and the managedFields it returns are rejected as input on the
		// next apply. Callers pass the same parsed manifests every time.
		if err := c.Patch(ctx, obj.DeepCopy(), client.Apply,
			client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}
