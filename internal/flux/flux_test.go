package flux

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
