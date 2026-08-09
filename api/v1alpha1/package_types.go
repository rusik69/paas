package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PackageStage orders a rollout. Migrations land before components, so no
// component ever starts against state that has not been migrated yet.
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

	// Values are passed to the chart, constrained by the chart's own
	// values.schema.json rather than by anything here.
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
