package flux

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Guards the --components flag in `make vendor-flux`. Widening it is a one-word
// change that silently doubles the installed surface, and nothing else here
// would notice.
func TestLoad_InstallsOnlyTheTwoControllersWeWant(t *testing.T) {
	t.Parallel()

	objs := objects

	var deployments []string
	for _, o := range objs {
		if o.GetKind() == "Deployment" {
			deployments = append(deployments, o.GetName())
		}
	}

	want := []string{"helm-controller", "source-controller"}
	less := func(a, b string) bool { return a < b }
	if diff := cmp.Diff(want, deployments, cmpopts.SortSlices(less)); diff != "" {
		t.Errorf("installed controllers differ (-want +got):\n%s", diff)
	}
}

func TestLoad_EverythingNamespacedLandsInFluxSystem(t *testing.T) {
	t.Parallel()

	objs := objects

	for _, o := range objs {
		if ns := o.GetNamespace(); ns != "" && ns != Namespace {
			t.Errorf("%s/%s is in namespace %q, want %q",
				o.GetKind(), o.GetName(), ns, Namespace)
		}
	}
}

// The CRDs the reconcilers write. Absent, the Platform reconciler's applies
// fail at runtime with a no-matches error rather than here.
func TestLoad_CarriesTheCRDsTheReconcilersWrite(t *testing.T) {
	t.Parallel()

	objs := objects

	got := map[string]bool{}
	for _, o := range objs {
		if o.GetKind() == "CustomResourceDefinition" {
			got[o.GetName()] = true
		}
	}

	for _, want := range []string{
		"ocirepositories.source.toolkit.fluxcd.io",
		"helmreleases.helm.toolkit.fluxcd.io",
	} {
		if !got[want] {
			t.Errorf("vendored manifests do not carry %s", want)
		}
	}
}

func TestLoad_EmptyManifestsAreAnError(t *testing.T) {
	t.Parallel()

	if _, err := load(nil); !errors.Is(err, ErrNoManifests) {
		t.Errorf("err = %v, want ErrNoManifests — an empty vendor dir must not read as success", err)
	}
}

func TestLoad_MalformedManifestsAreReported(t *testing.T) {
	t.Parallel()

	_, err := load([]byte("kind: [unterminated\n"))
	if err == nil {
		t.Fatal("malformed vendored manifests were accepted")
	}
	if !strings.Contains(err.Error(), "vendored flux manifests") {
		t.Errorf("err = %q, want it to name what failed to parse", err)
	}
}

// Without the selector both controllers watch every object, and adding a shard
// later means every existing object is briefly watched twice or not at all.
func TestLoad_ControllersAreConfinedToTheDefaultShard(t *testing.T) {
	t.Parallel()

	for _, o := range objects {
		if o.GetKind() != "Deployment" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			t.Fatalf("deployment %s has no containers: %v", o.GetName(), err)
		}
		for i, c := range containers {
			args, _, err := unstructured.NestedStringSlice(c.(map[string]any), "args")
			if err != nil {
				t.Fatalf("deployment %s container %d args: %v", o.GetName(), i, err)
			}
			if !slices.Contains(args, shardFlag) {
				t.Errorf("deployment %s container %d args = %v, want it to contain %q",
					o.GetName(), i, args, shardFlag)
			}
		}
	}
}

// Load is called once at init, but the patch must be safe to repeat: a second
// pass adding the flag twice would make the deployment churn on every apply.
func TestApplyShardSelector_IsIdempotent(t *testing.T) {
	t.Parallel()

	objs, err := load(manifests)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := applyShardSelector(objs); err != nil {
		t.Fatalf("second applyShardSelector: %v", err)
	}

	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			args, _, _ := unstructured.NestedStringSlice(c.(map[string]any), "args")
			var n int
			for _, a := range args {
				if a == shardFlag {
					n++
				}
			}
			if n != 1 {
				t.Errorf("deployment %s carries the shard flag %d times, want 1", o.GetName(), n)
			}
		}
	}
}

// Through load, so the propagation out of it is covered too: manifests that
// cannot be sharded must not install as if they had been.
func TestLoad_RejectsManifestsWithNoDeployment(t *testing.T) {
	t.Parallel()

	_, err := load([]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: flux-system\n"))
	if err == nil {
		t.Error("manifests with no Deployment were accepted; the controllers would be unsharded")
	}
}

// A vendored file whose Deployments are shaped unexpectedly must fail loudly.
// Silently skipping one would install an unsharded controller, and nothing
// downstream would say so.
func TestApplyShardSelector_RejectsMalformedDeployments(t *testing.T) {
	t.Parallel()

	deployment := func(spec map[string]any) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "source-controller"},
		}}
		if spec != nil {
			o.Object["spec"] = spec
		}
		return o
	}

	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want string
	}{
		{
			name: "no spec",
			obj:  deployment(nil),
			want: "no spec",
		},
		{
			name: "no template",
			obj:  deployment(map[string]any{}),
			want: "no template",
		},
		{
			name: "no template spec",
			obj:  deployment(map[string]any{"template": map[string]any{}}),
			want: "no template spec",
		},
		{
			name: "containers are not a list",
			obj: deployment(map[string]any{"template": map[string]any{
				"spec": map[string]any{"containers": "nope"},
			}}),
			want: "has no containers",
		},
		{
			name: "args are not a list",
			obj: deployment(map[string]any{"template": map[string]any{
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": "manager", "args": "nope"},
				}},
			}}),
			want: "args are not a list",
		},
		{
			name: "container is not a map",
			obj: deployment(map[string]any{"template": map[string]any{
				"spec": map[string]any{"containers": []any{"not-a-map"}},
			}}),
			want: "malformed",
		},
		{
			name: "args are not strings",
			// int64, not int: unstructured panics on any Go type that is not a
			// JSON one, so the malformed case has to stay inside that set to
			// exercise the error path rather than crash before reaching it.
			obj: deployment(map[string]any{"template": map[string]any{
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": "manager", "args": []any{int64(1)}},
				}},
			}}),
			want: "non-string arg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := applyShardSelector([]*unstructured.Unstructured{tc.obj})
			if err == nil {
				t.Fatalf("accepted a deployment with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
