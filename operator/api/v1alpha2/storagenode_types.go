// StorageNode in the shape design-storagenode.md Appendix A specifies: one
// backend storage node, meaning one SPDK process bound to one NUMA socket of one
// Kubernetes worker.
//
// What moves against the registered v1alpha1 type is §15.1 of that document, and
// the conversion in api/v1alpha1/storagenode_conversion.go is the whole of the
// translation. The headline changes are spec.storageNodeSetRef becoming
// spec.clusterRef plus spec.nodeSet, spec.overrides becoming spec.config and
// stopping being an override of anything, spec.socketIndex becoming spec.slot,
// the failure domain becoming a label rather than an index, the four per-node
// fields that reached nothing moving to StorageCluster.spec.storageNodes, and
// status gaining a typed phase, a provisioning step, a device summary that is two
// counts rather than a string, and observedGeneration.
//
// NodeLatencyMetrics is declared here rather than on the retired StorageNodeSet,
// because the reading is one node's and the fleet object that used to collect
// them is gone (§15.3).

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// StorageNodePhase is where the operator has got to with this node. The first two
// values are the operator's own provisioning path; the rest are its reading of the
// lifecycle status.status carries in the control plane's own spelling.
// +kubebuilder:validation:Enum=Pending;Provisioning;Online;Removing;Offline;Degraded;Failed
type StorageNodePhase string

const (
	// StorageNodePhasePending: the object exists and no slot has been claimed
	// for it yet.
	StorageNodePhasePending StorageNodePhase = "Pending"

	// StorageNodePhaseProvisioning: the provisioning machine is running.
	StorageNodePhaseProvisioning StorageNodePhase = "Provisioning"

	// StorageNodePhaseOnline: the control plane reports the node online and
	// carrying its share.
	StorageNodePhaseOnline StorageNodePhase = "Online"

	// StorageNodePhaseRemoving: a StorageNodeOps with action Remove is draining
	// it.
	StorageNodePhaseRemoving StorageNodePhase = "Removing"

	// StorageNodePhaseOffline: out of service and reachable, which is where
	// Shutdown, Suspend, and a host maintenance window leave it.
	StorageNodePhaseOffline StorageNodePhase = "Offline"

	// StorageNodePhaseDegraded: serving with less than its devices, which is the
	// node-level half of what StorageDevice reports per device.
	StorageNodePhaseDegraded StorageNodePhase = "Degraded"

	// StorageNodePhaseFailed: unreachable, timed out, or provisioning that will
	// not complete.
	StorageNodePhaseFailed StorageNodePhase = "Failed"
)

// StorageNodeStep is one step of the provisioning path. There is one graph rather
// than a MultiConfig, because an entity has no spec.action to key one on.
// +kubebuilder:validation:Enum=CheckingHost;CheckingConfig;AwaitingSlot;Posting;Resolving;Adopting;AwaitingWorker
type StorageNodeStep string

const (
	// StorageNodeStepCheckingHost waits for the worker's storage-node API to
	// answer, which is the precondition for adding the node at all.
	StorageNodeStepCheckingHost StorageNodeStep = "CheckingHost"

	// StorageNodeStepCheckingConfig holds until the node declares a fault group,
	// where its cluster requires one. It is a gate rather than a validation: the
	// value can arrive later, and holding is what makes filling it in sufficient.
	StorageNodeStepCheckingConfig StorageNodeStep = "CheckingConfig"

	// StorageNodeStepAwaitingSlot holds until the cluster is under its
	// parallel-add limit and no FoundationDB worker is in flight.
	StorageNodeStepAwaitingSlot StorageNodeStep = "AwaitingSlot"

	// StorageNodeStepPosting is the claim: the transition into it is the
	// optimistic-lock patch that makes the node-add single-shot, and the POST
	// follows it.
	StorageNodeStepPosting StorageNodeStep = "Posting"

	// StorageNodeStepResolving matches this node's slot against the cluster's
	// node list until the backend UUID appears.
	StorageNodeStepResolving StorageNodeStep = "Resolving"

	// StorageNodeStepAdopting takes over a backend node the operator did not add.
	StorageNodeStepAdopting StorageNodeStep = "Adopting"

	// StorageNodeStepAwaitingWorker holds while the machine the node is being
	// added to is not there: not Ready, or cordoned ahead of a drain.
	//
	// A worker is rebooted whenever a MachineConfig reaches it, and the storage
	// pool's own config is one, so the first node of a fresh cluster is rebooted
	// in the middle of being added. Every other step is waiting on something the
	// worker does, so none of them can make progress meanwhile, and the step's
	// deadline would be spent on a machine that is coming back. This is the wait
	// written down: it carries its own budget, and it leaves for CheckingHost so
	// the path is walked again rather than resumed in the middle of a claim.
	StorageNodeStepAwaitingWorker StorageNodeStep = "AwaitingWorker"
)

// JournalManagerSpec tunes the journal managers on one storage node.
type JournalManagerSpec struct {
	// Count is the number of journal managers to configure.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Count *int32 `json:"count,omitempty"`

	// PercentPerDevice is the share of each device given to the journal.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	PercentPerDevice *int32 `json:"percentPerDevice,omitempty"`
}

// StorageNodeSizing is what this node's SPDK core layout and huge-page floor were
// sized from. It is stamped from StorageCluster.spec when the node is created and
// is equal across the fleet in steady state; a rolling hardware upgrade is what
// makes two nodes differ, and only for as long as the roll takes. The StorageNode
// validating webhook admits a change from the operator and rejects it from
// everyone else, because unmanaged divergence is what stops the control plane
// placing erasure-coding chunks evenly.
//
// The cluster's maxSubsystemCount is not copied in here. It bounds how many
// volumes a node can serve rather than describing the host the node runs on, so
// it is the same for every node of a cluster and is read from the cluster when the
// node's configuration is generated.
type StorageNodeSizing struct {
	// VCPUCount is the number of vCPUs allocated to SPDK on this node, as an
	// explicit core count rather than a percentage.
	// +kubebuilder:validation:Minimum=4
	// +kubebuilder:validation:Required
	VCPUCount *int32 `json:"vcpuCount"`

	// MinHugePagesSize is the smallest huge-page allocation this node makes, as a
	// size string such as 100G or 1T, where a bare number is gigabytes. It is a
	// floor rather than a limit: the effective allocation is the larger of this
	// value and the minimum the node's device and subsystem count requires.
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`
}

// StorageNodeConfig is a storage node's complete configuration, copied from the
// ClusterDeploymentConfig entry that produced the node. It is a copy rather than a
// projection, because that document is ephemeral and nothing reads it once the
// node exists.
//
// Most of it is immutable: by marker where a field has no legitimate writer, and
// by the validating webhook where it has exactly one.
type StorageNodeConfig struct {
	// Sizing is what this node's huge pages and core layout were sized from.
	// Writable by the operator alone.
	// +kubebuilder:validation:Required
	Sizing StorageNodeSizing `json:"sizing"`

	// SpdkImage overrides the SPDK image the control plane starts for this node,
	// which is what makes a phased image rollout expressible per node.
	// +optional
	SpdkImage string `json:"spdkImage,omitempty"`

	// SpdkImagePullPolicy controls when that image is pulled, and defaults to
	// Always because the images this product ships are moving tags.
	//
	// The control plane starts the SPDK pod, not the operator, and its
	// spdk_process_start takes no pull policy: the pod template it renders writes
	// Always itself. So a node states the policy here and the node-add call does
	// not yet carry it, which is a gap the control plane closes rather than this
	// kind. Stating anything but Always is therefore recorded and not yet obeyed.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	SpdkImagePullPolicy corev1.PullPolicy `json:"spdkImagePullPolicy,omitempty"`

	// SpdkProxyImage overrides the SPDK proxy image for this node.
	// +optional
	SpdkProxyImage string `json:"spdkProxyImage,omitempty"`

	// SpdkSystemMemory is the memory the control plane starts this node's SPDK
	// with, as a size string such as 4G or 512M. Mutable: a node whose device
	// count grew legitimately needs to raise it.
	// +kubebuilder:validation:Pattern=`^[0-9]+(G|GI|GB|GiB|M|MI|MB|MiB|g|gi|gb|gib|m|mi|mb|mib)?$`
	// +optional
	SpdkSystemMemory string `json:"spdkSystemMemory,omitempty"`

	// JournalManager tunes the journal manager count and per-device capacity
	// share for this node. Immutable: both are on-disk layout, fixed when the
	// devices were partitioned.
	// +optional
	// +k8s:immutable
	JournalManager *JournalManagerSpec `json:"journalManager,omitempty"`

	// DeviceNames names the devices to use. An entry is a PCI address
	// ("0000:5e:00.0") or a device path ("/dev/sdb,") which are the two classes
	// simplyblock accepts as backend storage, and a bare name ("nvme0n1") is read
	// as a path under /dev. One list carries both spellings, and every entry is of
	// the class its cluster declares in StorageCluster.spec.deviceClass: a list
	// mixing the two, or naming the class the cluster is not, is rejected by the
	// StorageNode validating webhook. Set explicitly, it overrides every filter
	// below. Immutable: it selects which physical devices the node owns.
	// +kubebuilder:validation:items:Pattern=`^([0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]|/dev/[a-zA-Z0-9._/-]+|[a-zA-Z0-9._-]+)$`
	// +optional
	// +k8s:immutable
	DeviceNames []string `json:"deviceNames,omitempty"`

	// PcieAllowList selects devices by PCI address. It is the one device field a
	// migration writes, merging spec.migrate.newSsdPcie into it so devices added
	// on the target host survive a later rebuild, so it is guarded by the
	// StorageNode validating webhook rather than by a marker. This and the two
	// PCI filters below belong to an NVMe cluster: the webhook rejects them on a
	// cluster whose deviceClass is LogicalBlock, because a logical block device
	// has no PCI address to match.
	// +optional
	PcieAllowList []string `json:"pcieAllowList,omitempty"`

	// PcieDenyList excludes devices by PCI address.
	// +optional
	// +k8s:immutable
	PcieDenyList []string `json:"pcieDenyList,omitempty"`

	// PcieModel filters devices by PCI model string.
	// +optional
	// +k8s:immutable
	PcieModel string `json:"pcieModel,omitempty"`

	// DriveSizeRange filters devices by size.
	// +optional
	// +k8s:immutable
	DriveSizeRange string `json:"driveSizeRange,omitempty"`

	// FailureDomain is the label of the fault group this node belongs to
	// such as rack-b, naming the physical grouping it shares with its peers rather
	// than indexing it. Required when the cluster has enableFailureDomains set,
	// and provisioning is held with a FailureDomainMissing event until it is
	// present. Immutable once set, which is what makes it fillable later and then
	// frozen: chunk placement was computed from it.
	//
	// The value takes the shape of a Kubernetes label value, because that is what
	// it is seeded from where a cluster carries topology labels at all.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([-_.a-zA-Z0-9]*[a-zA-Z0-9])?$`
	// +optional
	// +k8s:immutable
	FailureDomain string `json:"failureDomain,omitempty"`

	// Expand marks this node as an addition to an already-active cluster, which
	// the control plane reads as a request to rebalance onto it rather than to
	// treat it as part of an initial layout. Immutable once set: it describes how
	// the node joined rather than what it is.
	// +optional
	// +k8s:immutable
	Expand *bool `json:"expand,omitempty"`
}

// StorageNodeSpec is the desired state of one backend storage node, meaning one
// SPDK process bound to one NUMA socket of one Kubernetes worker.
type StorageNodeSpec struct {
	// ClusterRef names the StorageCluster this node belongs to. The cluster also
	// owns this object by controller reference, so deleting the cluster deletes
	// its nodes.
	//
	// Bounded at what a StorageCluster name may be, since a longer value names
	// nothing that can exist (design-api-upgrade.md §19.4).
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// NodeSet is the name of the group in ClusterDeploymentConfig.nodeSets[] this
	// node was declared under. It is a label rather than a reference: nothing is
	// fetched by it, and it exists so that a node can be traced back to the
	// document that produced it.
	// +optional
	// +k8s:immutable
	NodeSet string `json:"nodeSet,omitempty"`

	// WorkerNode is the Kubernetes worker hostname this node runs on. It is not
	// marked immutable, because a migration re-points it, but the StorageNode
	// validating webhook rejects any change made by an identity outside the
	// operator's namespace.
	// +kubebuilder:validation:Required
	WorkerNode string `json:"workerNode"`

	// SocketID is the NUMA socket this node is bound to, as declared in the node
	// set's socket list, so 0 or 1. With NodeIndex it decomposes Slot into the
	// pair a person reads; nothing but a print column consumes either.
	// +optional
	// +k8s:immutable
	SocketID string `json:"socketId,omitempty"`

	// NodeIndex is the position among the nodes sharing this socket, in
	// 0..nodesPerSocket-1. See SocketID.
	// +kubebuilder:validation:Minimum=0
	// +optional
	// +k8s:immutable
	NodeIndex *int32 `json:"nodeIndex,omitempty"`

	// Slot is which storage-node slot on this worker the object occupies, counted
	// from zero. A worker runs one node per socket per nodesPerSocket, and the
	// slot is the position among them. It is the identity the operator keys on:
	// the topology label the CSI driver reads is
	// storage.simplyblock.io/storage-node-uuid.<clusterUUID>.<slot>, and the slot
	// outlives the node filling it, because only the UUID behind it changes when a
	// node is replaced or relocated.
	// +kubebuilder:validation:Minimum=0
	// +optional
	// +k8s:immutable
	Slot *int32 `json:"slot,omitempty"`

	// Config is this node's complete configuration, copied from the
	// ClusterDeploymentConfig entry that produced it. It is a copy rather than a
	// projection, because that document is ephemeral: nothing reads it once the
	// node exists, deleting it changes nothing, and editing it reaches only nodes
	// created afterward.
	// +kubebuilder:validation:Required
	Config StorageNodeConfig `json:"config"`
}

// NodeLatencyMetrics is the fio-measured 4K NVMe-oF write latency of one backend
// storage node, which is the denominator of the rebalancer's deviation signal.
//
// It is a node's reading and it lives on the node. The retired StorageNodeSet
// collected one entry per node in a fleet-wide list, which made every node's
// measurement a write to one object shared by all of them.
type NodeLatencyMetrics struct {
	// NodeUUID is the backend storage node the reading was taken against. It is
	// carried beside the reading rather than inferred from status.uuid, because a
	// baseline measured against one backend node stops describing the slot once a
	// replacement fills it.
	// +kubebuilder:validation:Required
	NodeUUID string `json:"nodeUUID"`

	// BaselineP50NS is the p50 write latency, in nanoseconds, of the initial
	// empty-cluster benchmark.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BaselineP50NS int64 `json:"baselineP50NS,omitempty"`

	// BaselineP99NS is the p99 write latency, in nanoseconds, of the same
	// benchmark.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BaselineP99NS int64 `json:"baselineP99NS,omitempty"`

	// BaselineMeasuredAt is when the baseline was established.
	// +optional
	BaselineMeasuredAt *metav1.Time `json:"baselineMeasuredAt,omitempty"`
}

// StorageNodeDevices counts the NVMe devices on a node and how many of them are
// online. It is a summary rather than an inventory: per-device capacity, health,
// and conditions belong to StorageDevice.
//
// Neither field takes omitempty. Zero online devices is the condition worth
// seeing, and a field that disappears at zero would report it as nothing at all. A
// node the control plane has not reported on is the absent parent instead.
type StorageNodeDevices struct {
	// Online is how many of the node's devices the control plane reports as
	// usable.
	// +kubebuilder:validation:Minimum=0
	Online int32 `json:"online"`

	// Total is how many devices the node has.
	// +kubebuilder:validation:Minimum=0
	Total int32 `json:"total"`
}

// StorageNodeCapacity is a node's storage occupancy, as the control plane last
// measured it.
//
// It carries the same two numbers as a device's capacity, because a node's is the
// sum of its devices' and a reader comparing the two should not have to reconcile
// different shapes. It is written only when the reading has moved materially: a
// sample that changed by a few blocks is not worth an etcd write, and writing
// every sample would make the reconciler retrigger itself on its own status
// update.
type StorageNodeCapacity struct {
	// TotalBytes is the storage the node's devices provide.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// UsedBytes is what they currently hold.
	// +kubebuilder:validation:Minimum=0
	// +optional
	UsedBytes *int64 `json:"usedBytes,omitempty"`

	// SampledAt is when the control plane took the reading. It is not when the
	// object was written, and it may be considerably older if metrics collection
	// has stopped.
	// +optional
	SampledAt *metav1.Time `json:"sampledAt,omitempty"`
}

// StorageNodeResources groups the compute and storage figures the control plane
// reports for a node.
type StorageNodeResources struct {
	// CPU is the number of SPDK cores allocated to this node.
	// +optional
	CPU *int32 `json:"cpu,omitempty"`

	// Memory is the SPDK memory allocation the control plane reports.
	// +optional
	Memory string `json:"memory,omitempty"`

	// Volumes is the current number of logical volumes on this node.
	// +optional
	Volumes *int32 `json:"volumes,omitempty"`

	// Devices summarizes the node's NVMe devices. Absent until the control plane
	// has reported, which is what tells a node that has not reported from one that
	// genuinely has no devices.
	// +optional
	Devices *StorageNodeDevices `json:"devices,omitempty"`

	// Capacity is how much of the node's storage is in use, summed over its
	// devices. It is a measurement rather than a declaration, so it is absent
	// until something has measured it, and it lags reality by the interval at
	// which the control plane's metrics are scraped.
	// +optional
	Capacity *StorageNodeCapacity `json:"capacity,omitempty"`
}

// StorageNodePorts groups the addresses and ports a node listens on.
type StorageNodePorts struct {
	// Management is the management IP address of the node.
	// +optional
	Management string `json:"management,omitempty"`

	// The NVMe-oF fabric port.
	// +optional
	NvmeOf *int32 `json:"nvmeof,omitempty"`

	// Lvol is the logical-volume subsystem port.
	// +optional
	Lvol *int32 `json:"lvol,omitempty"`

	// Rpc is the RPC and management API port.
	// +optional
	Rpc *int32 `json:"rpc,omitempty"`
}

// StorageNodeStatus is the observed state of one storage node.
type StorageNodeStatus struct {
	// Phase is the operator's own view of this node, and the field its
	// provisioning branches on.
	// +optional
	Phase StorageNodePhase `json:"phase,omitempty"`

	// Step is the position of the provisioning machine, as the shared
	// statemachine.KubeSnapshot. The rule is what an Enum marker would do if a
	// marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['CheckingHost','CheckingConfig','AwaitingSlot','Posting','Resolving','Adopting','AwaitingWorker']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// UUID is the backend node UUID. Empty means the node has neither been
	// provisioned nor adopted, and non-empty means steady state.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Status is the lifecycle the control plane reports: online, suspended,
	// offline, in_creation, in_restart, in_shutdown, unreachable, or timeout. The
	// values are the control plane's, which is why they are neither PascalCase nor
	// constrained by an Enum here.
	// +optional
	Status string `json:"status,omitempty"`

	// Health is the health flag the control plane reports.
	// +optional
	Health bool `json:"health,omitempty"`

	// Hostname is the node hostname as the control plane reports it.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// Uptime is the node uptime as the control plane reports it.
	// +optional
	Uptime string `json:"uptime,omitempty"`

	// Resources groups the reported compute and storage figures.
	// +optional
	Resources *StorageNodeResources `json:"resources,omitempty"`

	// Ports groups the reported addresses and ports.
	// +optional
	Ports *StorageNodePorts `json:"ports,omitempty"`

	// FailureDomain is the failure-domain label the control plane actually
	// assigned, which is not necessarily the one spec.config.failureDomain
	// requested.
	// +optional
	FailureDomain string `json:"failureDomain,omitempty"`

	// ActiveOpsRef names the StorageNodeOps currently allowed to touch this node.
	// Empty when none is running.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// LatencyMetrics holds the fio-measured NVMe-oF baseline the volume
	// rebalancer reads.
	// +optional
	LatencyMetrics *NodeLatencyMetrics `json:"latencyMetrics,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as the
	// node moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// v1alpha2 is the storage version in the manifests this repository ships, which
// are the ones a fresh install applies. An upgrade of an existing cluster reaches
// it through the storage rewrite rather than through this marker, for the reason
// storagenodeops_types.go states.
//
// The name is bounded at a label's 63 bytes, because the StorageDevice mirror
// writes it into storage.simplyblock.io/node on every device this node reports.
// The operator's own names fit by construction — the formula that builds them
// carries the same limit — and the rule is what holds a node somebody authored
// to the same bound (design-api-upgrade.md §19.1, §19.4).
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="a StorageNode name is at most 63 characters, because it is written into the storage.simplyblock.io/node label on every StorageDevice of this node"
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sn
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".spec.workerNode"
// +kubebuilder:printcolumn:name="Socket",type=string,JSONPath=".spec.socketId"
// +kubebuilder:printcolumn:name="Slot",type=integer,JSONPath=".spec.slot"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Health",type=boolean,JSONPath=".status.health"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid",priority=1
// +kubebuilder:printcolumn:name="FD",type=string,JSONPath=".status.failureDomain",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNode is one backend storage node: one SPDK process bound to one NUMA
// socket of one Kubernetes worker. One object exists per (workerNode, slot) pair,
// owned by the StorageCluster it belongs to.
type StorageNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageNodeSpec   `json:"spec,omitempty"`
	Status StorageNodeStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for StorageNode.
func (*StorageNode) Hub() {}

// +kubebuilder:object:root=true

// StorageNodeList contains a list of StorageNode.
type StorageNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageNode{}, &StorageNodeList{})
}
