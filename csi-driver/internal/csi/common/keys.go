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

	// CSIStoragePVNameKey is how external-provisioner names the PersistentVolume
	// in the same context. The node service writes it, with the claim's two
	// keys above, into the volume's LVM metadata as informational tags.
	CSIStoragePVNameKey = "csi.storage.k8s.io/pv/name"

	// ParamClusterID names the simplyblock cluster a StorageClass provisions
	// into.
	ParamClusterID = "cluster_id"

	// ParamEncryption names the StorageClass parameter that asks for an
	// encrypted volume, and the volume-context key the controller sends it on
	// under. Both halves are the same word because they are one fact read
	// at two moments: the control plane consumes the parameter at creation, and
	// the node service needs the answer at every stage, where the bytes on an
	// encrypted volume mean nothing and the guard against formatting somebody's
	// data has to defer to the record instead.
	ParamEncryption = "encryption"

	// The volume-context keys a pNFS export is mounted from. They are written
	// by the controller service and read by the node service, which is the
	// same reason the topology keys are here: two packages spelling one wire
	// contract separately eventually spell it differently, and a key that
	// differs by a character leaves the node taking the block branch for a
	// volume that has no block device.
	CtxAccessProtocol = "access_protocol"
	CtxExportService  = "export_service"
	CtxExportPath     = "export_path"

	// AccessProtocolNFS is what CtxAccessProtocol carries for a pNFS volume,
	// and the value the node branches on.
	AccessProtocolNFS = "nfs"

	// The volume-context keys a striped volume is staged from, written by the
	// controller service and read by the node service. CtxStripeMembers lists
	// the members' logical-volume UUIDs, comma separated, in stripe order: the
	// first is the volume's own identity, and the order is the order the node
	// stripes them in, so it is never rewritten after the volume is created.
	// CtxStripeChunkBytes is the chunk each member takes, in bytes.
	CtxStripeWidth      = "stripeWidth"
	CtxStripeMembers    = "stripeMembers"
	CtxStripeChunkBytes = "stripeChunkBytes"

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
	// stay stable across UUID churn, and only the value is expected to change. The
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
//
// A striped volume's id carries a fourth segment naming its width
// (lvol.ParseStripedHandle), and parses to its first member, which is the
// volume's identity: the logical volume its PersistentVolume is found by, its
// lock, and the one whose name the other members are named after. A caller that
// acts on the volume's data rather than its identity has to ask StripeWidth as
// well, or it acts on the first member alone.
func ParseVolumeHandle(csiVolumeID string) (*lvol.Handle, error) {
	handle, _, ok := lvol.ParseStripedHandle(lvol.VolumeHandle(csiVolumeID))
	if !ok {
		return nil, fmt.Errorf("invalid volume handle %q (expected {clusterID}:{poolID}:{lvolID})", csiVolumeID)
	}
	return &handle, nil
}

// StripeWidth is how many members the volume an id names is striped across,
// and 1 for an ordinary volume and for an id that does not parse.
func StripeWidth(csiVolumeID string) int {
	_, width, ok := lvol.ParseStripedHandle(lvol.VolumeHandle(csiVolumeID))
	if !ok {
		return 1
	}
	return width
}
