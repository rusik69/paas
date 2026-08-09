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

	// Digest is what that tag resolved to. Without it, a rollback to a tag can
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
