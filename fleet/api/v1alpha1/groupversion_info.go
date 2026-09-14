// Package v1alpha1 is the fleet API group: the kinds a hub owns when one
// simplyblock control plane fronts several Kubernetes clusters, each running the
// operator.
//
// The group is separate from storage.simplyblock.io, and it is installed on the
// hub and nowhere else. That separation is the whole arrangement's load-bearing
// constraint: a standalone installation installs no kind from this group, so its
// CustomResourceDefinition set, its RBAC, and its webhook configurations are
// what they were before a fleet existed.
//
// No kind here is a copy of a kind in a member. A hub object carries intent and
// names the member it is for, a member object is what the operator reconciles,
// and the two sides share no kind, which is why a mirrored generation, a pruned
// spec, a UID that does not travel, and a refused create with nowhere to be
// reported are all absent problems rather than solved ones.
//
// References run one way. A kind here names a member, and no kind in a member
// names a hub, so a member that loses its hub keeps reconciling.
//
// +kubebuilder:object:generate=true
// +groupName=fleet.simplyblock.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version these objects are registered under.
	GroupVersion = schema.GroupVersion{Group: "fleet.simplyblock.io", Version: "v1alpha1"}

	// SchemeBuilder adds the Go types of this group version to a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: matches the operator's api packages, which keep scheme.Builder to hold the api package's dependencies down

	// AddToScheme adds the types in this group version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
