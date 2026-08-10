package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Plan sizes a tenant's quota and gates its features.
type Plan string

const (
	// PlanTrial is the smallest tier.
	PlanTrial Plan = "trial"
	// PlanBusiness is the default paid tier.
	PlanBusiness Plan = "business"
	// PlanEnterprise is the largest tier.
	PlanEnterprise Plan = "enterprise"
)

// Isolation is how strongly a tenant's workloads are separated from others'.
type Isolation string

const (
	// IsolationShared packs tenants onto shared nodes.
	IsolationShared Isolation = "shared"
	// IsolationDedicatedNodes confines a tenant to its own node pool. The
	// compliance escape hatch, not the default.
	IsolationDedicatedNodes Isolation = "dedicated-nodes"
)

// Module is tenant-level infrastructure enabled at a node and inherited by its
// descendants.
type Module struct {
	// Enabled turns the module on for this tenant and its descendants.
	//
	// False is not a denial: resolution walks up to the nearest ancestor that
	// has it enabled, so false on a child means "use my parent's".
	Enabled bool `json:"enabled"`
}

// TenantSpec is the tenant's declared shape.
type TenantSpec struct {
	// Plan drives the quota and limits applied to this tenant's namespace.
	// +kubebuilder:validation:Enum=trial;business;enterprise
	Plan Plan `json:"plan"`

	// Isolation is immutable: changing it would move every workload between
	// node pools, which is a migration rather than an update.
	// +kubebuilder:validation:Enum=shared;dedicated-nodes
	// +kubebuilder:default=shared
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="isolation is immutable; moving a tenant between node pools is a migration"
	// +optional
	Isolation Isolation `json:"isolation,omitempty"`

	// Host is the wildcard domain this tenant's apps are published under.
	// Unused until the app plane exists.
	// +optional
	Host string `json:"host,omitempty"`

	// Modules enables tenant-level infrastructure, keyed by module name.
	//
	// A map rather than a list, so a child overriding one module under
	// server-side apply cannot replace the whole set.
	// +optional
	Modules map[string]Module `json:"modules,omitempty"`

	// Admins are the identities granted tenant-admin over this tenant and its
	// descendants.
	// +optional
	// +listType=atomic
	Admins []string `json:"admins,omitempty"`
}

// TenantStatus reports what the tenant resolved to.
type TenantStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Namespace is the path-derived namespace backing this tenant. Reported
	// because it is derived rather than declared, and everything a tenant owns
	// lives in it.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Modules reports where each enabled module resolved to, keyed by module
	// name. A child inheriting its parent's monitoring shows the parent here.
	//
	// Reported because resolution walks the ancestor chain and is otherwise
	// invisible: "which monitoring stack serves this tenant" is the question
	// ADR 0004 warns will be answered inconsistently, and this is the answer
	// the platform actually used.
	// +optional
	Modules map[string]ModuleStatus `json:"modules,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ModuleStatus is where one module resolved to.
type ModuleStatus struct {
	// Tenant is the name of the tenant providing the module — this one, or the
	// nearest ancestor with it enabled.
	Tenant string `json:"tenant"`

	// Namespace is where that tenant's module runs.
	Namespace string `json:"namespace"`

	// Inherited is false when this tenant provides the module itself.
	// +optional
	Inherited bool `json:"inherited,omitempty"`
}

// Tenant is a namespace, a quota, and a position in the tenant tree.
//
// Namespaced deliberately: a Tenant lives in its parent's namespace, and that
// is how the tree is expressed. Root tenants live in tenant-root.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=tn
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.plan`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// TenantList is a list of Tenant.
//
// +kubebuilder:object:root=true
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}
