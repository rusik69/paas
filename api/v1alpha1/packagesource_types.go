package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LocalRef names another cluster-scoped object.
type LocalRef struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ArtifactRef is a resolved OCI artifact.
type ArtifactRef struct {
	// Revision is what the tag resolved to, as tag@sha256:...
	// +optional
	Revision string `json:"revision,omitempty"`

	// Digest is the artifact digest on its own.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// PackageSourceSpec is an OCI repository packages are pulled from.
type PackageSourceSpec struct {
	// URL is the OCI repository, without a tag.
	// +kubebuilder:validation:Pattern=`^oci://`
	URL string `json:"url"`

	// Interval between polls of the repository.
	//
	// A pointer because metav1.Duration is a struct: omitempty never omits one,
	// so a value field ships an explicit "0s" and the default below never
	// applies.
	// +kubebuilder:default="5m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// SecretRef names a docker-registry Secret for authentication.
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`

	// Insecure permits plain HTTP. The in-cluster registry speaks it, which is
	// why no CA has to be distributed to every node.
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
