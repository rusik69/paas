# Phase 1 API and CRD Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `api/v1alpha1` (`Platform`, `PackageSource`, `Package`) and a `paas-operator` that installs its own CRDs from embedded manifests on every start.

**Architecture:** Hand-written Go types carrying kubebuilder markers; `controller-gen` produces deepcopy methods and CRD YAML; the YAML is embedded into the operator binary and server-side applied with a frozen field manager on startup, then waited on until `Established`.

**Tech Stack:** Go 1.25, `k8s.io/*` v0.34.1, controller-runtime v0.22.5, controller-tools v0.19.0 (build tool), envtest via setup-envtest v0.24.1.

## Global Constraints

Every task's requirements implicitly include these. They come from AGENTS.md and docs/go-guidelines.md; violating one fails review even if tests pass.

- **`api/v1alpha1` may import `k8s.io/apimachinery` and nothing else.** Not controller-runtime, not `internal/`. External clients import this package. This is why `groupversion_info.go` is hand-rolled in Task 1 rather than copied from a kubebuilder scaffold — the scaffold imports `sigs.k8s.io/controller-runtime/pkg/scheme`.
- **All pinned versions live only in `hack/versions.sh`.** Nothing else may hardcode one. New pins also get a `versions_check` entry.
- **No `time.Sleep` in tests.** Use `pkg/wait` or `testing/synctest`.
- **`make test` stays under ten seconds and needs no cluster.** Integration tests go behind `//go:build integration`.
- **Field manager strings are API.** `paas-operator/crd` is frozen from its first commit.
- **Negative tests assert the specific denial**, never merely that an error occurred.
- **Write fewer comments.** Default to none. A comment earns its place by explaining *why*. Doc comments on exported identifiers stay required.
- **Status is written only by its controller; spec only by users. Never put a secret in status.**
- `cmp.Diff(want, got)` for comparisons, reported as got/want. No assertion library.
- Run `make verify` before every commit.

---

### Task 1: Generation toolchain, the `Platform` type, and coverage exclusion

**Files:**
- Modify: `hack/versions.sh` (add `CONTROLLER_GEN_VERSION`, add a `versions_check` entry)
- Modify: `Makefile` (add `generate` target; filter generated files out of the coverage profile)
- Create: `api/v1alpha1/groupversion_info.go`
- Create: `api/v1alpha1/platform_types.go`
- Generated: `api/v1alpha1/zz_generated.deepcopy.go`
- Test: `api/v1alpha1/platform_types_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: package `github.com/rusik69/paas/api/v1alpha1` exporting `GroupVersion schema.GroupVersion`, `AddToScheme func(*runtime.Scheme) error`, and types `Platform`, `PlatformList`, `PlatformSpec`, `PlatformStatus`, `ReleaseRef`, `ReleaseAttempt`, `ReleaseState` with constants `ReleaseCompleted`/`ReleasePartial`. Task 3 embeds the CRD generated from these. Task 5 applies it.

- [ ] **Step 1: Add the controller-gen pin**

In `hack/versions.sh`, add to the go-run tool block that already holds `ACTIONLINT_VERSION`:

```sh
# Matched to k8s.io/* v0.34.1. controller-tools v0.20+ targets k8s 0.35/0.36 and
# generates schemas for an apimachinery we do not run.
CONTROLLER_GEN_VERSION="${CONTROLLER_GEN_VERSION:-v0.19.0}"
```

And in `versions_check`, beside the other `_check_url` lines:

```sh
	_check_url controller-gen "https://github.com/kubernetes-sigs/controller-tools/releases/tag/${CONTROLLER_GEN_VERSION}" || rc=1
```

- [ ] **Step 2: Add the `generate` target**

In `Makefile`, after the `vet-e2e` target:

```make
.PHONY: generate
generate: ## controller-gen: deepcopy methods and CRD manifests
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(call pin,CONTROLLER_GEN_VERSION) \
		object paths=./api/...
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(call pin,CONTROLLER_GEN_VERSION) \
		crd paths=./api/... output:crd:artifacts:config=internal/crd/manifests
```

- [ ] **Step 3: Exclude generated code from the coverage profile**

In `Makefile`, in the `cover` target, insert this line immediately after the `go test` line and before the `go tool cover -func` line:

```make
	@grep -v '/zz_generated\.' coverage.out >coverage.filtered && mv coverage.filtered coverage.out
```

Rationale for the reviewer: generated deepcopy is mechanical and large. Left in, it either drags the percentage down or, once round-trip tests exercise it, inflates it into meaninglessness. `COVERAGE_MIN` itself does not move.

- [ ] **Step 4: Write `groupversion_info.go`**

Hand-rolled, not scaffolded, so that `api/` keeps its apimachinery-only dependency set.

```go
// Package v1alpha1 contains the platform.paas.io API types.
//
// Its dependency set is k8s.io/apimachinery and nothing else: external clients
// import this package, and every dependency added here becomes theirs.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of every type in this package.
var GroupVersion = schema.GroupVersion{Group: "platform.paas.io", Version: "v1alpha1"}

// SchemeBuilder registers this package's types with a runtime.Scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds this package's types to a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Platform{}, &PlatformList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
```

- [ ] **Step 5: Write `platform_types.go`**

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ReleaseState is the outcome of one rollout attempt.
type ReleaseState string

const (
	// ReleaseCompleted means every package in the release reached its target.
	ReleaseCompleted ReleaseState = "Completed"
	// ReleasePartial means the attempt stopped before every package converged.
	ReleasePartial ReleaseState = "Partial"
)

// PlatformSpec pins the platform release the cluster converges on.
type PlatformSpec struct {
	// Version is the platform release to converge on. Changing it is the
	// upgrade; changing it back is the rollback.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Registry is the OCI repository platform artifacts are pulled from.
	// +kubebuilder:validation:Pattern=`^oci://`
	Registry string `json:"registry"`
}

// ReleaseRef is a release as asked for and as resolved.
type ReleaseRef struct {
	// Version is the tag named in the spec.
	Version string `json:"version"`
	// Digest is what that tag resolved to. Without it a rollback to a tag can
	// silently land on different bytes than the ones previously running.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ReleaseAttempt is one entry in the rollout history.
type ReleaseAttempt struct {
	// Version is the tag this attempt targeted.
	Version string `json:"version"`
	// Digest is what that tag resolved to.
	// +optional
	Digest string `json:"digest,omitempty"`
	// State is the outcome of the attempt.
	// +kubebuilder:validation:Enum=Completed;Partial
	State ReleaseState `json:"state"`
	// StartedTime is when the attempt began.
	StartedTime metav1.Time `json:"startedTime"`
	// CompletionTime is when it finished, unset while still running.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// PlatformStatus reports what is actually rolled out.
type PlatformStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Current is the release the cluster is running.
	// +optional
	Current *ReleaseRef `json:"current,omitempty"`

	// History records recent attempts, newest first, capped at ten by the
	// reconciler that writes it.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=10
	History []ReleaseAttempt `json:"history,omitempty"`

	// Conditions are Available, Progressing and Degraded.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Platform pins one platform release for the whole cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.status.current.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Platform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformSpec   `json:"spec,omitempty"`
	Status PlatformStatus `json:"status,omitempty"`
}

// PlatformList is a list of Platform.
//
// +kubebuilder:object:root=true
type PlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Platform `json:"items"`
}
```

Note the singleton CEL rule is deliberately **not** here yet — it arrives test-first in Task 6, once envtest exists to prove it rejects what it should.

- [ ] **Step 6: Write the failing test**

Create `api/v1alpha1/platform_types_test.go`:

```go
package v1alpha1

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestPlatform_DeepCopyIsIndependent(t *testing.T) {
	t.Parallel()

	started := metav1.NewTime(time.Unix(1750000000, 0).UTC())
	done := metav1.NewTime(time.Unix(1750000600, 0).UTC())

	orig := &Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: PlatformSpec{
			Version:  "v1.4.2",
			Registry: "oci://registry.paas.io/paas",
		},
		Status: PlatformStatus{
			ObservedGeneration: 3,
			Current:            &ReleaseRef{Version: "v1.4.2", Digest: "sha256:abc"},
			History: []ReleaseAttempt{{
				Version:        "v1.4.2",
				Digest:         "sha256:abc",
				State:          ReleaseCompleted,
				StartedTime:    started,
				CompletionTime: &done,
			}},
			Conditions: []metav1.Condition{{
				Type:   "Available",
				Status: metav1.ConditionTrue,
				Reason: "RolloutComplete",
			}},
		},
	}

	got := orig.DeepCopy()
	if diff := cmp.Diff(orig, got); diff != "" {
		t.Errorf("DeepCopy differs (-want +got):\n%s", diff)
	}

	// Mutating the copy must not reach the original, which is the entire point
	// of generating these methods rather than assigning the struct.
	got.Status.History[0].State = ReleasePartial
	got.Status.Current.Digest = "sha256:changed"
	if orig.Status.History[0].State != ReleaseCompleted {
		t.Error("mutating the copy's history changed the original")
	}
	if orig.Status.Current.Digest != "sha256:abc" {
		t.Error("mutating the copy's current digest changed the original")
	}
}

func TestPlatform_AddToScheme(t *testing.T) {
	t.Parallel()

	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if !s.Recognizes(GroupVersion.WithKind("Platform")) {
		t.Errorf("scheme does not recognize %s", GroupVersion.WithKind("Platform"))
	}
	if !s.Recognizes(GroupVersion.WithKind("PlatformList")) {
		t.Errorf("scheme does not recognize %s", GroupVersion.WithKind("PlatformList"))
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `go test ./api/...`
Expected: FAIL — `orig.DeepCopy undefined (type *Platform has no field or method DeepCopy)`. The deepcopy methods do not exist until controller-gen runs.

- [ ] **Step 8: Add go-cmp and generate**

```bash
go get github.com/google/go-cmp@latest
make generate
```

Expected: `api/v1alpha1/zz_generated.deepcopy.go` is created and `internal/crd/manifests/platform.paas.io_platforms.yaml` appears.

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./api/... -v`
Expected: PASS for both `TestPlatform_DeepCopyIsIndependent` and `TestPlatform_AddToScheme`.

- [ ] **Step 10: Verify the dependency rule holds**

Run:

```bash
go list -deps ./api/v1alpha1 | grep -E '^(sigs\.k8s\.io|k8s\.io)/' | grep -v '^k8s.io/apimachinery' | grep -v '^k8s.io/klog' | grep -v '^k8s.io/utils'
```

Expected: no output. Any line here is a dependency `api/` must not have. (`klog` and `utils` arrive transitively through apimachinery and are unavoidable.)

- [ ] **Step 11: Run the full gate and commit**

```bash
make verify && make versions
git add hack/versions.sh Makefile api/ go.mod go.sum
git commit -m "Add the platform.paas.io API group and the Platform type

Hand-rolled scheme registration rather than the kubebuilder scaffold: the
scaffold imports controller-runtime, and api/ is limited to apimachinery
because external clients import it.

Generated deepcopy is excluded from the coverage profile. It is mechanical
and large enough to dominate the number in either direction, and the floor
is there to measure code someone wrote."
```

---

### Task 2: CRD manifests, embedded and verified

**Files:**
- Create: `internal/crd/crd.go`
- Test: `internal/crd/crd_test.go`
- Generated: `internal/crd/manifests/platform.paas.io_platforms.yaml` (already produced in Task 1)
- Modify: `.github/workflows/ci.yml:57-60` (extend the generated-files gate)

**Interfaces:**
- Consumes: the generated CRD YAML from Task 1.
- Produces: `crd.Load() ([]*apiextensionsv1.CustomResourceDefinition, error)` and `const FieldManager = "paas-operator/crd"`, both used by Task 5.

- [ ] **Step 1: Write the failing test**

Create `internal/crd/crd_test.go`:

```go
package crd

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// The embed pattern silently matching nothing is the failure this guards:
// it produces a binary that installs zero CRDs and reports success.
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

	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/crd/...`
Expected: FAIL — the package does not compile, `undefined: Load`.

- [ ] **Step 3: Write `crd.go`**

```go
// Package crd installs the CustomResourceDefinitions this operator owns.
//
// The manifests are embedded rather than fetched so that a binary carries the
// exact schemas it was built against. A cluster running an old operator with
// new CRDs, or the reverse, is the stale-CRD problem this exists to remove.
package crd

import (
	"fmt"
	"io/fs"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"embed"
)

// FieldManager owns every field this package applies.
//
// This string is API. Changing it orphans field ownership on every CRD in
// every cluster already running, which surfaces as fields nothing will ever
// update again.
const FieldManager = "paas-operator/crd"

//go:embed manifests/*.yaml
var manifests embed.FS

// Load parses the embedded CRD manifests.
func Load() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	entries, err := fs.Glob(manifests, "manifests/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("glob embedded manifests: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no embedded CRD manifests: the embed pattern matched nothing")
	}

	out := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(entries))
	for _, name := range entries {
		b, err := manifests.ReadFile(name)
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
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go get k8s.io/apiextensions-apiserver@v0.34.1
go get sigs.k8s.io/yaml
go test ./internal/crd/... -v
```

Expected: PASS for both tests.

- [ ] **Step 5: Extend the CI generated-files gate**

In `.github/workflows/ci.yml`, replace the body of the "generated files are current" step:

```yaml
      - name: generated files are current
        run: |
          go mod tidy
          make generate
          git diff --exit-code
```

Rationale: a generated file that disagrees with its source is a build that lies about what it contains.

- [ ] **Step 6: Run the full gate and commit**

```bash
make verify && make actionlint
git add internal/crd .github/workflows/ci.yml go.mod go.sum
git commit -m "Embed the generated CRDs and prove the embed is not empty

An //go:embed pattern that matches nothing compiles, ships, installs zero
CRDs and reports success. Load fails loudly instead, and a test holds it to
that.

The CI generated-files gate now runs make generate, not just go mod tidy."
```

---

### Task 3: The envtest tier

**Files:**
- Modify: `hack/versions.sh` (add `SETUP_ENVTEST_VERSION`, add a `versions_check` entry)
- Modify: `Makefile` (add `test-integration` and `vet-integration`; add `vet-integration` to `verify`)
- Modify: `hack/deps.sh` (report setup-envtest assets in `check`)
- Modify: `.github/workflows/ci.yml` (add an `integration` job)
- Create: `internal/crd/suite_integration_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: a running envtest API server available to integration tests as package-level `restCfg *rest.Config` in package `crd`, plus `k8sClient client.Client`. Tasks 5 and 6 use both.

- [ ] **Step 1: Add the setup-envtest pin**

In `hack/versions.sh`, beside the other go-run tool pins:

```sh
# The tool is versioned independently of the assets it downloads; the assets
# version is ENVTEST_K8S_VERSION in the Makefile and tracks the cluster.
SETUP_ENVTEST_VERSION="${SETUP_ENVTEST_VERSION:-v0.24.1}"
```

In `versions_check`:

```sh
	_check_url setup-envtest "https://github.com/kubernetes-sigs/controller-runtime/releases/tag/${SETUP_ENVTEST_VERSION}" || rc=1
```

- [ ] **Step 2: Add the make targets**

In `Makefile`, next to `E2E_TIMEOUT`:

```make
INTEGRATION_TIMEOUT ?= 10m
# Tracks the cluster Kubernetes version; envtest serves the same API surface.
ENVTEST_K8S_VERSION ?= 1.34.x
```

And next to `vet-e2e`:

```make
.PHONY: vet-integration
vet-integration: ## The envtest suite is behind a build tag and invisible to `make test`
	$(GO) vet -tags integration ./...

.PHONY: test-integration
test-integration: ## envtest — real apiserver, no kubelet
	KUBEBUILDER_ASSETS="$$($(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(call pin,SETUP_ENVTEST_VERSION) use -p path $(ENVTEST_K8S_VERSION))" \
		$(GO) test -tags integration -race -timeout $(INTEGRATION_TIMEOUT) ./...
```

Change the `verify` target line to include `vet-integration`:

```make
verify: vet vet-e2e vet-integration lint cover check-stdout ## Everything that must pass before pushing
```

- [ ] **Step 3: Write the envtest harness**

Create `internal/crd/suite_integration_test.go`:

```go
//go:build integration

package crd

import (
	"fmt"
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/rusik69/paas/api/v1alpha1"
)

var (
	restCfg   *rest.Config
	k8sClient client.Client
	scheme    *runtime.Scheme
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\nrun 'make test-integration', which sets KUBEBUILDER_ASSETS\n", err)
		os.Exit(1)
	}
	restCfg = cfg

	scheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add client-go scheme: %v\n", err)
		os.Exit(1)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add apiextensions scheme: %v\n", err)
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add v1alpha1 scheme: %v\n", err)
		os.Exit(1)
	}

	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// Proves the harness itself works before anything depends on it.
func TestEnvtest_ServesTheAPI(t *testing.T) {
	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := k8sClient.List(t.Context(), crds); err != nil {
		t.Fatalf("list crds against envtest: %v", err)
	}
}
```

- [ ] **Step 4: Run it to verify it passes**

```bash
go get sigs.k8s.io/controller-runtime@v0.22.5
make vet-integration
make test-integration
```

Expected: `TestEnvtest_ServesTheAPI` PASSes. First run downloads the 1.34.x control-plane binaries, which takes a minute.

- [ ] **Step 5: Report envtest assets in `hack/deps.sh`**

In `hack/deps.sh`, inside `check()`, beside the other tool rows, add a row that reports whether the envtest assets are already fetched. Match the existing row formatting exactly — read the neighbouring lines and copy their `printf` shape rather than inventing one.

The check itself:

```sh
	if [[ -n "$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@"${SETUP_ENVTEST_VERSION}" list -i --output json 2>/dev/null)" ]]; then
		# assets present
	fi
```

- [ ] **Step 6: Add the CI job**

In `.github/workflows/ci.yml`, after the `unit` job:

```yaml
  # envtest runs a real kube-apiserver and etcd with no kubelet. It is separate
  # from unit because it needs downloaded control-plane binaries and minutes
  # rather than seconds — but it is not e2e: no cluster, no nodes.
  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: envtest suite
        run: make test-integration
```

- [ ] **Step 7: Run the full gate and commit**

```bash
make verify && make actionlint && make versions
git add hack/versions.sh hack/deps.sh Makefile .github/workflows/ci.yml internal/crd go.mod go.sum
git commit -m "Add the envtest tier

testing.md has budgeted for this since phase 0: a real apiserver and etcd,
no kubelet, for the validation and server-side-apply behaviour the fake
client only approximates. Behind a build tag so make test stays under ten
seconds, with vet-integration in verify so the tagged code cannot rot
uncompiled."
```

---

### Task 4: The applier

**Files:**
- Create: `internal/crd/apply.go`
- Test: `internal/crd/apply_integration_test.go`

**Interfaces:**
- Consumes: `Load()` and `FieldManager` from Task 2; `k8sClient`, `restCfg` from Task 3.
- Produces: `crd.Apply(ctx context.Context, c client.Client) error`, used by Task 7.

- [ ] **Step 1: Write the failing test**

Create `internal/crd/apply_integration_test.go`:

```go
//go:build integration

package crd

import (
	"context"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
)

func establishedCRD(t *testing.T, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get crd %s: %v", name, err)
	}
	return got
}

func TestApply_InstallsAndEstablishesEveryCRD(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, w := range want {
		got := establishedCRD(t, w.Name)
		var established bool
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		if !established {
			t.Errorf("crd %s is not Established: %+v", w.Name, got.Status.Conditions)
		}
	}
}

// The level-triggered claim, tested rather than asserted.
func TestApply_IsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before := establishedCRD(t, "platforms.platform.paas.io")

	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	after := establishedCRD(t, "platforms.platform.paas.io")

	if before.Generation != after.Generation {
		t.Errorf("generation moved %d -> %d on a no-op apply", before.Generation, after.Generation)
	}
}

// Without Force, one kubectl edit wedges the operator on a conflict it can
// never resolve. This is that scenario.
func TestApply_OverwritesForeignEdits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	drifted := establishedCRD(t, "platforms.platform.paas.io")
	drifted.Spec.Names.ShortNames = []string{"tamper"}
	if err := k8sClient.Update(ctx, drifted); err != nil {
		t.Fatalf("simulate a manual edit: %v", err)
	}

	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("Apply after drift: %v", err)
	}

	got := establishedCRD(t, "platforms.platform.paas.io")
	for _, s := range got.Spec.Names.ShortNames {
		if s == "tamper" {
			t.Error("the manual edit survived; the apply is not forcing ownership")
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test-integration`
Expected: FAIL — `undefined: Apply`.

- [ ] **Step 3: Write `apply.go`**

```go
package crd

import (
	"context"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rusik69/paas/pkg/wait"
)

// Apply installs every embedded CRD and waits for each to be Established.
//
// It is level-triggered: running it against an up-to-date cluster changes
// nothing. Ownership is forced, because the operator owns these objects
// completely and a conflict it cannot resolve would wedge it permanently.
func Apply(ctx context.Context, c client.Client) error {
	crds, err := Load()
	if err != nil {
		return err
	}

	for _, crd := range crds {
		obj := crd.DeepCopy()
		obj.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
		if err := c.Patch(ctx, obj, client.Apply,
			client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply crd %s: %w", crd.Name, err)
		}
	}

	for _, crd := range crds {
		if err := waitEstablished(ctx, c, crd.Name); err != nil {
			return err
		}
	}
	return nil
}

func waitEstablished(ctx context.Context, c client.Client, name string) error {
	return wait.For(ctx, time.Second, "crd "+name+" Established", func(ctx context.Context) (bool, error) {
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
			return false, nil
		}
		for _, cond := range got.Status.Conditions {
			if cond.Type == apiextensionsv1.NamesAccepted && cond.Status == apiextensionsv1.ConditionFalse {
				return false, fmt.Errorf("crd %s names rejected: %s", name, cond.Message)
			}
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `make test-integration`
Expected: PASS for `TestApply_InstallsAndEstablishesEveryCRD`, `TestApply_IsIdempotent`, `TestApply_OverwritesForeignEdits`.

- [ ] **Step 5: Run the full gate and commit**

```bash
make verify && make test-integration
git add internal/crd
git commit -m "Apply the embedded CRDs, forcing ownership

Server-side apply under a frozen field manager, then wait for Established
rather than assuming the apply took: a CRD whose names are rejected stays
absent while the apply reports success.

Force is deliberate. The operator owns these objects completely, and without
it a single kubectl edit leaves the operator wedged on a conflict it can
never resolve on its own."
```

---

### Task 5: The singleton rule and its denial

**Files:**
- Modify: `api/v1alpha1/platform_types.go` (add the CEL marker to `Platform`)
- Test: `internal/crd/platform_validation_integration_test.go`
- Regenerated: `api/v1alpha1/zz_generated.deepcopy.go`, `internal/crd/manifests/platform.paas.io_platforms.yaml`

**Interfaces:**
- Consumes: `Apply` from Task 4, `k8sClient` from Task 3.
- Produces: nothing new; constrains the existing `Platform` type.

- [ ] **Step 1: Write the failing test**

Create `internal/crd/platform_validation_integration_test.go`:

```go
//go:build integration

package crd

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/v1alpha1"
)

func installCRDs(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := Apply(ctx, k8sClient); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestPlatform_AcceptsTheSingletonName(t *testing.T) {
	installCRDs(t)

	p := &v1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: v1alpha1.PlatformSpec{
			Version:  "v1.4.2",
			Registry: "oci://registry.paas.io/paas",
		},
	}
	if err := k8sClient.Create(t.Context(), p); err != nil {
		t.Fatalf("create the singleton Platform: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(context.Background(), p); err != nil {
			t.Logf("cleanup: delete platform: %v", err)
		}
	})
}

// Asserts the specific denial. A test that accepts any error keeps passing
// after the rule it guards is deleted.
func TestPlatform_RejectsAnyOtherName(t *testing.T) {
	installCRDs(t)

	p := &v1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "notcluster"},
		Spec: v1alpha1.PlatformSpec{
			Version:  "v1.4.2",
			Registry: "oci://registry.paas.io/paas",
		},
	}
	err := k8sClient.Create(t.Context(), p)
	if err == nil {
		t.Fatal("a second Platform name was accepted; the singleton rule is not in effect")
	}
	if !strings.Contains(err.Error(), "must be named cluster") {
		t.Errorf("error = %q, want it to name the singleton rule", err)
	}
}

func TestPlatform_RejectsARegistryThatIsNotOCI(t *testing.T) {
	installCRDs(t)

	p := &v1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: v1alpha1.PlatformSpec{
			Version:  "v1.4.2",
			Registry: "https://registry.paas.io/paas",
		},
	}
	err := k8sClient.Create(t.Context(), p)
	if err == nil {
		t.Fatal("a non-oci:// registry was accepted")
	}
	if !strings.Contains(err.Error(), "spec.registry") {
		t.Errorf("error = %q, want it to name spec.registry", err)
	}
}
```

- [ ] **Step 2: Run to verify the singleton test fails**

Run: `make test-integration`
Expected: `TestPlatform_RejectsAnyOtherName` FAILs with "a second Platform name was accepted". The other two should already pass.

- [ ] **Step 3: Add the CEL marker**

In `api/v1alpha1/platform_types.go`, add one marker to the `Platform` doc-comment block, directly above `+kubebuilder:object:root=true`:

```go
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="the Platform is a singleton and must be named cluster"
```

- [ ] **Step 4: Regenerate and re-run**

```bash
make generate
make test-integration
```

Expected: all three tests PASS.

- [ ] **Step 5: Run the full gate and commit**

```bash
make verify && make test-integration
git add api/ internal/crd
git commit -m "Constrain Platform to a single object named cluster

architecture.md pins the platform version in a single Platform CR. An API
that permits two invites a split brain nothing downstream is designed to
resolve, so the API server rejects it rather than a reconciler discovering
it later.

The negative test asserts the specific message, not merely that an error
happened."
```

---

### Task 6: `PackageSource` and `Package`

**Files:**
- Create: `api/v1alpha1/packagesource_types.go`
- Create: `api/v1alpha1/package_types.go`
- Modify: `api/v1alpha1/groupversion_info.go` (register the new kinds)
- Modify: `internal/crd/crd_test.go` (the expected-CRD list grows)
- Test: `internal/crd/package_validation_integration_test.go`
- Regenerated: deepcopy and two new manifests

**Interfaces:**
- Consumes: `GroupVersion`, `AddToScheme` from Task 1; `Apply` from Task 4.
- Produces: types `PackageSource`, `PackageSourceList`, `PackageSourceSpec`, `PackageSourceStatus`, `ArtifactRef`, `Package`, `PackageList`, `PackageSpec`, `PackageStatus`, `PackageStage` with constants `StageMigration`/`StageComponent`.

- [ ] **Step 1: Write the failing validation test**

Create `internal/crd/package_validation_integration_test.go`:

```go
//go:build integration

package crd

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/api/v1alpha1"
)

func TestPackage_AcceptsBothStages(t *testing.T) {
	installCRDs(t)

	for _, stage := range []v1alpha1.PackageStage{v1alpha1.StageMigration, v1alpha1.StageComponent} {
		t.Run(string(stage), func(t *testing.T) {
			p := &v1alpha1.Package{
				ObjectMeta: metav1.ObjectMeta{Name: "cilium-" + strings.ToLower(string(stage))},
				Spec: v1alpha1.PackageSpec{
					SourceRef: v1alpha1.LocalRef{Name: "platform"},
					Chart:     "cilium",
					Version:   "1.18.12",
					Stage:     stage,
				},
			}
			if err := k8sClient.Create(t.Context(), p); err != nil {
				t.Fatalf("create package with stage %s: %v", stage, err)
			}
			t.Cleanup(func() {
				if err := k8sClient.Delete(context.Background(), p); err != nil {
					t.Logf("cleanup: delete package: %v", err)
				}
			})
		})
	}
}

func TestPackage_RejectsAnUnknownStage(t *testing.T) {
	installCRDs(t)

	p := &v1alpha1.Package{
		ObjectMeta: metav1.ObjectMeta{Name: "cilium-bogus"},
		Spec: v1alpha1.PackageSpec{
			SourceRef: v1alpha1.LocalRef{Name: "platform"},
			Chart:     "cilium",
			Version:   "1.18.12",
			Stage:     v1alpha1.PackageStage("bogus"),
		},
	}
	err := k8sClient.Create(t.Context(), p)
	if err == nil {
		t.Fatal("an unknown stage was accepted; the enum is not in effect")
	}
	if !strings.Contains(err.Error(), "spec.stage") {
		t.Errorf("error = %q, want it to name spec.stage", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `make test-integration`
Expected: FAIL to compile — `undefined: v1alpha1.Package`.

- [ ] **Step 3: Write `packagesource_types.go`**

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LocalRef names another object in the same scope.
type LocalRef struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ArtifactRef is a resolved OCI artifact.
type ArtifactRef struct {
	// Revision is the tag and digest it resolved to, as tag@sha256:...
	// +optional
	Revision string `json:"revision,omitempty"`
	// Digest is the artifact digest.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// PackageSourceSpec is an OCI repository packages are pulled from.
type PackageSourceSpec struct {
	// URL is the OCI repository, without a tag.
	// +kubebuilder:validation:Pattern=`^oci://`
	URL string `json:"url"`

	// Interval between polls of the repository.
	// +kubebuilder:default="5m"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// SecretRef names a docker-registry Secret for authentication.
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`

	// Insecure permits plain HTTP. The in-cluster phase-0 registry speaks it,
	// which is why no CA has to be distributed to every node.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// PackageSourceStatus reports what the source last resolved to.
type PackageSourceStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Artifact is the most recently resolved artifact.
	// +optional
	Artifact *ArtifactRef `json:"artifact,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PackageSource is an OCI repository packages are pulled from.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.artifact.revision`
type PackageSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackageSourceSpec   `json:"spec,omitempty"`
	Status PackageSourceStatus `json:"status,omitempty"`
}

// PackageSourceList is a list of PackageSource.
//
// +kubebuilder:object:root=true
type PackageSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PackageSource `json:"items"`
}
```

- [ ] **Step 4: Write `package_types.go`**

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PackageStage orders a rollout. Migrations land before components, so a
// component never starts against state that has not been migrated yet.
type PackageStage string

const (
	// StageMigration runs first and must complete before StageComponent starts.
	StageMigration PackageStage = "migration"
	// StageComponent is the component itself.
	StageComponent PackageStage = "component"
)

// PackageSpec is one component at one version.
type PackageSpec struct {
	// SourceRef names the PackageSource this chart is pulled from.
	SourceRef LocalRef `json:"sourceRef"`

	// Chart is the chart name within the source.
	// +kubebuilder:validation:MinLength=1
	Chart string `json:"chart"`

	// Version is the chart version.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Stage orders this package within a rollout.
	// +kubebuilder:validation:Enum=migration;component
	Stage PackageStage `json:"stage"`

	// Values are passed to the chart. Constrained by the chart's own
	// values.schema.json, which is not available at this layer.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *runtime.RawExtension `json:"values,omitempty"`
}

// PackageStatus reports what was applied.
type PackageStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AppliedDigest is the artifact digest last applied.
	// +optional
	AppliedDigest string `json:"appliedDigest,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Package is one component of a platform release.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Chart",type=string,JSONPath=`.spec.chart`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.spec.stage`
type Package struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackageSpec   `json:"spec,omitempty"`
	Status PackageStatus `json:"status,omitempty"`
}

// PackageList is a list of Package.
//
// +kubebuilder:object:root=true
type PackageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Package `json:"items"`
}
```

- [ ] **Step 5: Register the new kinds**

In `api/v1alpha1/groupversion_info.go`, replace the `AddKnownTypes` call:

```go
	s.AddKnownTypes(GroupVersion,
		&Platform{}, &PlatformList{},
		&PackageSource{}, &PackageSourceList{},
		&Package{}, &PackageList{},
	)
```

- [ ] **Step 6: Update the expected-CRD list**

In `internal/crd/crd_test.go`, in `TestLoad_ReturnsEveryEmbeddedCRD`, replace the `want` slice:

```go
	want := []string{
		"packages.platform.paas.io",
		"packagesources.platform.paas.io",
		"platforms.platform.paas.io",
	}
```

- [ ] **Step 7: Regenerate and run**

```bash
make generate
go test ./api/... ./internal/crd/...
make test-integration
```

Expected: all PASS, including both new validation tests.

- [ ] **Step 8: Verify the dependency rule still holds**

Run:

```bash
go list -deps ./api/v1alpha1 | grep -E '^(sigs\.k8s\.io|k8s\.io)/' | grep -v '^k8s.io/apimachinery' | grep -v '^k8s.io/klog' | grep -v '^k8s.io/utils'
```

Expected: no output.

- [ ] **Step 9: Run the full gate and commit**

```bash
make verify && make test-integration
git add api/ internal/crd
git commit -m "Add PackageSource and Package

stage is an enum rather than an isMigration bool: go-guidelines calls for
enums wherever a third state is imaginable, and it puts the two-stage
ordering in kubectl get package rather than leaving it inferable only from
reconciler source.

values stays a RawExtension. The schema that constrains it lives in the
chart's values.schema.json, which arrives with the publishing pipeline."
```

---

### Task 7: `cmd/paas-operator`

**Files:**
- Create: `cmd/paas-operator/main.go`
- Modify: `docs/roadmap.md` (mark what this phase-1 bullet delivered)

**Interfaces:**
- Consumes: `crd.Apply` from Task 4.
- Produces: the `paas-operator` binary.

- [ ] **Step 1: Write `main.go`**

Thin, per go-guidelines: flag wiring and a call into `internal/`.

```go
// Command paas-operator installs the platform CRDs and reconciles Platform.
//
// Today it does the first half only: the reconcilers arrive with the next
// phase-1 increment.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/rusik69/paas/internal/crd"
)

func main() {
	timeout := flag.Duration("crd-install-timeout", 2*time.Minute,
		"how long to wait for every CRD to become Established")
	flag.Parse()

	if err := run(*timeout); err != nil {
		fmt.Fprintf(os.Stderr, "paas-operator: %v\n", err)
		os.Exit(1)
	}
}

func run(timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := crd.Apply(ctx, c); err != nil {
		return fmt.Errorf("install CRDs: %w", err)
	}
	fmt.Println("CRDs installed and established")
	return nil
}
```

- [ ] **Step 2: Verify it builds and runs against envtest**

```bash
go build ./cmd/paas-operator
```

Expected: builds clean. Do not run it against the e2e cluster in this task; Task 4's integration tests already prove the apply path.

- [ ] **Step 3: Record what landed**

In `docs/roadmap.md`, under Phase 1, leave the bullet text alone but append one sentence to the `api/v1alpha1` bullet noting the reconcilers are still outstanding, so the phase's remaining work stays visible:

```markdown
- `api/v1alpha1` scaffolding; CRDs embedded in the operator binary and applied on every start.
  *(Types, generation, embedding and the applier landed; the reconcilers below are outstanding.)*
```

- [ ] **Step 4: Run the full gate and commit**

```bash
make verify && make test-integration
git add cmd/ docs/roadmap.md
git commit -m "Add the paas-operator binary

Installs its own CRDs on every start and exits. That is the whole job until
the Platform reconciler lands: a binary that converges the schemas it was
built against is what removes the stale-CRD problem, and it is worth having
before anything depends on it."
```

---

## Self-Review

**Spec coverage.** Every section of the design maps to a task: the three types (1, 6), singleton CEL (5), `stage` enum (6), asked-for/resolved split (1, `ReleaseRef`), history cap (1, `MaxItems=10`), controller-gen wiring and pin (1), manifests location and embedding (2), CI generated-files gate (2), field manager and `Force: true` (2, 4), unit tier (1, 2), envtest tier and its CI job (3), the five integration cases (4, 5, 6), coverage exclusion (1), `cmd/paas-operator` (7).

**Deliberate ordering divergence from the spec.** The spec lists the three types together; this plan builds `Platform` first (Task 1), proves the whole pipeline on it, then adds the other two cheaply in Task 6. Validation markers are added test-first in Tasks 5 and 6, after envtest exists, rather than written blind in Task 1.

**Known gap, deliberately left.** `PackageSource` and `Package` get validation tests but no deepcopy round-trip tests of their own — Task 1's round-trip covers the generation mechanism, and repeating it per type tests controller-gen rather than our code.
