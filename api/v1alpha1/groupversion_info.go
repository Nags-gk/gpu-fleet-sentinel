// Package v1alpha1 contains the GPUNodePolicy API of GPU Fleet Sentinel.
//
// +kubebuilder:object:generate=true
// +groupName=gpu-sentinel.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version of this package.
	GroupVersion = schema.GroupVersion{Group: "gpu-sentinel.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
