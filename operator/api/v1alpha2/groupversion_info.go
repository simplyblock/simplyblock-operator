// Package v1alpha2 is the storage version of the simplyblock API group and the
// shape every controller reads. It carries the property names settled by the CRD
// redesign; v1alpha1 keeps the names that shipped and converts into this package
// through the conversion webhook, so no reconciler has to know that an older
// spelling exists.
//
// This package is the conversion hub: every type here implements conversion.Hub
// and none of them implements ConvertTo or ConvertFrom. The spoke side lives
// beside the older types, in api/v1alpha1/*_conversion.go.
//
// operator/docs/designs/crd-redesign/design-property-renames.md is the inventory
// of what was renamed and why, and its §3 is the mechanism this package is the
// hub of.
//
// +kubebuilder:object:generate=true
// +groupName=storage.simplyblock.io
package v1alpha2

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "storage.simplyblock.io", Version: "v1alpha2"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: TODO drop scheme.Builder to keep api package deps minimal

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
