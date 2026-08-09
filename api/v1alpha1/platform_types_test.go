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
	for _, kind := range []string{"Platform", "PlatformList"} {
		if !s.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize %s", GroupVersion.WithKind(kind))
		}
	}
}
