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
	//
	// Immutable: it names the CRD this class serves, so editing it would leave
	// the previous kind's CRD serving with a controller nothing can reach to
	// stop. Renaming a service is a migration, not an update.
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kind is immutable"
	Kind string `json:"kind"`

	// Plural is the lowercase plural used in the resource path.
	//
	// Immutable, for the reason kind is.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="plural is immutable"
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
