/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// JournalManagerSpec defines journal manager tuning parameters.
type JournalManagerSpec struct {
	// Count is the number of journal managers to configure.
	Count *int32 `json:"count,omitempty"`
	// PercentPerDevice is the journal manager capacity percentage per device.
	PercentPerDevice *int32 `json:"percentPerDevice,omitempty"`
	// UseSeparateJournalDevice enables using separate journal devices.
	// FIXME: Unused for now
	UseSeparateJournalDevice *bool `json:"useSeparateJournalDevice,omitempty"`
}

// StorageNodeSpec defines the desired state of StorageNode
type StorageNodeSpec struct {
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// ClusterImage is the container image used for storage-node workloads.
	ClusterImage string `json:"clusterImage,omitempty"`
	// +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove
	// Action triggers an imperative node operation.
	Action string `json:"action,omitempty"`
	// NodeUUID is required when action is specified
	NodeUUID string `json:"nodeUUID,omitempty"`

	// MaxLogicalVolumeCount is the maximum number of logical volumes per node.
	MaxLogicalVolumeCount *int32 `json:"maxLogicalVolumeCount,omitempty"`
	// MaxSize is the maximum allocatable size of the storage node.
	MaxSize string `json:"maxSize,omitempty"`
	// SpdkImage is the SPDK image reference used by node services.
	SpdkImage string `json:"spdkImage,omitempty"`
	// SpdkProxyImage is the SPDK proxy image reference used by node services.
	SpdkProxyImage string `json:"spdkProxyImage,omitempty"`
	// MgmtIfname is the management interface name used by storage nodes.
	MgmtIfname string `json:"mgmtIfname,omitempty"`
	// Partitions is the number of partitions created per backend storage device.
	Partitions *int32 `json:"partitions,omitempty"`
	// JournalManagerSpec configures journal manager behavior.
	JournalManagerSpec *JournalManagerSpec `json:"journalManager,omitempty"`
	// CoreIsolation enables CPU core isolation mode.
	CoreIsolation *bool `json:"coreIsolation,omitempty"`
	// CorePercentage is the percentage of cores to be used for spdk (0-99).
	CorePercentage *int32 `json:"corePercentage,omitempty"`
	// PcieAllowList is the list of PCI addresses allowed for use.
	PcieAllowList []string `json:"pcieAllowList,omitempty"`
	// PcieDenyList is the list of PCI addresses excluded from use.
	PcieDenyList []string `json:"pcieDenyList,omitempty"`
	// PcieModel filters devices by PCI model.
	PcieModel string `json:"pcieModel,omitempty"`
	// DriveSizeRange filters devices by size range.
	DriveSizeRange string `json:"driveSizeRange,omitempty"`
	// SocketsToUse restricts deployment to selected NUMA sockets.
	SocketsToUse []string `json:"socketsToUse,omitempty"`
	// NodesPerSocket defines how many storage nodes are created per NUMA socket.
	NodesPerSocket *int32 `json:"nodesPerSocket,omitempty"`
	// DataIfname lists data-plane network interfaces.
	DataIfname []string `json:"dataIfname,omitempty"`
	// WorkerNodes is the set of Kubernetes worker nodes to manage.
	WorkerNodes []string `json:"workerNodes,omitempty"`
	// WorkerNode is a single worker node used by action flows.
	WorkerNode string `json:"workerNode,omitempty"`
	// OpenShiftCluster indicates OpenShift-specific behavior should be enabled.
	OpenShiftCluster *bool `json:"openShiftCluster,omitempty"`
	// DeviceNames explicitly defines a comma separated list of nvme namespace names like nvme0n1,nvme1n1...
	DeviceNames []string `json:"deviceNames,omitempty"`
	// UbuntuHost indicates the node host OS is Ubuntu.
	UbuntuHost *bool `json:"ubuntuHost,omitempty"`
	// SkipKubeletConfiguration skips kubelet configuration changes.
	SkipKubeletConfiguration *bool `json:"skipKubeletConfiguration,omitempty"`
	// ForceFormat4K forces 4K blocksize formatting of the NVMe device where supported.
	ForceFormat4K *bool `json:"forceFormat4K,omitempty"`
	// EnableCpuTopology enables topology-aware CPU handling.
	EnableCpuTopology *bool `json:"enableCpuTopology,omitempty"`
	// ReservedSystemCPU defines CPUs reserved for system workloads.
	ReservedSystemCPU string `json:"reservedSystemCPU,omitempty"`

	// Tolerations configures pod tolerations for storage-node pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// AddPcieToAllowList appends devices to the allow-list during restart actions.
	// FIXME: Unused for now
	AddPcieToAllowList []string `json:"addPcieToAllowList,omitempty"`
	// NodeAddr is the explicit node address used by action flows.
	// FIXME: Unused for now
	NodeAddr string `json:"nodeAddr,omitempty"`
	// Force enables forced action execution where supported.
	// FIXME: Unused for now
	Force *bool `json:"force,omitempty"`
}

// Drain coordination phases for a worker node undergoing a rolling upgrade drain.
const (
	// DrainPhaseDetected means the node is cordoned and waiting for a drain slot.
	DrainPhaseDetected = "detected"
	// DrainPhaseShutdownCalled means the simplyblock shutdown API has been called.
	DrainPhaseShutdownCalled = "shutdown_called"
	// DrainPhaseDraining means shutdown is confirmed and the PDB has been relaxed to allow pod eviction.
	DrainPhaseDraining = "draining"
	// DrainPhaseRestartCalled means the node SPDK stack is back and the simplyblock restart API has been called.
	DrainPhaseRestartCalled = "restart_called"
	// DrainPhaseComplete means the node is back online in the simplyblock cluster.
	DrainPhaseComplete = "complete"
	// DrainPhaseFailed means an unrecoverable error occurred during drain coordination.
	DrainPhaseFailed = "failed"
)

// NodeDrainState tracks the upgrade-drain coordination state for a single worker node.
type NodeDrainState struct {
	// Hostname is the Kubernetes node name.
	Hostname string `json:"hostname"`
	// Phase is the current drain coordination phase.
	// +kubebuilder:validation:Enum=detected;shutdown_called;draining;restart_called;complete;failed
	Phase string `json:"phase"`
	// StartedAt is when drain coordination began for this node.
	StartedAt metav1.Time `json:"startedAt"`
	// Message provides additional status detail or error information.
	Message string `json:"message,omitempty"`
	// ActiveNodeUUID is the backend UUID of the storage node currently being shut
	// down or restarted. Used to sequence through multiple NUMA-socket nodes on
	// the same worker one at a time during drain coordination.
	ActiveNodeUUID string `json:"activeNodeUUID,omitempty"`
}

// StorageNodeStatus defines the observed state of StorageNode.
type StorageNodeStatus struct {
	// Nodes is the observed state of each managed storage node.
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// ActionStatus tracks the latest action execution status.
	ActionStatus *ActionStatus `json:"actionStatus,omitempty"`
	// DrainCoordination tracks the upgrade-drain state per worker node.
	DrainCoordination []NodeDrainState `json:"drainCoordination,omitempty"`
}

type NodeStatus struct {
	// UUID is the backend node UUID.
	UUID string `json:"uuid,omitempty"`
	// Health indicates whether health checks are currently passing.
	Health bool `json:"health,omitempty"`
	// Status is the backend lifecycle state for the node.
	Status string `json:"status,omitempty"`
	// CPU is the reported CPU allocation/count for the node.
	CPU *int32 `json:"cpu,omitempty"`
	// Memory is the reported memory value.
	Memory string `json:"memory,omitempty"`
	// Volumes is the current logical volume count.
	Volumes *int32 `json:"volumes,omitempty"`
	// RpcPort is the node RPC service port.
	RpcPort *int32 `json:"rpcPort,omitempty"`
	// LvolPort is the logical-volume subsystem port.
	LvolPort *int32 `json:"lvolPort,omitempty"`
	// NvmfPort is the NVMf service port.
	NvmfPort *int32 `json:"nvmfPort,omitempty"`
	// Devices is the backend summary of devices on this node.
	Devices string `json:"devices,omitempty"`
	// Uptime is the reported node uptime value.
	Uptime string `json:"uptime,omitempty"`
	// Hostname is the Kubernetes node hostname.
	Hostname string `json:"hostname,omitempty"`
	// MgmtIp is the management IP address for the node.
	MgmtIp string `json:"mgmtIp,omitempty"`
}

type ActionStatus struct {
	// Action is the requested action name.
	Action string `json:"action,omitempty"`
	// NodeUUID is the target node UUID for the action.
	NodeUUID string `json:"nodeUUID,omitempty"`
	State    string `json:"state,omitempty"` // pending | running | success | failed
	// Message is a human-readable action result or error.
	Message string `json:"message,omitempty"`
	// UpdatedAt is the timestamp of the last status transition.
	UpdatedAt metav1.Time `json:"updatedAt,omitempty"`
	// ObservedGeneration is the resource generation observed by this status.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Triggered indicates whether the underlying backend action has been fired.
	Triggered bool `json:"triggered,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.action) && self.spec.action != \"\" && (!has(self.spec.nodeUUID) || self.spec.nodeUUID == \"\"))",message="nodeUUID is required when action is specified"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.action) && self.spec.action != \"\") || (has(self.spec.clusterImage) && self.spec.clusterImage != \"\" && has(self.spec.maxLogicalVolumeCount) && has(self.spec.workerNodes) && size(self.spec.workerNodes) > 0)",message="clusterImage, maxLogicalVolumeCount, and workerNodes are required when action is not specified"
// StorageNode is the Schema for the storagenodes API
type StorageNode struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageNode
	// +required
	Spec StorageNodeSpec `json:"spec"`

	// status defines the observed state of StorageNode
	// +optional
	Status StorageNodeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageNodeList contains a list of StorageNode
type StorageNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageNode{}, &StorageNodeList{})
}
