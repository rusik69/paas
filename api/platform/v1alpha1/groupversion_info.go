// Package v1alpha1 contains the platform.paas.io API types.
//
// Its dependency set is k8s.io/apimachinery and nothing else: external clients
// import this package, and every dependency added here becomes theirs. That is
// why scheme registration below is hand-written rather than taken from the
// kubebuilder scaffold, which imports controller-runtime.
//
// +kubebuilder:object:generate=true
// +groupName=platform.paas.io
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
	s.AddKnownTypes(GroupVersion,
		&Platform{}, &PlatformList{},
		&PackageSource{}, &PackageSourceList{},
		&Package{}, &PackageList{},
		&ServiceClass{}, &ServiceClassList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
