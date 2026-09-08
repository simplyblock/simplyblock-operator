// The keys both CSI services speak, and the handle they both parse.
//
// A topology key is a contract between the two: the node service advertises it
// on every node, and the controller service matches a volume's placement
// against it. They cannot be spelled separately without eventually being
// spelled differently, and external-provisioner caches the key set at node
// registration and hard-errors when a live node diverges from it.

package csicommon

import (
	"fmt"

	"github.com/simplyblock/atlas/lvol"
)

const (
	// CSIStorageBaseKey and the two keys under it are how external-provisioner
	// passes the claim's identity into CreateVolume parameters, and how it
	// reaches the node service in the volume context.
	CSIStorageBaseKey      = "csi.storage.k8s.io/pvc"
	CSIStorageNameKey      = CSIStorageBaseKey + "/name"
	CSIStorageNamespaceKey = CSIStorageBaseKey + "/namespace"

	// ParamClusterID names the simplyblock cluster a StorageClass provisions
	// into.
	ParamClusterID = "cluster_id"

	// The topology keys the node service reports and the controller service
	// places against. The beta zone key is still read because clusters upgraded
	// from it keep the label.
	TopologyKeyZoneStable   = "topology.kubernetes.io/zone"
	TopologyKeyZoneBeta     = "failure-domain.beta.kubernetes.io/zone"
	TopologyKeyRegionStable = "topology.kubernetes.io/region"

	// TopologyKeyStorageNodeUUIDPrefix mirrors the Kubernetes Node label the
	// simplyblock-operator writes for every storage-node instance co-located on
	// that worker. The full label KEY is "<prefix><clusterUUID>.<socketOrdinal>"
	// and the VALUE is the storage-node UUID. The key deliberately excludes the
	// UUID: external-provisioner caches the set of topology KEYS in the CSINode
	// object at node-plugin registration time and hard-errors CreateVolume if a
	// live Node's label keys ever diverge from that cached set, so the key must
	// stay stable across UUID churn — only the value is expected to change. The
	// key is cluster-scoped so a worker hosting instances from more than one
	// simplyblock cluster cannot have one cluster's socket slot collide with
	// another's.
	TopologyKeyStorageNodeUUIDPrefix = "simplyblock.io/storage-node-uuid."
)

// ParseVolumeHandle decomposes a CSI volume id, which is a volume handle:
// {clusterUUID}:{poolUUIDOrName}:{lvolUUID}. The pool segment is a name on
// volumes provisioned before the v2 API migration, which is why it is carried
// as written and resolved against the control plane later.
//
// Both services parse it: the controller to address the volume it was asked
// about, the node to address the one it is staging.
func ParseVolumeHandle(csiVolumeID string) (*lvol.Handle, error) {
	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(csiVolumeID))
	if !ok {
		return nil, fmt.Errorf("invalid volume handle %q (expected {clusterID}:{poolID}:{lvolID})", csiVolumeID)
	}
	return &handle, nil
}
