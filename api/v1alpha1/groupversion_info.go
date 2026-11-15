// Package v1alpha1 contains the API types for the platform.mella7.me group.
// +kubebuilder:object:generate=true
// +groupName=platform.mella7.me
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version served by this API.
	GroupVersion = schema.GroupVersion{Group: "platform.mella7.me", Version: "v1alpha1"}

	// SchemeBuilder collects the Go types of this group.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers the types of this group with a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
