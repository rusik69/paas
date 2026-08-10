// Package v1alpha1 contains the core.paas.io API types.
//
// Its dependency set is k8s.io/apimachinery and nothing else: external clients
// import this package, and every dependency added here becomes theirs.
//
// +kubebuilder:object:generate=true
// +groupName=core.paas.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of every type in this package.
var GroupVersion = schema.GroupVersion{Group: "core.paas.io", Version: "v1alpha1"}

// SchemeBuilder registers this package's types with a runtime.Scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds this package's types to a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Tenant{}, &TenantList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
