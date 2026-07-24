package kube

// DriverName is the CSI driver name simplyblock registers. PVs whose
// Spec.CSI.Driver equals this value are managed by this stack.
const DriverName = "csi.simplyblock.io"

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
	// AnnoHostID pins a PVC's logical volume to a specific storage node.
	// The volume-placement webhook stamps it on PVCs it schedules; the CSI
	// controller reads it in CreateVolume and forwards it as host_id.
	AnnoHostID = "simplyblock.io/host-id"
	// DeprecatedAnnoHostID is the pre-rename form of AnnoHostID, still
	// honored for backward compatibility.
	DeprecatedAnnoHostID = "simplybk/host-id"
	// AnnoDisableSmartPlacement opts a single PVC out of both the load-aware
	// (volume-placement webhook) and node/pod-affinity (CSI controller)
	// automatic placement tiers, regardless of cluster-wide configuration —
	// the volume falls straight through to the backend's own default
	// placement. Distinct from AnnoHostID: that pins an exact node, this
	// annotation names none and just disables the automatic guesses.
	AnnoDisableSmartPlacement = "simplyblock.io/disable-smart-placement"
	// Finalizer guards a PV/PVC from deletion until the backing logical
	// volume is released.
	Finalizer = "simplyblock.io/lvol-protection"
)
