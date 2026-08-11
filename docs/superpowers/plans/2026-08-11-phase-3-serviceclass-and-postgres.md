# Phase 3 — ServiceClass machinery and the postgres catalog entry

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A tenant applies a `Postgres` in their namespace and gets a running HA CloudNativePG cluster, with the CRD for that kind generated at runtime from the chart's `values.schema.json` rather than written in Go.

**Architecture:** A cluster-scoped `ServiceClass` names a chart. Its reconciler pulls the chart from the in-cluster registry, converts the chart's `values.schema.json` into a Kubernetes structural schema, and applies a `CustomResourceDefinition` in group `apps.paas.io`. A dynamic controller engine then starts a generic reconciler for that kind, which renders each tenant CR into a `HelmRelease` in the same namespace — the CR's `.spec` being the Helm values verbatim — and copies status back.

**Tech Stack:** Go, controller-runtime, `k8s.io/apiextensions-apiserver`, `github.com/google/go-containerregistry` (chart pull, no Helm SDK), Flux `helm-controller`, CloudNativePG, envtest, Talos/KVM e2e.

**Design spec:** [docs/superpowers/specs/2026-08-11-phase-3-serviceclass-design.md](../specs/2026-08-11-phase-3-serviceclass-design.md). Read it before Task 1.

## Global Constraints

- **Read [docs/go-guidelines.md](../../go-guidelines.md) before any Go change.** Not optional.
- Reconcilers are level-triggered and idempotent, use server-side apply with a stable field manager, and never `context.Background()`.
- Field manager for the new service reconciler: **`paas-operator/service`**. Existing platform reconcilers use `paas-operator/platform` — do not reuse it; they write disjoint objects and one manager per writer keeps ownership legible.
- **No `time.Sleep` in tests.** Use `pkg/wait` or `testing/synctest`.
- `make test` stays under **ten seconds** and needs no cluster.
- Negative tests assert the **specific** denial — a 403, a named validation message — never merely that an error occurred.
- **Write fewer comments.** Default to none. A comment earns its place only by explaining *why*. Doc comments on exported identifiers stay required.
- Pinned versions live only in `hack/versions.sh`.
- Bash provisions, Go asserts. No `grep -q` checks in `hack/*.sh`.
- Generated CRD group is **`apps.paas.io`**, version **`v1alpha1`**, scope **Namespaced**.
- `x-kubernetes-preserve-unknown-fields` is **never** set anywhere in a generated CRD.
- Run `make verify` before every commit.

## Already done — do not rebuild

- **Tenant RBAC needs no change.** `internal/controller/tenant/rbac.go:31` already grants `apps.paas.io` with `resources: ["*"]` to `tenant-admin`/`tenant-viewer`. The spec's RBAC section is satisfied by existing code. Verify with `grep -n apps.paas.io internal/controller/tenant/rbac.go` and move on.
- **Flux watches all namespaces.** `--watch-all-namespaces=true` is set in `internal/flux/manifests/flux.yaml`, so a `HelmRelease` in a tenant namespace reconciles. Do not add a Flux config change.
- `internal/crd` already has `Apply` and an established-wait helper; `internal/controller/platform/oci.go` is the model for pulling from the in-cluster registry.

---

### Task 1: JSON Schema to structural schema, failing closed

The security-critical unit. A chart's `values.schema.json` becomes the CRD's `spec` schema. Anything that cannot be represented faithfully must produce an error naming the offending path — never a partial schema.

**Files:**
- Create: `internal/schema/structural.go`
- Test: `internal/schema/structural_test.go`
- Modify: `Makefile:22` (add `internal/schema` to `COVERED_PACKAGES`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func Convert(raw []byte) (*apiextensionsv1.JSONSchemaProps, error)`
  - `type UnrepresentableError struct { Path, Reason string }` with `func (e *UnrepresentableError) Error() string`

- [ ] **Step 1: Write the failing tests**

Create `internal/schema/structural_test.go`:

```go
package schema

import (
	"errors"
	"testing"
)

func TestConvert_PlainObject(t *testing.T) {
	got, err := Convert([]byte(`{
		"type": "object",
		"properties": {
			"instances": {"type": "integer", "minimum": 1},
			"size": {"type": "string"}
		},
		"required": ["instances"]
	}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.Type != "object" {
		t.Errorf("Type = %q, want object", got.Type)
	}
	if _, ok := got.Properties["instances"]; !ok {
		t.Error("instances is missing from the converted schema")
	}
	if got.Properties["instances"].Minimum == nil || *got.Properties["instances"].Minimum != 1 {
		t.Error("minimum was not carried across — a dropped constraint is an unvalidated field")
	}
	if len(got.Required) != 1 || got.Required[0] != "instances" {
		t.Errorf("Required = %v, want [instances]", got.Required)
	}
}

func TestConvert_PreserveUnknownFieldsIsNeverSet(t *testing.T) {
	got, err := Convert([]byte(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.XPreserveUnknownFields != nil {
		t.Error("x-kubernetes-preserve-unknown-fields was set — that hands the whole boundary away")
	}
}

func TestConvert_Rejects(t *testing.T) {
	cases := []struct {
		name, json, wantPath, wantReason string
	}{
		{
			name:     "ref",
			json:     `{"type":"object","properties":{"a":{"$ref":"#/definitions/x"}}}`,
			wantPath: ".properties.a",
			wantReason: "$ref",
		},
		{
			name:     "definitions",
			json:     `{"type":"object","definitions":{"x":{"type":"string"}},"properties":{}}`,
			wantPath: ".",
			wantReason: "definitions",
		},
		{
			name:     "patternProperties",
			json:     `{"type":"object","patternProperties":{"^a":{"type":"string"}}}`,
			wantPath: ".",
			wantReason: "patternProperties",
		},
		{
			name:     "typed oneOf",
			json:     `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`,
			wantPath: ".properties.a.oneOf[0]",
			wantReason: "type",
		},
		{
			name:     "untyped property",
			json:     `{"type":"object","properties":{"a":{"minimum":1}}}`,
			wantPath: ".properties.a",
			wantReason: "type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Convert([]byte(tc.json))
			if err == nil {
				t.Fatalf("Convert succeeded and returned %+v — a schema it cannot represent must produce no schema at all", got)
			}
			if got != nil {
				t.Error("Convert returned a schema alongside its error; a partial schema is the failure mode this exists to prevent")
			}
			var ue *UnrepresentableError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v, want an *UnrepresentableError naming the path", err)
			}
			if ue.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", ue.Path, tc.wantPath)
			}
			if !strings.Contains(ue.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", ue.Reason, tc.wantReason)
			}
		})
	}
}

func TestConvert_NotJSON(t *testing.T) {
	if _, err := Convert([]byte(`{not json`)); err == nil {
		t.Fatal("Convert accepted malformed JSON")
	}
}

func TestConvert_RootMustBeObject(t *testing.T) {
	_, err := Convert([]byte(`{"type":"string"}`))
	var ue *UnrepresentableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want an *UnrepresentableError", err)
	}
	if ue.Path != "." {
		t.Errorf("Path = %q, want .", ue.Path)
	}
}
```

Add `"strings"` to the test imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/schema/... -run TestConvert -v`
Expected: FAIL — `undefined: Convert`, `undefined: UnrepresentableError`.

- [ ] **Step 3: Write the implementation**

Create `internal/schema/structural.go`. The shape:

```go
// Package schema converts a chart's values.schema.json into the structural
// schema a CustomResourceDefinition needs.
package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// UnrepresentableError reports a schema Kubernetes structural schemas cannot
// express, and where in the document it is.
type UnrepresentableError struct {
	Path   string
	Reason string
}

func (e *UnrepresentableError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// Convert turns a JSON Schema document into a structural schema.
//
// It returns an error rather than a partial result for anything it cannot
// represent. Because a tenant CR's spec is passed to Helm as values, a dropped
// constraint is not a missing validation — it is an unvalidated field reaching
// the chart.
func Convert(raw []byte) (*apiextensionsv1.JSONSchemaProps, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse values.schema.json: %w", err)
	}
	return convert(doc, ".")
}

func convert(doc map[string]any, path string) (*apiextensionsv1.JSONSchemaProps, error) {
	for _, banned := range []string{"$ref", "definitions", "$defs", "patternProperties", "not", "dependencies"} {
		if _, ok := doc[banned]; ok {
			return nil, &UnrepresentableError{Path: path, Reason: banned + " is not expressible in a structural schema"}
		}
	}

	typ, _ := doc["type"].(string)
	if typ == "" {
		return nil, &UnrepresentableError{Path: path, Reason: "every subschema needs an explicit type"}
	}
	if path == "." && typ != "object" {
		return nil, &UnrepresentableError{Path: path, Reason: "the root of values.schema.json must be an object"}
	}

	out := &apiextensionsv1.JSONSchemaProps{Type: typ}
	if d, ok := doc["description"].(string); ok {
		out.Description = d
	}
	if n, ok := doc["minimum"].(float64); ok {
		out.Minimum = &n
	}
	if n, ok := doc["maximum"].(float64); ok {
		out.Maximum = &n
	}
	if s, ok := doc["pattern"].(string); ok {
		out.Pattern = s
	}
	if v, ok := doc["enum"]; ok {
		items, ok := v.([]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: "enum must be an array"}
		}
		for _, it := range items {
			b, err := json.Marshal(it)
			if err != nil {
				return nil, &UnrepresentableError{Path: path, Reason: "enum value is not JSON-encodable"}
			}
			out.Enum = append(out.Enum, apiextensionsv1.JSON{Raw: b})
		}
	}
	if v, ok := doc["default"]; ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, &UnrepresentableError{Path: path, Reason: "default is not JSON-encodable"}
		}
		out.Default = &apiextensionsv1.JSON{Raw: b}
	}
	if v, ok := doc["required"]; ok {
		items, ok := v.([]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: "required must be an array"}
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, &UnrepresentableError{Path: path, Reason: "required entries must be strings"}
			}
			out.Required = append(out.Required, s)
		}
	}

	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		v, ok := doc[key]
		if !ok {
			continue
		}
		items, ok := v.([]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: key + " must be an array"}
		}
		for i, it := range items {
			sub, ok := it.(map[string]any)
			if !ok {
				return nil, &UnrepresentableError{Path: fmt.Sprintf("%s.%s[%d]", path, key, i), Reason: "must be an object"}
			}
			// Structural schemas forbid these inside a logical combinator.
			for _, banned := range []string{"type", "additionalProperties", "default", "nullable"} {
				if _, bad := sub[banned]; bad {
					return nil, &UnrepresentableError{
						Path:   fmt.Sprintf("%s.%s[%d]", path, key, i),
						Reason: banned + " may not appear inside " + key,
					}
				}
			}
		}
		return nil, &UnrepresentableError{Path: path, Reason: key + " is not supported by the generator"}
	}

	if props, ok := doc["properties"].(map[string]any); ok {
		out.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			sub, ok := props[k].(map[string]any)
			if !ok {
				return nil, &UnrepresentableError{Path: path + ".properties." + k, Reason: "must be an object"}
			}
			conv, err := convert(sub, path+".properties."+k)
			if err != nil {
				return nil, err
			}
			out.Properties[k] = *conv
		}
	}

	if items, ok := doc["items"].(map[string]any); ok {
		conv, err := convert(items, path+".items")
		if err != nil {
			return nil, err
		}
		out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: conv}
	}

	return out, nil
}
```

Note the `oneOf`/`anyOf`/`allOf` handling: it checks the banned inner keys first so the test's `.oneOf[0]` path and `type` reason are produced, then refuses the combinator outright. Supporting combinators is a later decision; refusing them is the safe default.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/schema/... -v`
Expected: PASS, every subtest.

- [ ] **Step 5: Add the package to the coverage floor**

In `Makefile:22`, add `internal/schema` to `COVERED_PACKAGES`, keeping the list alphabetical:

```make
COVERED_PACKAGES ?= internal/controller/packagesource internal/controller/pkg internal/controller/platform internal/controller/tenant internal/crd internal/flux internal/kube internal/operator internal/schema pkg/tenancy pkg/wait
```

- [ ] **Step 6: Verify and commit**

Run: `make verify`
Expected: all gates pass, `internal/schema` at or above the 95% floor.

```bash
git add internal/schema Makefile
git commit -m "Convert a chart's values schema, or refuse to"
```

---

### Task 2: Pull a chart from the in-cluster registry and read its schema

**Files:**
- Create: `internal/chart/fetch.go`
- Test: `internal/chart/fetch_test.go`
- Modify: `Makefile:22` (add `internal/chart`)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `type Fetcher interface { Schema(ctx context.Context, registry, chart, version string) ([]byte, error) }`
  - `type OCIFetcher struct { Insecure bool }` implementing it
  - `var ErrNoSchema = errors.New("chart contains no values.schema.json")`

An interface for the same reason `platform.Fetcher` is one: the reconciler's behaviour is worth testing against a real API server without standing up a registry to do it.

- [ ] **Step 1: Write the failing test**

A Helm chart in OCI is an image whose layer is the chart `.tgz`, so the test builds one with `go-containerregistry`'s in-memory helpers rather than reaching for a registry.

Create `internal/chart/fetch_test.go`:

```go
package chart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"testing"
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/chart/... -v`
Expected: FAIL — `undefined: schemaFromTGZ`, `undefined: ErrNoSchema`, `undefined: maxSchemaFile`.

- [ ] **Step 3: Write the implementation**

Create `internal/chart/fetch.go`, mirroring `internal/controller/platform/oci.go`:

```go
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

// Fetcher resolves a chart reference into its values schema.
type Fetcher interface {
	Schema(ctx context.Context, registry, chart, version string) ([]byte, error)
}

// OCIFetcher reads the schema out of a chart in an OCI registry.
type OCIFetcher struct {
	// Insecure permits plain HTTP, which the in-cluster registry speaks.
	Insecure bool
}

// Schema pulls <registry>/<chart>:<version> and returns its values.schema.json.
func (f *OCIFetcher) Schema(ctx context.Context, registry, chart, version string) ([]byte, error) {
	repo := strings.TrimPrefix(registry, "oci://")
	if repo == "" {
		return nil, errors.New("registry is empty")
	}

	var opts []name.Option
	if f.Insecure {
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/chart/... -v`
Expected: PASS.

- [ ] **Step 5: Add to the coverage floor and commit**

Add `internal/chart` to `COVERED_PACKAGES` in `Makefile:22`.

Run: `make verify`

```bash
git add internal/chart Makefile
git commit -m "Read a chart's values schema out of the registry"
```

---

### Task 3: The ServiceClass API type

**Files:**
- Create: `api/platform/v1alpha1/serviceclass_types.go`
- Modify: `api/platform/v1alpha1/zz_generated.deepcopy.go` (regenerated, do not hand-edit)
- Modify: `internal/crd/manifests/` (regenerated)

**Interfaces:**
- Consumes: nothing.
- Produces: `ServiceClass`, `ServiceClassSpec`, `ServiceClassStatus`, `ServiceClassList`, `ChartRef`, `StatusSource`, `ObjectRef`, `UISpec`. Later tasks use `sc.Spec.Kind`, `sc.Spec.Plural`, `sc.Spec.Chart.Name`, `sc.Spec.Chart.Version`, `sc.Spec.StatusFrom`, `sc.Status.Conditions`, `sc.Status.ObservedChartVersion`.

**Reminder:** `api/` may depend on `k8s.io/apimachinery` and nothing else, because external clients import it.

- [ ] **Step 1: Write the type**

Create `api/platform/v1alpha1/serviceclass_types.go`:

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ChartRef names a chart in the platform's own registry.
//
// No repository field: a catalog entry pointing at an arbitrary registry would
// put an unreviewed values.schema.json in the position of being the security
// boundary for a tenant-facing kind.
type ChartRef struct {
	// Name is the chart name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the chart version.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

// ObjectRef names a kind to read status from.
type ObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
}

// StatusSource copies one field out of an underlying object into the tenant's
// own object status.
type StatusSource struct {
	// Path is where the value lands in the generated kind's status, as a dotted
	// path beginning with .status.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// From names the kind to read from.
	From ObjectRef `json:"from"`

	// JSONPath is the field to read, relative to the source object.
	// +kubebuilder:validation:MinLength=1
	JSONPath string `json:"jsonPath"`
}

// UISpec is what the dashboard needs to render a catalog entry.
type UISpec struct {
	// +optional
	Icon string `json:"icon,omitempty"`
	// +optional
	Category string `json:"category,omitempty"`
}

// ServiceClassSpec declares one tenant-facing kind and the chart behind it.
type ServiceClassSpec struct {
	// Kind is the generated kind in apps.paas.io/v1alpha1.
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	Kind string `json:"kind"`

	// Plural is the lowercase plural used in the resource path.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*$`
	Plural string `json:"plural"`

	// Chart supplies the values.schema.json that becomes this kind's schema.
	Chart ChartRef `json:"chart"`

	// StatusFrom propagates fields out of the objects the chart creates.
	// +optional
	StatusFrom []StatusSource `json:"statusFrom,omitempty"`

	// +optional
	UI UISpec `json:"ui,omitempty"`
}

// ServiceClassStatus reports the CRD generated from this class.
type ServiceClassStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedChartVersion is the chart version the live CRD schema was
	// generated from, so "which schema is serving" is answerable without
	// pulling anything.
	// +optional
	ObservedChartVersion string `json:"observedChartVersion,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ServiceClass turns a chart into a tenant-facing Kubernetes kind.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Chart",type=string,JSONPath=`.spec.chart.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.observedChartVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ServiceClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceClassSpec   `json:"spec,omitempty"`
	Status ServiceClassStatus `json:"status,omitempty"`
}

// ServiceClassList is a list of ServiceClass.
//
// +kubebuilder:object:root=true
type ServiceClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceClass{}, &ServiceClassList{})
}
```

- [ ] **Step 2: Regenerate deepcopy and CRD manifests**

Run: `make generate`
Expected: `zz_generated.deepcopy.go` gains `ServiceClass` methods and a new file appears under `internal/crd/manifests/`.

- [ ] **Step 3: Verify it compiles and the CRD loads**

Run: `go build ./... && go test ./internal/crd/... -v`
Expected: PASS — `internal/crd` embeds and loads the manifests directory, so a malformed generated CRD surfaces here.

- [ ] **Step 4: Verify and commit**

Run: `make verify`

```bash
git add api/platform/v1alpha1 internal/crd/manifests
git commit -m "Declare the ServiceClass kind"
```

---

### Task 4: Build a CRD from a ServiceClass and a schema

Pure function, no client. Split from the reconciler so the mapping is testable without an API server.

**Files:**
- Create: `internal/controller/serviceclass/crd.go`
- Test: `internal/controller/serviceclass/crd_test.go`

**Interfaces:**
- Consumes: `schema.Convert` (Task 1), `v1alpha1.ServiceClass` (Task 3).
- Produces:
  - `const Group = "apps.paas.io"`
  - `const Version = "v1alpha1"`
  - `func CRDFor(sc *v1alpha1.ServiceClass, rawSchema []byte) (*apiextensionsv1.CustomResourceDefinition, error)`
  - `func GVKFor(sc *v1alpha1.ServiceClass) schema.GroupVersionKind`

Note the import collision: this file imports both `internal/schema` and `k8s.io/apimachinery/pkg/runtime/schema`. Alias ours as `paasschema "github.com/rusik69/paas/internal/schema"`.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/serviceclass/crd_test.go`:

```go
package serviceclass

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func testClass() *v1alpha1.ServiceClass {
	return &v1alpha1.ServiceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres"},
		Spec: v1alpha1.ServiceClassSpec{
			Kind:   "Postgres",
			Plural: "postgreses",
			Chart:  v1alpha1.ChartRef{Name: "postgres", Version: "0.1.0"},
			StatusFrom: []v1alpha1.StatusSource{{
				Path:     ".status.primary",
				From:     v1alpha1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
				JSONPath: ".status.currentPrimary",
			}},
		},
	}
}

func TestCRDFor(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object","properties":{"instances":{"type":"integer"}}}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}

	if got, want := crd.Name, "postgreses."+Group; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got := crd.Spec.Scope; got != "Namespaced" {
		t.Errorf("Scope = %q, want Namespaced", got)
	}
	if got, want := crd.Spec.Names.Kind, "Postgres"; got != want {
		t.Errorf("Kind = %q, want %q", got, want)
	}

	v := crd.Spec.Versions[0]
	if !v.Served || !v.Storage {
		t.Error("the single version must be both served and storage")
	}
	if v.Subresources == nil || v.Subresources.Status == nil {
		t.Error("status subresource is off — kubectl get would report nothing real")
	}

	spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		t.Fatal("generated CRD has no spec")
	}
	if _, ok := spec.Properties["instances"]; !ok {
		t.Error("the chart's schema did not become the spec schema")
	}
	if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"]; !ok {
		t.Error("generated CRD has no status")
	}
	if v.Schema.OpenAPIV3Schema.XPreserveUnknownFields != nil {
		t.Error("preserve-unknown-fields is set at the root")
	}
}

func TestCRDFor_StatusFromBecomesAPrinterColumn(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}
	var found bool
	for _, c := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		if c.JSONPath == ".status.primary" {
			found = true
		}
	}
	if !found {
		t.Error("the first statusFrom path is not a printer column, so kubectl get shows no primary")
	}
}

func TestCRDFor_RejectsUnrepresentableSchema(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object","properties":{"a":{"$ref":"#/x"}}}`))
	if err == nil {
		t.Fatal("CRDFor built a CRD from a schema it cannot represent")
	}
	if crd != nil {
		t.Error("CRDFor returned a CRD alongside its error")
	}
}

func TestGVKFor(t *testing.T) {
	gvk := GVKFor(testClass())
	if gvk.Group != Group || gvk.Version != Version || gvk.Kind != "Postgres" {
		t.Errorf("GVKFor = %v, want %s/%s Postgres", gvk, Group, Version)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/serviceclass/... -v`
Expected: FAIL — `undefined: CRDFor`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/serviceclass/crd.go`:

```go
// Package serviceclass reconciles a ServiceClass into the CustomResourceDefinition
// that serves its kind.
package serviceclass

import (
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	paasschema "github.com/rusik69/paas/internal/schema"
)

// Group and Version are the API the generated kinds are served under.
const (
	Group   = "apps.paas.io"
	Version = "v1alpha1"
)

// GVKFor is the group-version-kind a ServiceClass generates.
func GVKFor(sc *v1alpha1.ServiceClass) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: Version, Kind: sc.Spec.Kind}
}

// CRDFor renders the CustomResourceDefinition for a ServiceClass.
//
// The chart's schema becomes .spec verbatim, which is what makes one
// values.schema.json the validation and the security boundary at once.
func CRDFor(sc *v1alpha1.ServiceClass, rawSchema []byte) (*apiextensionsv1.CustomResourceDefinition, error) {
	specSchema, err := paasschema.Convert(rawSchema)
	if err != nil {
		return nil, fmt.Errorf("chart %s:%s: %w", sc.Spec.Chart.Name, sc.Spec.Chart.Version, err)
	}

	columns := []apiextensionsv1.CustomResourceColumnDefinition{{
		Name:     "Ready",
		Type:     "string",
		JSONPath: `.status.conditions[?(@.type=="Ready")].status`,
	}}
	if len(sc.Spec.StatusFrom) > 0 {
		columns = append(columns, apiextensionsv1.CustomResourceColumnDefinition{
			Name:     "Primary",
			Type:     "string",
			JSONPath: sc.Spec.StatusFrom[0].Path,
		})
	}
	columns = append(columns, apiextensionsv1.CustomResourceColumnDefinition{
		Name:     "Age",
		Type:     "date",
		JSONPath: ".metadata.creationTimestamp",
	})

	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   sc.Spec.Plural + "." + Group,
			Labels: map[string]string{ManagedByLabel: sc.Name},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: Group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     sc.Spec.Kind,
				ListKind: sc.Spec.Kind + "List",
				Plural:   sc.Spec.Plural,
				Singular: strings.ToLower(sc.Spec.Kind),
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:                     Version,
				Served:                   true,
				Storage:                  true,
				Subresources:             &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				AdditionalPrinterColumns: columns,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec":   *specSchema,
							"status": statusSchema(),
						},
					},
				},
			}},
		},
	}, nil
}

// ManagedByLabel names the ServiceClass a generated CRD came from, so an
// orphaned CRD is identifiable after its class is gone.
const ManagedByLabel = "platform.paas.io/service-class"

// statusSchema is ours rather than the chart's: conditions plus whatever
// statusFrom copies in, which is free-form by construction.
func statusSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"observedGeneration": {Type: "integer", Format: "int64"},
			"primary":            {Type: "string"},
			"conditions": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type:     "object",
						Required: []string{"type", "status", "lastTransitionTime", "reason"},
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"type":               {Type: "string"},
							"status":             {Type: "string"},
							"observedGeneration": {Type: "integer", Format: "int64"},
							"lastTransitionTime": {Type: "string", Format: "date-time"},
							"reason":             {Type: "string"},
							"message":            {Type: "string"},
						},
					},
				},
			},
		},
	}
}
```

Add `"strings"` to the imports.

**Note on `statusSchema`:** it declares `primary` because the postgres class copies into `.status.primary`. A later class copying into a different path needs that path declared too — a structural schema will silently drop an undeclared field on write. Task 9's e2e is what catches this; when `redis` lands, its `statusFrom` path must be added here or the status write is a no-op.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/serviceclass/... -v`
Expected: PASS.

- [ ] **Step 5: Verify and commit**

Run: `make verify`

```bash
git add internal/controller/serviceclass
git commit -m "Render a ServiceClass into the CRD that serves its kind"
```

---

### Task 5: The dynamic controller engine

**Files:**
- Create: `internal/controller/engine/engine.go`
- Test: `internal/controller/engine/engine_test.go` — a plain unit test. It needs no API server, so it must run in `make test`; do not put it behind the integration tag.
- Modify: `Makefile:22` (add `internal/controller/engine`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Builder func(ctx context.Context, gvk schema.GroupVersionKind) error`
  - `type Engine struct { Manager ctrl.Manager; Build Builder }`
  - `func (e *Engine) Start(ctx context.Context, gvk schema.GroupVersionKind) error` — idempotent
  - `func (e *Engine) Stop(ctx context.Context, gvk schema.GroupVersionKind) error`
  - `func (e *Engine) Running(gvk schema.GroupVersionKind) bool`

`Stop` takes a context so cache removal has one. The global constraint against `context.Background()` has no exception here, and an earlier draft of this plan carved one out — it was wrong, and threading the caller's context is both cleaner and shorter.

`Build` is injected so the engine's lifecycle is testable without standing up a real service reconciler, and so Task 6 can supply the real one without the engine importing it.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/engine/engine_test.go`:

```go
package engine

import (
	"context"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testGVK = schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "Postgres"}

func TestEngine_StartIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	var built int
	e := &Engine{Build: func(ctx context.Context, gvk schema.GroupVersionKind) error {
		mu.Lock()
		defer mu.Unlock()
		built++
		return nil
	}}

	for range 3 {
		if err := e.Start(t.Context(), testGVK); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if built != 1 {
		t.Errorf("built %d controllers, want 1 — a ServiceClass reconciles repeatedly and must not accumulate them", built)
	}
	if !e.Running(testGVK) {
		t.Error("Running reports false after Start")
	}
}

func TestEngine_StopCancelsBeforeRemoving(t *testing.T) {
	var order []string
	var mu sync.Mutex
	done := make(chan struct{})

	e := &Engine{
		Build: func(ctx context.Context, gvk schema.GroupVersionKind) error {
			go func() {
				<-ctx.Done()
				mu.Lock()
				order = append(order, "cancelled")
				mu.Unlock()
				close(done)
			}()
			return nil
		},
		removeInformer: func(_ context.Context, gvk schema.GroupVersionKind) error {
			<-done
			mu.Lock()
			order = append(order, "removed")
			mu.Unlock()
			return nil
		},
	}

	if err := e.Start(t.Context(), testGVK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "cancelled" || order[1] != "removed" {
		t.Errorf("order = %v, want [cancelled removed] — removing an informer under a live controller delivers to a controller that has gone", order)
	}
	if e.Running(testGVK) {
		t.Error("Running reports true after Stop")
	}
}

func TestEngine_StopUnknownIsNotAnError(t *testing.T) {
	e := &Engine{Build: func(context.Context, schema.GroupVersionKind) error { return nil }}
	if err := e.Stop(t.Context(), testGVK); err != nil {
		t.Errorf("Stop on an unstarted kind returned %v; reconcile is level-triggered and will ask more than once", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/engine/... -v`
Expected: FAIL — `undefined: Engine`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/engine/engine.go`:

```go
// Package engine runs one controller per kind that did not exist when the
// manager started.
//
// controller-runtime builds its controllers before the manager starts, and has
// no way to remove one. Generated kinds appear and disappear with the catalog,
// so their controllers need a lifecycle of their own.
package engine

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Builder starts a controller for one kind. It must return once the controller
// is running, and the controller must stop when ctx is cancelled.
type Builder func(ctx context.Context, gvk schema.GroupVersionKind) error

// Engine owns the running controllers for generated kinds.
type Engine struct {
	Manager ctrl.Manager
	Build   Builder

	mu      sync.Mutex
	running map[schema.GroupVersionKind]context.CancelFunc

	// removeInformer is swappable so the lifecycle can be tested without a
	// live cache.
	removeInformer func(context.Context, schema.GroupVersionKind) error
}

// Start runs a controller for gvk. Calling it for a kind already running is a
// no-op, because a ServiceClass reconciles every time anything about it changes.
func (e *Engine) Start(ctx context.Context, gvk schema.GroupVersionKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running == nil {
		e.running = map[schema.GroupVersionKind]context.CancelFunc{}
	}
	if _, ok := e.running[gvk]; ok {
		return nil
	}

	// Deliberately not derived from the reconcile request's context: that one
	// is cancelled when the reconcile returns, and this controller outlives it.
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := e.Build(cctx, gvk); err != nil {
		cancel()
		return fmt.Errorf("start controller for %s: %w", gvk, err)
	}
	e.running[gvk] = cancel
	return nil
}

// Stop cancels the controller for gvk and drops its informer, in that order.
func (e *Engine) Stop(ctx context.Context, gvk schema.GroupVersionKind) error {
	e.mu.Lock()
	cancel, ok := e.running[gvk]
	if ok {
		delete(e.running, gvk)
	}
	e.mu.Unlock()

	if !ok {
		return nil
	}
	cancel()

	remove := e.removeInformer
	if remove == nil {
		remove = e.removeFromCache
	}
	if err := remove(ctx, gvk); err != nil {
		return fmt.Errorf("remove informer for %s: %w", gvk, err)
	}
	return nil
}

// Running reports whether a controller for gvk is live.
func (e *Engine) Running(gvk schema.GroupVersionKind) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.running[gvk]
	return ok
}

func (e *Engine) removeFromCache(ctx context.Context, gvk schema.GroupVersionKind) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return e.Manager.GetCache().RemoveInformer(ctx, u)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/engine/... -v`
Expected: PASS.

- [ ] **Step 5: Add to the coverage floor, verify and commit**

Add `internal/controller/engine` to `COVERED_PACKAGES`.

Run: `make verify`

```bash
git add internal/controller/engine Makefile
git commit -m "Run a controller per generated kind, and stop it again"
```

---

### Task 6: The generic per-kind reconciler

**Files:**
- Create: `internal/controller/service/reconciler.go`
- Test: `internal/controller/service/reconciler_test.go` — `desired` is pure rendering and needs no API server, so this runs in `make test`.
- Modify: `Makefile:22` (add `internal/controller/service`)

**Interfaces:**
- Consumes: `v1alpha1.ServiceClass` (Task 3), `serviceclass.Group`/`Version` (Task 4).
- Produces:
  - `const FieldManager = "paas-operator/service"`
  - `const LabelServiceName = "paas.io/service-name"`, `const LabelServiceNamespace = "paas.io/service-namespace"`
  - `type Reconciler struct { client.Client; Scheme *runtime.Scheme; GVK schema.GroupVersionKind; Class *v1alpha1.ServiceClass; Registry string }`
  - `func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error`
  - `func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)`
  - `func (r *Reconciler) desired(cr *unstructured.Unstructured) (*helmv2.HelmRelease, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/service/reconciler_test.go`. It tests `desired` directly — the rendering is the part worth pinning, and it needs no API server:

```go
package service

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

func testReconciler() *Reconciler {
	return &Reconciler{
		GVK:      schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "Postgres"},
		Registry: "oci://registry.paas-system.svc.cluster.local:5000/paas/charts",
		Class: &v1alpha1.ServiceClass{
			ObjectMeta: metav1.ObjectMeta{Name: "postgres"},
			Spec: v1alpha1.ServiceClassSpec{
				Kind:   "Postgres",
				Plural: "postgreses",
				Chart:  v1alpha1.ChartRef{Name: "postgres", Version: "0.1.0"},
			},
		},
	}
}

func testCR() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"instances": int64(2),
			"size":      "1Gi",
		},
	}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps.paas.io", Version: "v1alpha1", Kind: "Postgres"})
	u.SetName("db")
	u.SetNamespace("tenant-acme")
	u.SetUID("abc-123")
	return u
}

func TestDesired_SameNamespaceAndOwned(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}

	if hr.Namespace != "tenant-acme" {
		t.Errorf("Namespace = %q, want tenant-acme — helm-controller watches all namespaces so the release belongs beside the CR", hr.Namespace)
	}
	refs := hr.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("got %d owner references, want 1 — reclaim depends entirely on this", len(refs))
	}
	if refs[0].Kind != "Postgres" || refs[0].Name != "db" || refs[0].UID != "abc-123" {
		t.Errorf("owner reference = %+v, want the Postgres that asked for it", refs[0])
	}
	if refs[0].Controller == nil || !*refs[0].Controller {
		t.Error("owner reference is not a controller reference")
	}
}

func TestDesired_SpecBecomesValuesVerbatim(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if hr.Spec.Values == nil {
		t.Fatal("HelmRelease carries no values")
	}
	got := string(hr.Spec.Values.Raw)
	for _, want := range []string{`"instances":2`, `"size":"1Gi"`} {
		if !strings.Contains(got, want) {
			t.Errorf("values = %s, want it to contain %s", got, want)
		}
	}
}

func TestDesired_EmptySpecIsNotAnError(t *testing.T) {
	cr := testCR()
	unstructured.RemoveNestedField(cr.Object, "spec")

	hr, err := testReconciler().desired(cr)
	if err != nil {
		t.Fatalf("desired on a CR with no spec: %v — a chart whose values are all defaulted is legitimate", err)
	}
	if hr == nil {
		t.Fatal("desired returned no HelmRelease")
	}
}

func TestDesired_CarriesTheServiceLabels(t *testing.T) {
	hr, err := testReconciler().desired(testCR())
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	if hr.Labels[LabelServiceName] != "db" || hr.Labels[LabelServiceNamespace] != "tenant-acme" {
		t.Errorf("labels = %v, want the service name and namespace that status watches map back by", hr.Labels)
	}
}
```

Add `"strings"` to the imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/service/... -v`
Expected: FAIL — `undefined: Reconciler`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/service/reconciler.go`:

```go
// Package service reconciles a tenant's generated-kind object into the
// HelmRelease that installs it.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/rusik69/paas/api/platform/v1alpha1"
)

// FieldManager is this reconciler's alone. The platform reconcilers write
// disjoint objects under their own, and one manager per writer keeps ownership
// legible.
const FieldManager = "paas-operator/service"

// Labels every chart in the catalog stamps on everything it creates, so a
// status watch on an underlying kind maps back to the CR that asked for it.
const (
	LabelServiceName      = "paas.io/service-name"
	LabelServiceNamespace = "paas.io/service-namespace"
)

// ReleaseInterval matches the platform reconciler's.
const ReleaseInterval = 10 * time.Minute

// Reconciler renders one generated kind. One instance runs per kind.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	GVK      schema.GroupVersionKind
	Class    *v1alpha1.ServiceClass
	Registry string
}

// SetupWithManager registers a controller for this kind on an already-running
// manager, which is why it takes a context: the engine owns its lifetime.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(r.GVK)

	b := ctrl.NewControllerManagedBy(mgr).
		Named("service-"+r.Class.Name).
		For(cr).
		Owns(&helmv2.HelmRelease{})

	for _, s := range r.Class.Spec.StatusFrom {
		src := &unstructured.Unstructured{}
		src.SetAPIVersion(s.From.APIVersion)
		src.SetKind(s.From.Kind)
		b = b.WatchesRawSource(source.Kind(mgr.GetCache(), client.Object(src),
			handler.EnqueueRequestsFromMapFunc(byServiceLabels)))
	}
	return b.Complete(r)
}

// byServiceLabels maps an underlying object back to the CR whose status it
// belongs in, using the labels the chart contract requires.
func byServiceLabels(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	name, ns := l[LabelServiceName], l[LabelServiceNamespace]
	if name == "" || ns == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}}}
}

// Reconcile renders the derived HelmRelease and copies status back.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(r.GVK)
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	hr, err := r.desired(cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, hr, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply helmrelease %s/%s: %w", hr.Namespace, hr.Name, err)
	}

	return ctrl.Result{}, r.syncStatus(ctx, cr, hr)
}

func (r *Reconciler) desired(cr *unstructured.Unstructured) (*helmv2.HelmRelease, error) {
	values, found, err := unstructured.NestedMap(cr.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("read spec of %s/%s: %w", cr.GetNamespace(), cr.GetName(), err)
	}
	if !found {
		values = map[string]any{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode values for %s/%s: %w", cr.GetNamespace(), cr.GetName(), err)
	}

	yes := true
	return &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{APIVersion: helmv2.GroupVersion.String(), Kind: helmv2.HelmReleaseKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.GetName(),
			Namespace: cr.GetNamespace(),
			Labels: map[string]string{
				LabelServiceName:      cr.GetName(),
				LabelServiceNamespace: cr.GetNamespace(),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: r.GVK.GroupVersion().String(),
				Kind:       r.GVK.Kind,
				Name:       cr.GetName(),
				UID:        cr.GetUID(),
				Controller: &yes,
			}},
		},
		Spec: helmv2.HelmReleaseSpec{
			Interval: metav1.Duration{Duration: ReleaseInterval},
			Values:   &apiextensionsv1.JSON{Raw: raw},
		},
	}, nil
}
```

**`syncStatus` and the chart reference are deliberately left to Step 3b** — write `desired` first, get the tests in Step 1 green, then extend. Splitting keeps the failing-test cycle honest.

- [ ] **Step 3b: Add the chart reference and status sync**

Fill in `hr.Spec.Chart` as a `*helmv2.HelmChartTemplate` pointing at `r.Class.Spec.Chart.Name` and `.Version` against the platform's chart repository, copying the shape from `internal/controller/pkg/reconciler.go:145-160` rather than inventing a second one — that code sets `Chart:`, then `hr.Spec.Values = &apiextensionsv1.JSON{Raw: ...}`, which is the same `apiextensionsv1.JSON` type `desired` above uses. Read those lines first. Then write `syncStatus`:

```go
func (r *Reconciler) syncStatus(ctx context.Context, cr *unstructured.Unstructured, hr *helmv2.HelmRelease) error {
	var live helmv2.HelmRelease
	if err := r.Get(ctx, client.ObjectKeyFromObject(hr), &live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read helmrelease %s/%s: %w", hr.Namespace, hr.Name, err)
	}

	ready := metav1.ConditionUnknown
	var reason, message string
	for _, c := range live.Status.Conditions {
		if c.Type == "Ready" {
			ready, reason, message = c.Status, c.Reason, c.Message
		}
	}
	if reason == "" {
		reason = "Pending"
	}

	patch := cr.DeepCopy()
	_ = unstructured.SetNestedField(patch.Object, cr.GetGeneration(), "status", "observedGeneration")
	conds := []any{map[string]any{
		"type":               "Ready",
		"status":             string(ready),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
		"observedGeneration": cr.GetGeneration(),
	}}
	_ = unstructured.SetNestedSlice(patch.Object, conds, "status", "conditions")

	for _, s := range r.Class.Spec.StatusFrom {
		v, err := r.readStatusFrom(ctx, cr, s)
		if err != nil {
			return err
		}
		// An absent value is early, not broken: the release may not have
		// created its object yet.
		if v == "" {
			continue
		}
		_ = unstructured.SetNestedField(patch.Object, v, fieldPath(s.Path)...)
	}

	return r.Status().Patch(ctx, patch, client.MergeFrom(cr))
}
```

And the two helpers it calls:

```go
// fieldPath turns ".status.primary" into the segments SetNestedField wants.
func fieldPath(p string) []string {
	return strings.Split(strings.TrimPrefix(p, "."), ".")
}

// readStatusFrom finds the object a chart created for this CR and reads one
// field out of it. An empty return means "not there yet", which is early
// rather than broken.
func (r *Reconciler) readStatusFrom(ctx context.Context, cr *unstructured.Unstructured, s v1alpha1.StatusSource) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(s.From.APIVersion)
	list.SetKind(s.From.Kind + "List")
	if err := r.List(ctx, list,
		client.InNamespace(cr.GetNamespace()),
		client.MatchingLabels{
			LabelServiceName:      cr.GetName(),
			LabelServiceNamespace: cr.GetNamespace(),
		},
	); err != nil {
		// The kind may not be served at all if the chart has not installed its
		// operator yet, which is the same "early" case as an empty list.
		if meta.IsNoMatchError(err) {
			return "", nil
		}
		return "", fmt.Errorf("list %s in %s: %w", s.From.Kind, cr.GetNamespace(), err)
	}
	if len(list.Items) == 0 {
		return "", nil
	}

	jp := jsonpath.New("statusFrom")
	if err := jp.Parse("{" + s.JSONPath + "}"); err != nil {
		return "", fmt.Errorf("parse jsonPath %q: %w", s.JSONPath, err)
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, list.Items[0].Object); err != nil {
		return "", nil
	}
	return buf.String(), nil
}
```

Add `"bytes"`, `"strings"`, `"k8s.io/apimachinery/pkg/api/meta"` and `"k8s.io/client-go/util/jsonpath"` to the imports.

Write these three tests in `reconciler_test.go` first, and watch them fail:

```go
func TestFieldPath(t *testing.T) {
	got := fieldPath(".status.primary")
	if len(got) != 2 || got[0] != "status" || got[1] != "primary" {
		t.Errorf("fieldPath = %v, want [status primary]", got)
	}
}

func TestSyncStatus_AbsentSourceLeavesTheFieldUnset(t *testing.T) {
	// Build a Reconciler over a fake client with no CNPG Cluster present, call
	// readStatusFrom, and assert it returns "" and a nil error. A release that
	// has not created its Cluster yet is early, not an error, and returning one
	// would put the CR permanently in a failed state while it is merely starting.
}

func TestReadStatusFrom_ReadsTheLabelledObject(t *testing.T) {
	// Seed a fake client with an unstructured postgresql.cnpg.io/v1 Cluster in
	// tenant-acme carrying both service labels and
	// .status.currentPrimary = "db-1", then assert readStatusFrom returns "db-1".
	// Use fake.NewClientBuilder().WithScheme(s).WithObjects(...) with the GVK
	// registered via s.AddKnownTypeWithName, following whatever pattern
	// internal/controller/tenant's tests already use for fake clients.
}
```

Fill in the two commented bodies as real tests before implementing — they are described rather than written out because the fake-client setup must match the pattern already in this repo, which you should read first.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/service/... -v`
Expected: PASS.

- [ ] **Step 5: Add to the coverage floor, verify and commit**

Add `internal/controller/service` to `COVERED_PACKAGES`.

Run: `make verify`

```bash
git add internal/controller/service Makefile
git commit -m "Render a tenant's object into the release behind it"
```

---

### Task 7: The ServiceClass reconciler, and wiring it in

**Files:**
- Create: `internal/controller/serviceclass/reconciler.go`
- Create: `internal/controller/serviceclass/reconciler_integration_test.go`
- Modify: `internal/operator/operator.go:60-95`
- Modify: `Makefile:22` (add `internal/controller/serviceclass`)

**Interfaces:**
- Consumes: `CRDFor`/`GVKFor` (Task 4), `engine.Engine` (Task 5), `service.Reconciler` (Task 6), `chart.Fetcher` (Task 2).
- Produces: `type Reconciler struct { client.Client; Scheme *runtime.Scheme; Fetcher chart.Fetcher; Engine *engine.Engine; Registry string }` and `SetupWithManager(ctrl.Manager) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/serviceclass/reconciler_integration_test.go`. Follow the existing envtest suite pattern in `internal/crd/suite_integration_test.go` for cluster setup. Assert, with a fake `chart.Fetcher`:

```go
//go:build integration

package serviceclass

// TestReconcile_EstablishesTheCRDAndStartsAController — apply a ServiceClass with a
// fetcher returning a valid schema; expect a CRD named postgreses.apps.paas.io to
// exist and become Established, the ServiceClass Ready condition to be True, and
// status.observedChartVersion to equal the chart version.

// TestReconcile_UnrepresentableSchemaLeavesNoCRD — fetcher returns a schema with
// $ref; expect NO CRD created, Ready=False, and the condition message to contain
// the offending path ".properties.a". Assert the message, not merely that it failed:
// a condition that says "error" would pass while telling an operator nothing.

// TestReconcile_FetchFailureLeavesAWorkingCRDStanding — reconcile once with a good
// fetcher, then swap in one that errors and reconcile again; expect the CRD to still
// exist and Ready=False. A registry blip must not take a serving kind away.

// TestReconcile_DeletedServiceClassLeavesTheCRD — delete the ServiceClass; expect
// the engine to no longer be Running for the GVK, and the CRD to still exist
// carrying the ManagedByLabel. Deleting it would take every tenant's objects of
// that kind, and their data with them.
```

Write these four as real Go tests with real assertions before implementing.

- [ ] **Step 2: Run to verify they fail**

Run: `make test-integration 2>&1 | grep -A5 serviceclass`
Expected: FAIL — `undefined: Reconciler`.

- [ ] **Step 3: Implement the reconciler**

`Reconcile` does, in order: get the `ServiceClass` (on not-found, `Engine.Stop` the GVK and return — the CRD stays); fetch the schema; on fetch error set `Ready=False` reason `ChartUnavailable` and requeue **without touching any existing CRD**; `CRDFor`; on `*schema.UnrepresentableError` set `Ready=False` reason `SchemaNotStructural` with the path in the message and return **no error**, since retrying a published schema cannot fix it; server-side apply the CRD with `FieldManager`; wait for `Established` reusing the helper in `internal/crd/apply.go`; `Engine.Start` the GVK; set `Ready=True` and `observedChartVersion`.

Use a finalizer on the `ServiceClass` so `Stop` runs before the object vanishes.

- [ ] **Step 4: Wire it into the operator**

In `internal/operator/operator.go`, add to the `setups` table. The engine needs the manager, so build it before the table and give it a `Build` that constructs a `service.Reconciler` and calls its `SetupWithManager`:

```go
eng := &engine.Engine{Manager: mgr}
eng.Build = func(ctx context.Context, gvk schema.GroupVersionKind) error {
	// The ServiceClass is read fresh here rather than captured, so a chart
	// version bump restarts the controller against the current class.
	var sc v1alpha1.ServiceClass
	if err := mgr.GetClient().Get(ctx, client.ObjectKey{Name: opts.classNameFor(gvk)}, &sc); err != nil {
		return err
	}
	return (&service.Reconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		GVK: gvk, Class: &sc, Registry: opts.Registry,
	}).SetupWithManager(ctx, mgr)
}
```

Simplify if the class is already to hand — the point is that the engine does not import `serviceclass`, avoiding an import cycle. Add `{"serviceclass", (&serviceclass.Reconciler{...Engine: eng...}).SetupWithManager}` to the table.

Also add `apiextensionsv1` to the operator `Scheme` if it is not already registered, or the CRD apply fails at runtime with an unhelpful "no kind registered".

- [ ] **Step 5: Verify and commit**

Run: `make verify`

```bash
git add internal/controller/serviceclass internal/operator Makefile
git commit -m "Turn a ServiceClass into a served kind"
```

---

### Task 8: The postgres chart and the catalog package

**Files:**
- Create: `packages/apps/postgres/Chart.yaml`, `values.yaml`, `values.schema.json`, `templates/cluster.yaml`
- Create: `packages/system/catalog/Chart.yaml`, `values.yaml`, `templates/postgres.yaml`
- Modify: `packages/packages.yaml`
- Modify: `hack/publish.sh` if it does not already discover `packages/apps/**`

**Interfaces:**
- Consumes: nothing in Go.
- Produces: a chart named `postgres` at version `0.1.0` in the registry, and a `ServiceClass` named `postgres` in the cluster.

- [ ] **Step 1: Verify the reclaim assumption first**

Before writing the chart, settle the question the spec flags as risk #2. On a running cluster:

```bash
export KUBECONFIG=$PWD/.e2e/kubeconfig
kubectl -n paas-system get cluster.postgresql.cnpg.io keycloak-db -o jsonpath='{.metadata.uid}'; echo
kubectl -n paas-system get pvc keycloak-db-1 -o jsonpath='{.metadata.ownerReferences}'; echo
```

Expected: the PVC's owner references include the `Cluster` UID. If they do **not**, the ownership chain in the design is broken at its last link, and the chart needs an explicit reclaim step. Record the result in the design doc's Risks section either way before continuing.

- [ ] **Step 2: Write the values schema**

`packages/apps/postgres/values.schema.json` is the security boundary. Expose only what a tenant may set:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "instances": {
      "type": "integer",
      "minimum": 1,
      "maximum": 3,
      "default": 2,
      "description": "Number of Postgres instances. Two is the smallest that survives losing one."
    },
    "storage": {
      "type": "object",
      "properties": {
        "size": {"type": "string", "pattern": "^[0-9]+(Mi|Gi)$", "default": "1Gi"},
        "class": {"type": "string", "enum": ["replicated-2", "replicated-3"], "default": "replicated-3"}
      }
    },
    "resources": {
      "type": "object",
      "properties": {
        "cpu": {"type": "string", "pattern": "^[0-9]+m?$", "default": "100m"},
        "memory": {"type": "string", "pattern": "^[0-9]+(Mi|Gi)$", "default": "256Mi"}
      }
    }
  }
}
```

No image field, no arbitrary pod spec, no `podTemplate`. Every field a tenant can set is one someone has decided they may.

The defaults are sized for the dev cluster's two 3 GiB workers, per the spec's last risk.

- [ ] **Step 3: Write the chart template**

`packages/apps/postgres/templates/cluster.yaml` renders a CNPG `Cluster`. It **must** carry the chart-contract labels on every object:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
  labels:
    paas.io/service-name: {{ .Release.Name }}
    paas.io/service-namespace: {{ .Release.Namespace }}
spec:
  instances: {{ .Values.instances }}
  storage:
    size: {{ .Values.storage.size }}
    storageClass: {{ .Values.storage.class }}
  resources:
    requests:
      cpu: {{ .Values.resources.cpu }}
      memory: {{ .Values.resources.memory }}
```

Without those labels the status watch has nothing to map back by and `kubectl get postgres` shows no primary.

- [ ] **Step 4: Write the catalog package**

`packages/system/catalog/templates/postgres.yaml`:

```yaml
apiVersion: platform.paas.io/v1alpha1
kind: ServiceClass
metadata:
  name: postgres
spec:
  kind: Postgres
  plural: postgreses
  chart:
    name: postgres
    version: {{ .Values.postgres.version | quote }}
  statusFrom:
    - path: .status.primary
      from:
        apiVersion: postgresql.cnpg.io/v1
        kind: Cluster
      jsonPath: .status.currentPrimary
  ui:
    icon: postgres
    category: databases
```

Add `catalog` to `packages/packages.yaml` as a `component`, and give it `dependsOn` semantics equivalent to CNPG's if the manifest supports it — the `Cluster` CRD must exist before a tenant can create one, and CNPG is already a `migration` stage, which orders it first.

- [ ] **Step 5: Publish and verify by hand**

Run: `make cluster-up && make operator-up` (or reuse a running cluster), then:

```bash
export KUBECONFIG=$PWD/.e2e/kubeconfig
kubectl get serviceclass
kubectl get crd postgreses.apps.paas.io
kubectl explain postgres.spec.instances
```

Expected: the `ServiceClass` is Ready, the CRD exists, and `kubectl explain` shows the description from `values.schema.json` — which proves the schema really became the API.

- [ ] **Step 6: Commit**

```bash
git add packages hack
git commit -m "Add postgres to the catalog"
```

---

### Task 9: The e2e assertions

**Files:**
- Create: `test/e2e/service_test.go` (`//go:build e2e`)

**Interfaces:**
- Consumes: the helpers already in `test/e2e` — `setPlatformVersion`, `ensureRootNamespace`, `applyTenant`, `waitNamespace`, `clientset`, `dumpNamespace`. Read `test/e2e/tenancy_test.go` and `cnpg_test.go` for their exact signatures before writing.

**This task is for the `e2e-author` subagent** if you are dispatching per task — it owns `test/e2e`, and it may not run the suite or touch `hack/`.

- [ ] **Step 1: Write the tests**

```go
//go:build e2e

package e2e

// TestService_PostgresBecomesReadyAndReportsItsPrimary
//   setPlatformVersion v0.1.0; applyTenant acme; wait for tenant-acme.
//   Apply a Postgres named "db" in tenant-acme with instances: 2 via the
//   dynamic client.
//   Wait for .status.conditions Ready=True.
//   Assert .status.primary is non-empty AND equals the CNPG Cluster's
//   .status.currentPrimary — equality is the point, since a hardcoded string
//   would pass a test that only checked non-emptiness.
//   Assert the CNPG Cluster exists in tenant-acme with 2 ready instances.

// TestService_OffSchemaFieldIsRejectedWithItsOwnMessage
//   Apply a Postgres with instances: 99 (schema maximum is 3).
//   Assert the error is a StatusError whose message names "instances" and
//   mentions the maximum. Per non-negotiable #6: a test accepting any error
//   would keep passing if the CRD were generated with
//   x-kubernetes-preserve-unknown-fields, which is exactly the regression
//   worth catching.
//   Also apply a Postgres with an undeclared field "image": "evil:latest" and
//   assert it is rejected — that is the security boundary, stated as a test.

// TestService_DeleteReclaimsEverything
//   Delete the Postgres. Wait for the HelmRelease, the CNPG Cluster and the
//   PVCs to all be gone. Assert on the PVCs specifically: they are the last
//   link in the ownership chain and the one the design flagged as assumed.
```

Write these as real Go tests with real assertions.

- [ ] **Step 2: Check it compiles**

Run: `make vet-e2e`
Expected: clean. The e2e suite is invisible to `make test`, so this is the only thing that catches a broken build.

- [ ] **Step 3: Run against a real cluster**

Run: `make cluster-up && make operator-up && make e2e`
Expected: every test passes, the new ones included. Tear down with `make cluster-down` — a leaked guest holds gigabytes of RAM.

- [ ] **Step 4: Commit**

```bash
git add test/e2e
git commit -m "Prove a tenant's Postgres runs, validates, and reclaims"
```

---

### Task 10: Close the loop on docs and CI

**Files:**
- Modify: `docs/roadmap.md` (phase 3 bullets, with what landed)
- Modify: `docs/superpowers/specs/2026-08-11-phase-3-serviceclass-design.md` (Status, and the reclaim finding from Task 8 Step 1)

- [ ] **Step 1: Record what landed**

Update phase 3's bullets in `docs/roadmap.md` in the established style — a parenthesised *(Landed: ...)* note per bullet, naming the test that proves it. Mark the machinery done and `redis`/`bucket` outstanding.

- [ ] **Step 2: Update the spec status**

Change Status to `implemented and proven by <test names>`, and replace the "PVC reclaim is claimed and not yet known" risk with what Task 8 Step 1 actually found.

- [ ] **Step 3: Confirm CI needs nothing new**

Run: `grep -n "COVERED_PACKAGES" Makefile`
Expected: the five new packages are listed. No new CI job is needed — non-negotiable #10 is satisfied because `unit`, `integration` and `e2e` already run everything added here.

- [ ] **Step 4: Verify and commit**

Run: `make verify && make actionlint`

```bash
git add docs
git commit -m "Record what phase 3's machinery proved"
```

---

## Self-review

**Spec coverage:** `ServiceClass` type → Task 3. CRD generator → Tasks 1, 4. Dynamic reconciler → Tasks 5, 6, 7. Status propagation → Task 6. Chart → Task 8. Catalog delivery → Task 8. Fail-closed schema → Task 1, asserted again in Task 9. Orphaned CRD on delete → Task 7. Chart contract labels → Tasks 6, 8. Reclaim → Tasks 8, 9. RBAC → already done, noted above. Testing tiers → Tasks 1, 5, 6, 7, 9.

**Known gap, deliberately left:** `statusSchema()` in Task 4 hardcodes `primary` as the only `statusFrom` landing field. This is correct for `postgres` and wrong for the second service. Task 4's note says so, and it is the first thing the `redis` spec must fix — a generated status schema derived from `spec.statusFrom` rather than a fixed list. Fixing it now would be speculative; the second service is what shows the right shape.

**Type consistency:** `chart.Fetcher.Schema` is used by `serviceclass.Reconciler` (Task 7) with the signature Task 2 defines. `engine.Builder` is used in Task 7's wiring with the signature Task 5 defines. `service.Reconciler.SetupWithManager` takes a context in both Task 6 and Task 7. `GVKFor`/`CRDFor` are used in Task 7 as Task 4 defines them. `LabelServiceName`/`LabelServiceNamespace` are consistent across Tasks 6, 8 and 9.
