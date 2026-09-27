// Package v1alpha2 is the current version of the storage.simplyblock.io API
// group and the version the cluster stores. It declares the kinds that run
// simplyblock on Kubernetes: entity kinds such as StorageCluster, StorageNode,
// StoragePool, and StorageDevice, whose spec is the desired state simplyblock
// converges on, and the Ops kind beside each of them, which requests one
// imperative operation against its subject and reports how far that operation
// got.
//
// Several of these kinds also exist as v1alpha1, under property names that have
// since been renamed. A manifest written against v1alpha1 is still accepted and
// converted, and a client that asks for v1alpha1 gets the older spelling back.
// A kind introduced in v1alpha2 has no v1alpha1 spelling.
//
// +kubebuilder:object:generate=true
// +groupName=storage.simplyblock.io
package v1alpha2

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version these objects are registered under.
	GroupVersion = schema.GroupVersion{Group: "storage.simplyblock.io", Version: "v1alpha2"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: TODO drop scheme.Builder to keep api package deps minimal

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
