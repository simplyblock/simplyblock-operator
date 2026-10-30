package kube

import (
	"net/netip"
	"strconv"
	"strings"
)

// DriverName is the CSI driver name simplyblock registers. PVs whose
// Spec.CSI.Driver equals this value are managed by this stack.
const DriverName = "csi.simplyblock.io"

// StorageNodeSetAPIEndpointSliceName returns the name of the EndpointSlice that
// publishes the storage-node-api pods for the given StorageNodeSet. It is named
// per-StorageNodeSet so multiple sets can each own their own slice while all
// backing the cluster's storage-node API Service (WorkloadNames). Both the operator's
// slice builder and any consumer checking whether a worker is published must
// derive the name here so the two cannot drift.
func StorageNodeSetAPIEndpointSliceName(storageNodeSetName string) string {
	return storageNodeSetName + "-storage-node-api-endpoints"
}

// Storage-plane workload identity. The storage-node DaemonSet, its pods, and the
// worker Nodes it enrolls carry these labels so controllers can select them.
// Centralized here because a rename that updates some sites but not others has
// silently broken storage-node scheduling and migration before.
const (
	// LabelApp marks the storage-node workload, with value AppStorageNode. Carried by
	// the DaemonSet and its pods.
	LabelApp       = "app"
	AppStorageNode = "storage-node"

	// LabelSimplyblockCluster records the simplyblock cluster name on the
	// storage-node DaemonSet and its pods. Cluster-scoped: shared by every
	// StorageNodeSet in the cluster.
	LabelSimplyblockCluster = "simplyblock-cluster"

	// LabelStorageNodeSet scopes a worker Node, pod, and DaemonSet to a single
	// StorageNodeSet (value = the StorageNodeSet name). It is the storage-node
	// DaemonSet's node selector, letting multiple StorageNodeSets coexist in one
	// cluster.
	LabelStorageNodeSet = "io.simplyblock.storagenodeset"
)

// LabelSubnetPrefix starts every subnet label the CSI node plugin publishes on
// its node: one per IPv4 subnet a host interface is attached to, valued with
// that interface's name. The operator matches the pNFS metadata server's data
// network against them, to find the nodes where the network can be reached
// and the interface it is reached through. The plugin owns every label under
// this prefix and removes the ones whose subnet the node no longer has.
const LabelSubnetPrefix = "subnet.storage.simplyblock.io/"

// SubnetLabel is the label key for the subnet prefix belongs to: its network
// address and length, with an underscore for the slash a label name cannot
// carry. Host bits are dropped, so every node on a subnet publishes one key.
func SubnetLabel(prefix netip.Prefix) string {
	network := prefix.Masked()
	return LabelSubnetPrefix + network.Addr().String() + "_" + strconv.Itoa(network.Bits())
}

// SubnetOfLabel reads the subnet back from a SubnetLabel key. It refuses any
// other key, and a key whose address is not the network address of its prefix,
// which SubnetLabel never writes.
func SubnetOfLabel(key string) (netip.Prefix, bool) {
	rest, found := strings.CutPrefix(key, LabelSubnetPrefix)
	if !found {
		return netip.Prefix{}, false
	}
	addr, bits, found := strings.Cut(rest, "_")
	if !found {
		return netip.Prefix{}, false
	}
	prefix, err := netip.ParsePrefix(addr + "/" + bits)
	if err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix {
		return netip.Prefix{}, false
	}
	return prefix, true
}

// PoolNodeLabelKey is the label key identifying one storage pool.
func PoolNodeLabelKey(poolUUID string) string {
	return LabelPoolPrefix + poolUUID
}

// StorageNodeSetDaemonSetName is the name of the storage-node DaemonSet owned by
// the named StorageNodeSet. Named per-set so multiple sets can coexist in one
// cluster without sharing a DaemonSet or per-node ConfigMap.
func StorageNodeSetDaemonSetName(storageNodeSetName string) string {
	return "simplyblock-storage-node-ds-" + storageNodeSetName
}

// StorageClass parameter keys. These are the operator/CSI-controller inputs
// that describe how to provision a logical volume. The CSI controller reads
// them at CreateVolume (see PropertiesFromStorageClass, which parses them into
// a typed Properties). The values match the wire keys the CSI driver uses.
const (
	ParamPool                  = "pool_name"
	ParamFabric                = "fabric"
	ParamClusterID             = "cluster_id"
	ParamMaxSize               = "max_size"
	ParamMaxNamespacePerSubsys = "max_namespace_per_subsys"
	ParamEncryption            = "encryption"

	// ParamFSType is Kubernetes' own well-known key for the filesystem an
	// external provisioner formats a volume with, which is why it is not
	// spelled the way the keys above are.
	ParamFSType = "csi.storage.k8s.io/fstype"
	// FSTypePNFS under ParamFSType selects a pNFS export instead of a block
	// device with a filesystem: a class carrying it provisions something only
	// NFS clients can use.
	FSTypePNFS = "pnfs"

	// The three a StoragePool's volume defaults reach the class under. The
	// driver sends ParamPriorityClass, the name of one of the cluster's service
	// classes, with every volume it creates; the other two nothing consumes yet.
	// They are written because the pool's contract is that its defaults appear
	// in the class's parameters, and a class's parameters are immutable: a key
	// omitted now cannot be added to a class later, so the pool that wanted
	// compression would need replacing rather than editing.
	ParamCompression   = "compression"
	ParamReplication   = "replicate"
	ParamPriorityClass = "priority_class"

	// ParamClientCompression and ParamClientDeduplication opt a volume into
	// client-side (VDO) compression and deduplication, independently of each
	// other and of the server-side ParamCompression above. Either one being
	// true is what the CSI driver and the topology gate key off (see
	// LabelVDOCapable).
	ParamClientCompression   = "client_compression"
	ParamClientDeduplication = "client_deduplication"

	// The QoS limits' older spelling. Empty or absent means unset (0). These are
	// read for as long as a class carrying them exists, which is indefinitely: a
	// class's parameters are immutable, so one an older operator generated can
	// never be rewritten. The current spellings and the resolver that prefers
	// them are in qos.go.
	ParamQoSRWIOPS   = "qos_rw_iops"
	ParamQoSRWMBytes = "qos_rw_mbytes"
	ParamQoSRMBytes  = "qos_r_mbytes"
	ParamQoSWMBytes  = "qos_w_mbytes"
)

// VolumeContext keys. The CSI controller sets these in
// PV.Spec.CSI.VolumeAttributes at CreateVolume, and the node service reads
// them at NodeStageVolume.
const (
	VolCtxNQN  = "nqn"
	VolCtxPool = "pool"
)

// PublishContext keys. Returned by ControllerPublishVolume and passed to
// NodeStageVolume: for NVMe-oF, where to reach the target.
const (
	PubCtxTransport = "transport"
	PubCtxAddress   = "address"
	PubCtxPort      = "port"
)

// Labels, annotations, and finalizers atlas-managed objects carry.
//
// Each is the spelling that is written, which is the API group's own prefix.
// The spellings an object written before the move carries are in keys.go beside
// the Key that reads them, and a caller that reads one of these off an object
// reads it through that Key rather than through the constant — the constant
// alone would stop understanding every claim, volume, and node that predates
// the move (design-crd-model.md §9.4).
const (
	// LabelVolumeHandle lets selectors find the K8s objects for a logical
	// volume. Nothing writes it, and nothing can: a handle is 110 bytes
	// normalized and a label value stops at 63, so AnnoVolumeHandle carries
	// this instead.
	LabelVolumeHandle = "storage.simplyblock.io/volume-handle"

	// AnnoVolumeHandle records a volume's handle with its pool segment
	// normalized to a UUID, on the PersistentVolume and the
	// VolumeSnapshotContent whose own field cannot be changed.
	//
	// A handle provisioned before the v2 API migration encodes the pool's
	// name rather than its id, and the field it lives in is immutable:
	// ValidatePersistentVolumeUpdate rejects any change to
	// spec.persistentVolumeSource. Metadata is writable where spec is not, so
	// the field keeps the spelling it was provisioned with and this carries
	// the identity every reader wants.
	//
	// A reader takes it when it is present and consistent with the field, and
	// the field otherwise. Consistency is exact: the cluster and volume
	// segments must match, and only the pool segment may differ, so a
	// hand-edited annotation cannot redirect a volume to another cluster.
	AnnoVolumeHandle = "storage.simplyblock.io/volume-handle"
	// AnnoPool records the source pool on the PV for observability.
	AnnoPool = "storage.simplyblock.io/pool"
	// LabelPoolPrefix opens the per-pool label the operator puts on every node in
	// a StoragePool's AllowedNodes
	LabelPoolPrefix  = "storage.simplyblock.io/storage-pool."
	LabelPoolAllowed = "allowed"
	// LabelVDOCapable marks a node whose kernel loaded dm-vdo, self-probed and
	// self-applied by the CSI node plugin. A volume requesting either client-side
	// parameter is pinned to a node carrying this label (see
	// ParamClientCompression, ParamClientDeduplication).
	LabelVDOCapable = "storage.simplyblock.io/vdo-capable"
	// AnnoRelocatedTo records, on a PersistentVolume moved to another member of
	// its supercluster, the handle of the volume serving it now. The PV's own
	// handle is immutable and names the deleted source. The CSI driver starts
	// resolving the PV at this handle, and deletes the volume it names when the
	// PV is deleted; a PV without it never has its handle's active volume
	// deleted, because another cluster's PV may carry the same handle.
	AnnoRelocatedTo = "storage.simplyblock.io/relocated-to"
	// LabelConsistencyGroup names, on a claim, the consistency group its volume
	// belongs to. A group's members are snapshotted together, live in one
	// cluster, and are never moved apart.
	LabelConsistencyGroup = "storage.simplyblock.io/consistency-group"
	// AnnoVDOCapableManagedBy stamps every LabelVDOCapable value the node
	// plugin's own probe writes, with AnnoVDOCapableManagedByAutoDetect. A label
	// carrying this annotation is the probe's to overwrite on its next run; one
	// without it — an operator's hand-set override — is left alone.
	AnnoVDOCapableManagedBy = "storage.simplyblock.io/vdo-capable-managed-by"
	// AnnoVDOCapableManagedByAutoDetect is AnnoVDOCapableManagedBy's one value.
	AnnoVDOCapableManagedByAutoDetect = "auto-detect"
	// LabelKVMCapable marks a node whose /dev/kvm the CSI node plugin could
	// open, self-probed and self-applied the same way as LabelVDOCapable. The
	// pNFS metadata server pod is scheduled only onto nodes carrying it.
	LabelKVMCapable = "storage.simplyblock.io/kvm-capable"
	// AnnoKVMCapableManagedBy stamps every LabelKVMCapable value the node
	// plugin's own probe writes, with AnnoKVMCapableManagedByAutoDetect. A label
	// without it is an operator's override, left alone.
	AnnoKVMCapableManagedBy = "storage.simplyblock.io/kvm-capable-managed-by"
	// AnnoKVMCapableManagedByAutoDetect is AnnoKVMCapableManagedBy's one value.
	AnnoKVMCapableManagedByAutoDetect = "auto-detect"
	// AnnoSelectedStorageNode pins a PVC's logical volume to a specific storage
	// node. It is the canonical placement/pin annotation: the operator's pin
	// controller, drain, and rebalancer key off it, and the CSI controller reads
	// it in CreateVolume as the primary host_id source.
	AnnoSelectedStorageNode = "storage.simplyblock.io/selected-storage-node"
	// AnnoSelectedStorageNodeApplied records the pinned-volume target the PVC
	// controller has already acted on. It is the strict change-diff marker: the
	// controller only requests a migration when AnnoSelectedStorageNode differs
	// from this value, so its own writes do not re-trigger a migration.
	AnnoSelectedStorageNodeApplied = "storage.simplyblock.io/selected-storage-node-applied"
	// AnnoSelectedStorageNodeRejected records the last pinned-volume value the PVC
	// controller's backstop validation rejected as an unknown storage node. It
	// suppresses duplicate warning events while the invalid value remains in place.
	AnnoSelectedStorageNodeRejected = "storage.simplyblock.io/selected-storage-node-rejected"
	// AnnoPlacementHint is a one-shot creation-time placement hint: the volume-
	// placement webhook writes it with the least-loaded node it picked, the CSI
	// controller sends it as host_id at CreateVolume, and then removes it once the
	// volume exists. Unlike AnnoSelectedStorageNode it is not a pin, and the
	// volume stays eligible for rebalancing.
	AnnoPlacementHint = "storage.simplyblock.io/placement-hint"
	// AnnoHostID is the legacy per-PVC placement annotation. It is honored by the
	// CSI controller as a lowest-priority host_id fallback for pre-existing PVCs,
	// but is never rewritten or removed by the provisioner. The volume-placement
	// webhook rewrites a user-supplied host-id into AnnoSelectedStorageNode (a pin,
	// matching its pre-migration behavior) on new PVCs.
	AnnoHostID = "storage.simplyblock.io/host-id"
	// DeprecatedAnnoHostID is the pre-rename form of AnnoHostID, still
	// honored for backward compatibility.
	DeprecatedAnnoHostID = "simplybk/host-id"
	// AnnoNVMeHostNQN is the host NQN the node's kernel presents when the CSI
	// node plugin connects a volume without naming one (/etc/nvme/hostnqn,
	// derived from the node's persisted hostid). The node plugin publishes it
	// on its Node; the operator reports it, beside the UID-derived form of
	// nqn.Host, to the control plane, which revokes both when it fences the
	// node's host on a two-node cluster
	// (sbcli docs/design/two-node-kubernetes-fencing.md, section 3.1).
	AnnoNVMeHostNQN = "storage.simplyblock.io/nvme-host-nqn"
	// Finalizer guards a PV/PVC from deletion until the backing logical
	// volume is released.
	Finalizer = "storage.simplyblock.io/lvol-protection"
)
