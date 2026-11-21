// Package v1alpha1 contains the cloud.paguos.dev/v1alpha1 API.
// +kubebuilder:object:generate=true
// +groupName=cloud.paguos.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "cloud.paguos.dev", Version: "v1alpha1"}

	// SchemeBuilder adds the types in this group to a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
