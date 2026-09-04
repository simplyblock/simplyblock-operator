// The metrics.simplyblock.io/v1alpha1 group: read-only measurements of
// simplyblock objects, served by the operator's aggregated API server rather
// than stored as custom resources.
//
// It is a separate group from storage.simplyblock.io because the two are served
// by different machinery and answer different questions. A kind in the storage
// group is a custom resource: it is persisted in etcd, it has a spec a user
// writes, and a controller reconciles it. A kind here is a sample: it is
// computed on demand from the control-plane cache when a client asks for it, it
// is never written, and it is gone the moment the process restarts. Mixing the
// two in one group would put a resource that cannot be applied, watched, or
// backed up beside sixteen that can.
//
// This mirrors how core Kubernetes splits metrics.k8s.io from the workload
// groups, and for the same reason.
//
// The kinds here are served by an aggregated API server, so no CustomResource-
// Definition describes them and `+kubebuilder:skip` keeps the CRD generator out
// of this package. Deepcopy generation still runs: the kinds are runtime.Objects
// like any other.
//
// The aggregated API server also needs OpenAPI v3 definitions for everything it
// serves: since server-side apply went GA, InstallAPIGroup refuses a group
// without them. So openapi-gen runs over this package too and writes
// zz_generated.openapi.go, which is also what makes `kubectl explain` work.
//
// +kubebuilder:object:generate=true
// +kubebuilder:skip
// +k8s:openapi-gen=true
// +groupName=metrics.simplyblock.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group these kinds are served under.
const GroupName = "metrics.simplyblock.io"

// GroupVersion is the group version this package defines.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

// Resource qualifies an unqualified resource name with this group, for the
// API errors the REST storage returns.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
