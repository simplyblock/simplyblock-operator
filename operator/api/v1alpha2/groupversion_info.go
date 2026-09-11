// Package v1alpha2 is the storage version of the simplyblock API group and the
// shape every controller reads. It holds two kinds of type, which differ in
// whether anything converts into them.
//
// A kind the CRD redesign renamed a property on has a v1alpha1 spoke: this
// package declares the settled names, v1alpha1 keeps the names that shipped, and
// the conversion webhook translates between them, so no reconciler has to know
// that an older spelling exists. Those types implement conversion.Hub and
// nothing else; the spoke side lives beside the older types, in
// api/v1alpha1/*_conversion.go, and
// operator/docs/designs/crd-redesign/design-property-renames.md is the inventory
// of what moved and why.
//
// A kind the redesign introduces has no spoke and needs no Hub: it was never
// published under v1alpha1, so there is no older shape to convert from and its
// CRD declares one version.
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
