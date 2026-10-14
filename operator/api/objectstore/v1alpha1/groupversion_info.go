// Package v1alpha1 contains API Schema definitions for the
// objectstore.simplyblock.io v1alpha1 API group: the S3 object interface's
// bucket provisioning, kept as its own group so its API versioning and
// lifecycle stay independent of storage.simplyblock.io's.
//
// +kubebuilder:object:generate=true
// +groupName=objectstore.simplyblock.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "objectstore.simplyblock.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: TODO drop scheme.Builder to keep api package deps minimal

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
