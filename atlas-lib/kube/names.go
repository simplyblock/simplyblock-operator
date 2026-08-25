package kube

// DriverName is the CSI driver name simplyblock registers. PVs whose
// Spec.CSI.Driver equals this value are managed by this stack.
const DriverName = "csi.simplyblock.io"

// StorageNodeSetAPIServiceName is the headless Service that fronts the
// storage-node-api pods of every StorageNodeSet in a namespace. The control
// plane reaches an individual pod at <node-hostname>.<this-service>.<ns>.svc.
const StorageNodeSetAPIServiceName = "simplyblock-storage-node-api"

// StorageNodeSetAPIEndpointSliceName returns the name of the EndpointSlice that
// publishes the storage-node-api pods for the given StorageNodeSet. It is named
// per-StorageNodeSet so multiple sets can each own their own slice while all
// backing the shared StorageNodeSetAPIServiceName Service. Both the operator's
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
	// LabelApp marks the storage-node workload; value AppStorageNode. Carried by
	// the DaemonSet and its pods.
	LabelApp       = "app"
	AppStorageNode = "storage-node"

	// LabelSimplyblockCluster records the simplyblock cluster name on the
	// storage-node DaemonSet and its pods. Cluster-scoped: shared by every
	// StorageNodeSet in the cluster.
	LabelSimplyblockCluster = "simplyblock-cluster"

	// LabelNodeType marks a worker Node as part of a cluster's storage plane;
	// value is NodeTypeStoragePlaneValue(clusterName). Cluster-scoped — do not
	// use it to select a single StorageNodeSet's workers; use LabelStorageNodeSet.
	LabelNodeType = "io.simplyblock.node-type"

	// LabelStorageNodeSet scopes a worker Node, pod, and DaemonSet to a single
	// StorageNodeSet (value = the StorageNodeSet name). It is the storage-node
	// DaemonSet's node selector, letting multiple StorageNodeSets coexist in one
	// cluster.
	LabelStorageNodeSet = "io.simplyblock.storagenodeset"
)

// NodeTypeStoragePlaneValue is the LabelNodeType value marking a worker as part
// of the given cluster's storage plane.
func NodeTypeStoragePlaneValue(clusterName string) string {
	return "simplyblock-storage-plane-" + clusterName
}

// StorageNodeSetDaemonSetName is the name of the storage-node DaemonSet owned by
// the named StorageNodeSet. Named per-set so multiple sets can coexist in one
// cluster without sharing a DaemonSet or per-node ConfigMap.
func StorageNodeSetDaemonSetName(storageNodeSetName string) string {
	return "simplyblock-storage-node-ds-" + storageNodeSetName
}

// StorageNodeSetServiceAccountName is the name of the ServiceAccount owned by
// the named StorageNodeSet and mounted by its storage-node DaemonSet pods.
// Named per-set, not shared, so it can safely carry a controller
// ownerReference to exactly the StorageNodeSet that owns it.
func StorageNodeSetServiceAccountName(storageNodeSetName string) string {
	return "simplyblock-storage-node-sa-" + storageNodeSetName
}

// StorageNodeSetClusterRoleBindingName is the name of the ClusterRoleBinding
// that grants the named StorageNodeSet's own ServiceAccount the shared
// simplyblock-storage-node-role ClusterRole. Includes both namespace and set
// name since ClusterRoleBindings are cluster-scoped.
func StorageNodeSetClusterRoleBindingName(namespace, storageNodeSetName string) string {
	return "simplyblock-storage-node-binding-" + namespace + "-" + storageNodeSetName
}

// LegacyStorageNodeSetServiceAccountName is the pre-migration ServiceAccount
// name, shared by every StorageNodeSet in a namespace before each set got its
// own. Whichever StorageNodeSet already owns it when upgrading keeps this
// name (and its DaemonSet's pod template, and thus its running pods,
// untouched); every other StorageNodeSet uses StorageNodeSetServiceAccountName.
const LegacyStorageNodeSetServiceAccountName = "simplyblock-storage-node-sa"

// LegacyStorageNodeSetClusterRoleBindingName is the pre-migration, per-
// namespace (not per-set) ClusterRoleBinding name, paired with
// LegacyStorageNodeSetServiceAccountName.
func LegacyStorageNodeSetClusterRoleBindingName(namespace string) string {
	return "simplyblock-storage-node-binding-" + namespace
}

// StorageClass parameter keys. These are the operator/CSI-controller inputs
// that describe how to provision a logical volume; the CSI controller reads
// them at CreateVolume (see PropertiesFromStorageClass, which parses them into
// a typed Properties). The values match the wire keys the CSI driver uses.
const (
	ParamPool                  = "pool_name"
	ParamFabric                = "fabric"
	ParamClusterID             = "cluster_id"
	ParamMaxSize               = "max_size"
	ParamLvolPriorityClass     = "lvol_priority_class"
	ParamMaxNamespacePerSubsys = "max_namespace_per_subsys"
	ParamCompression           = "compression"
	ParamEncryption            = "encryption"
	ParamReplicate             = "replicate"

	// QoS limits. Empty/absent means unset (0).
	ParamQoSRWIOPS   = "qos_rw_iops"
	ParamQoSRWMBytes = "qos_rw_mbytes"
	ParamQoSRMBytes  = "qos_r_mbytes"
	ParamQoSWMBytes  = "qos_w_mbytes"
)

// VolumeContext keys. The CSI controller sets these in
// PV.Spec.CSI.VolumeAttributes at CreateVolume; the node service reads
// them at NodeStageVolume.
const (
	VolCtxNQN  = "nqn"
	VolCtxPool = "pool"
)

// PublishContext keys. Returned by ControllerPublishVolume and passed to
// NodeStageVolume — for NVMe-oF, where to reach the target.
const (
	PubCtxTransport = "transport"
	PubCtxAddress   = "address"
	PubCtxPort      = "port"
)

// Labels, annotations, and finalizers atlas-managed objects carry.
const (
	// LabelVolumeHandle lets selectors find the K8s objects for a logical
	// volume.
	LabelVolumeHandle = "simplyblock.io/volume-handle"
	// AnnoPool records the source pool on the PV for observability.
	AnnoPool = "simplyblock.io/pool"
	// AnnoSelectedStorageNode pins a PVC's logical volume to a specific storage
	// node. It is the canonical placement/pin annotation: the operator's pin
	// controller, drain, and rebalancer key off it, and the CSI controller reads
	// it in CreateVolume as the primary host_id source.
	AnnoSelectedStorageNode = "simplyblock.io/selected-storage-node"
	// AnnoSelectedStorageNodeApplied records the pinned-volume target the PVC
	// controller has already acted on. It is the strict change-diff marker: the
	// controller only requests a migration when AnnoSelectedStorageNode differs
	// from this value, so its own writes do not re-trigger a migration.
	AnnoSelectedStorageNodeApplied = "simplyblock.io/selected-storage-node-applied"
	// AnnoSelectedStorageNodeRejected records the last pinned-volume value the PVC
	// controller's backstop validation rejected as an unknown storage node. It
	// suppresses duplicate warning events while the invalid value remains in place.
	AnnoSelectedStorageNodeRejected = "simplyblock.io/selected-storage-node-rejected"
	// AnnoPlacementHint is a one-shot creation-time placement hint: the volume-
	// placement webhook writes it with the least-loaded node it picked, the CSI
	// controller sends it as host_id at CreateVolume, and then removes it once the
	// volume exists. Unlike AnnoSelectedStorageNode it is not a pin — the volume
	// stays eligible for rebalancing.
	AnnoPlacementHint = "simplyblock.io/placement-hint"
	// AnnoHostID is the legacy per-PVC placement annotation. It is honored by the
	// CSI controller as a lowest-priority host_id fallback for pre-existing PVCs,
	// but is never rewritten or removed by the provisioner. The volume-placement
	// webhook rewrites a user-supplied host-id into AnnoSelectedStorageNode (a pin,
	// matching its pre-migration behavior) on new PVCs.
	AnnoHostID = "simplyblock.io/host-id"
	// DeprecatedAnnoHostID is the pre-rename form of AnnoHostID, still
	// honored for backward compatibility.
	DeprecatedAnnoHostID = "simplybk/host-id"
	// Finalizer guards a PV/PVC from deletion until the backing logical
	// volume is released.
	Finalizer = "simplyblock.io/lvol-protection"
)
