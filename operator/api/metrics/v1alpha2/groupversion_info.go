// Package v1alpha2 holds the measurements simplyblock serves under
// metrics.simplyblock.io: the metrics of a logical volume, a storage cluster, a
// storage node, a storage device, and a storage pool.
//
// A kind here is a sample rather than a stored object. It is computed when a
// client reads it, so a read returns the current numbers and there is nothing
// to retain. All five resources are namespaced and answer get and list only:
// no create, update, delete, or watch, and no field of these types is one a
// client sets.
//
// v1alpha2 is the group's only version.
//
// +kubebuilder:object:generate=true
// +kubebuilder:skip
// +k8s:openapi-gen=true
// +groupName=metrics.simplyblock.io
package v1alpha2

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group these kinds are served under.
const GroupName = "metrics.simplyblock.io"

// GroupVersion is the group version this package defines.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha2"}

// Resource qualifies an unqualified resource name with this group.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
