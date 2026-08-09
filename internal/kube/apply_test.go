package kube

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDecode_SkipsTheLeadingEmptyDocument(t *testing.T) {
	t.Parallel()

	// The shape `flux install --export` emits: a leading separator, so a naive
	// decoder yields one nameless object before the real ones.
	in := []byte(`---
apiVersion: v1
kind: Namespace
metadata:
  name: flux-system
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: source-controller
  namespace: flux-system
`)

	objs, err := Decode(in)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	var got []string
	for _, o := range objs {
		got = append(got, o.GetKind()+"/"+o.GetName())
	}
	want := []string{"Namespace/flux-system", "ServiceAccount/source-controller"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("decoded objects differ (-want +got):\n%s", diff)
	}
}

func TestDecode_EmptyInputIsNotAnError(t *testing.T) {
	t.Parallel()

	objs, err := Decode(nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("decoded %d objects from empty input, want 0", len(objs))
	}
}

// A document that parses as YAML but is not a Kubernetes object must not reach
// the apiserver as a nameless apply, which fails far from the cause. The
// decoder rejects it, so this pins that behaviour rather than re-checking it.
func TestDecode_RejectsADocumentWithNoKind(t *testing.T) {
	t.Parallel()

	_, err := Decode([]byte("just: a mapping\n"))
	if err == nil {
		t.Fatal("a document with no kind was accepted")
	}
	if !strings.Contains(err.Error(), "Kind") {
		t.Errorf("err = %q, want it to name the missing Kind", err)
	}
}

func TestDecode_ReportsMalformedYAML(t *testing.T) {
	t.Parallel()

	_, err := Decode([]byte("kind: [unterminated\n"))
	if err == nil {
		t.Fatal("malformed YAML was accepted")
	}
}

// A `null` document decodes to an object with no fields. Applying that would
// reach the apiserver as a nameless patch, so it is skipped rather than passed
// on.
func TestDecode_SkipsNullDocuments(t *testing.T) {
	t.Parallel()

	objs, err := Decode([]byte("null\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("decoded %d objects from a null document, want 0", len(objs))
	}
}
