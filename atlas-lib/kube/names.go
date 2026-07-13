package kube

// DriverName is the CSI driver name simplyblock registers. PVs whose
// Spec.CSI.Driver equals this value are managed by this stack.
const DriverName = "csi.simplyblock.io"

// StorageClass parameter keys. These are the operator/CSI-controller
// inputs that describe how to provision a logical volume.
const (
	ParamPool       = "pool"
	ParamQoS        = "qos"
	ParamReplicas   = "replicaCount"
	ParamEncryption = "encryption"
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
	// Finalizer guards a PV/PVC from deletion until the backing logical
	// volume is released.
	Finalizer = "simplyblock.io/lvol-protection"
)
