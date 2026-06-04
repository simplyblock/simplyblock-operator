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
}

// StorageNodeSpec defines the desired state of StorageNode
type StorageNodeSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Image"
	// ClusterImage is the container image used for storage-node workloads.
	ClusterImage string `json:"clusterImage,omitempty"`
	// +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Action"
	// Action triggers an imperative node operation.
	Action string `json:"action,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Node UUID"
	// NodeUUID is required when action is specified
	NodeUUID string `json:"nodeUUID,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Logical Volume Count"
	// MaxLogicalVolumeCount is the maximum number of logical volumes per node.
	MaxLogicalVolumeCount *int32 `json:"maxLogicalVolumeCount,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Max Size"
	// MaxSize is the maximum allocatable size of huge pages.
	MaxSize string `json:"maxSize,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="SPDK Image"
	// SpdkImage is the SPDK image reference used by node services.
	SpdkImage string `json:"spdkImage,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="SPDK Proxy Image"
	// SpdkProxyImage is the SPDK proxy image reference used by node services.
	SpdkProxyImage string `json:"spdkProxyImage,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Management Interface"
	// MgmtIfname is the management interface name used by storage nodes.
	MgmtIfname string `json:"mgmtIfname,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Partitions"
	// Partitions is the number of partitions created per backend storage device.
	Partitions *int32 `json:"partitions,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Journal Manager"
	// JournalManagerSpec configures journal manager behavior.
	JournalManagerSpec *JournalManagerSpec `json:"journalManager,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Core Percentage"
	// CorePercentage is the percentage of cores to be used for spdk (0-99).
	CorePercentage *int32 `json:"corePercentage,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PCIe Allow List"
	// PcieAllowList is the list of PCI addresses allowed for use.
	PcieAllowList []string `json:"pcieAllowList,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PCIe Deny List"
	// PcieDenyList is the list of PCI addresses excluded from use.
	PcieDenyList []string `json:"pcieDenyList,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="PCIe Model"
	// PcieModel filters devices by PCI model.
	PcieModel string `json:"pcieModel,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Drive Size Range"
	// DriveSizeRange filters devices by size range.
	DriveSizeRange string `json:"driveSizeRange,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Sockets To Use"
	// SocketsToUse restricts deployment to selected NUMA sockets.
	SocketsToUse []string `json:"socketsToUse,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Nodes Per Socket"
	// NodesPerSocket defines how many storage nodes are created per NUMA socket.
	NodesPerSocket *int32 `json:"nodesPerSocket,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Data Interfaces"
	// DataIfname lists data-plane network interfaces.
	DataIfname []string `json:"dataIfname,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Worker Nodes"
	// WorkerNodes is the set of Kubernetes worker nodes to manage.
	WorkerNodes []string `json:"workerNodes,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Worker Node"
	// WorkerNode is a single worker node used by action flows.
	WorkerNode string `json:"workerNode,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Reattach Volume"
	// ReattachVolume reattaches volumes during restart where supported by the backend.
	ReattachVolume *bool `json:"reattachVolume,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="OpenShift Cluster"
	// OpenShiftCluster indicates OpenShift-specific behavior should be enabled.
	OpenShiftCluster *bool `json:"openShiftCluster,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Device Names"
	// DeviceNames explicitly defines a comma separated list of nvme namespace names like nvme0n1,nvme1n1...
	DeviceNames []string `json:"deviceNames,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Ubuntu Host"
	// UbuntuHost indicates the node host OS is Ubuntu.
	UbuntuHost *bool `json:"ubuntuHost,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Skip Kubelet Configuration"
	// SkipKubeletConfiguration skips kubelet configuration changes.
	SkipKubeletConfiguration *bool `json:"skipKubeletConfiguration,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Force Format 4K"
	// ForceFormat4K forces 4K blocksize formatting of the NVMe device where supported.
	ForceFormat4K *bool `json:"forceFormat4K,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Enable CPU Topology"
	// EnableCpuTopology enables topology-aware CPU handling.
	EnableCpuTopology *bool `json:"enableCpuTopology,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Reserved System CPU"
	// ReservedSystemCPU defines CPUs reserved for system workloads.
	ReservedSystemCPU string `json:"reservedSystemCPU,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="SPDK System Memory"
	// +kubebuilder:validation:Pattern=`^[0-9]+(G|GI|GB|GiB|M|MI|MB|MiB|g|gi|gb|gib|m|mi|mb|mib)?$`
	// SpdkSystemMemory is the amount of memory reserved for SPDK system use (e.g. "4G", "512M").
	// When omitted the backend default is used.
	SpdkSystemMemory string `json:"spdkSystemMemory,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Tolerations"
	// Tolerations configures pod tolerations for storage-node pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Force"
	// Force enables forced action execution where supported.
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
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Nodes"
	// Nodes is the observed state of each managed storage node.
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// ActionStatus tracks the latest action execution status.
	ActionStatus *ActionStatus `json:"actionStatus,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Drain Coordination"
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
	// PostedAt is when the storage-node add request was sent. Used to detect
	// timeout without blocking the reconcile goroutine.
	PostedAt *metav1.Time `json:"postedAt,omitempty"`
}

type ActionStatus struct {
	// Action is the requested action name.
	Action string `json:"action,omitempty"`
	// NodeUUID is the target node UUID for the action.
	NodeUUID string `json:"nodeUUID,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Action State"
	State string `json:"state,omitempty"` // pending | running | success | failed
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Action Message"
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
// +kubebuilder:validation:XValidation:rule="(has(self.spec.action) && self.spec.action != \"\") || (has(self.spec.maxLogicalVolumeCount) && has(self.spec.workerNodes) && size(self.spec.workerNodes) > 0 && has(self.spec.mgmtIfname) && self.spec.mgmtIfname != \"\")",message="maxLogicalVolumeCount, workerNodes, and mgmtIfname are required when action is not specified"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Node",resources={{ServiceAccount,v1,simplyblock-storage-node},{Service,v1,simplyblock-storage-node},{DaemonSet,v1,simplyblock-storage-node},{ClusterRole,v1,simplyblock-storage-node},{ClusterRoleBinding,v1,simplyblock-storage-node}}
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
