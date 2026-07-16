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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageNodeOverrides holds per-node configuration that overrides the parent
// StorageNodeSet fleet defaults for a specific worker node. Populated by the
// StorageNodeSetReconciler from StorageNodeSet.spec.nodeConfigs[workerNode] on
// every reconcile. The StorageNodeSet is the single source of truth — users
// should not edit this struct directly on the StorageNode.
//
// Fields here mirror the configurable (non-immutable, non-infrastructure) fields
// of StorageNodeSetSpec. When a field is set here it takes precedence over the
// fleet default; when omitted the fleet default applies.
type StorageNodeOverrides struct {
	// MaxLogicalVolumeCount overrides the maximum number of logical volumes for this node.
	// +optional
	MaxLogicalVolumeCount *int32 `json:"maxLogicalVolumeCount,omitempty"`

	// MaxSize overrides the maximum allocatable size of huge pages for this node.
	// +optional
	MaxSize string `json:"maxSize,omitempty"`

	// SpdkImage overrides the SPDK image for this node (e.g. for phased rollouts).
	// +optional
	SpdkImage string `json:"spdkImage,omitempty"`

	// SpdkProxyImage overrides the SPDK proxy image for this node.
	// +optional
	SpdkProxyImage string `json:"spdkProxyImage,omitempty"`

	// CorePercentage overrides the percentage of cores allocated to SPDK for this node (0-99).
	// +optional
	CorePercentage *int32 `json:"corePercentage,omitempty"`

	// SpdkSystemMemory overrides the SPDK huge-page memory allocation for this node
	// (e.g. "4G", "512M").
	// +kubebuilder:validation:Pattern=`^[0-9]+(G|GI|GB|GiB|M|MI|MB|MiB|g|gi|gb|gib|m|mi|mb|mib)?$`
	// +optional
	SpdkSystemMemory string `json:"spdkSystemMemory,omitempty"`

	// JournalManagerSpec overrides journal manager tuning for this node.
	// +optional
	JournalManagerSpec *JournalManagerSpec `json:"journalManager,omitempty"`

	// PcieAllowList overrides the list of PCI addresses allowed for use on this node.
	// +optional
	PcieAllowList []string `json:"pcieAllowList,omitempty"`

	// PcieDenyList overrides the list of PCI addresses excluded from use on this node.
	// +optional
	PcieDenyList []string `json:"pcieDenyList,omitempty"`

	// PcieModel overrides the PCI model filter for this node.
	// +optional
	PcieModel string `json:"pcieModel,omitempty"`

	// DriveSizeRange overrides the drive size range filter for this node.
	// +optional
	DriveSizeRange string `json:"driveSizeRange,omitempty"`

	// DeviceNames explicitly defines the NVMe namespace names to use on this node
	// (e.g. ["nvme0n1","nvme1n1"]).
	// +optional
	DeviceNames []string `json:"deviceNames,omitempty"`

	// EnableCpuTopology overrides topology-aware CPU handling for this node.
	// +optional
	EnableCpuTopology *bool `json:"enableCpuTopology,omitempty"`

	// ReservedSystemCPU overrides the CPUs reserved for system workloads on this node.
	// +optional
	ReservedSystemCPU string `json:"reservedSystemCPU,omitempty"`

	// UbuntuHost overrides the Ubuntu host OS flag for this node.
	// +optional
	UbuntuHost *bool `json:"ubuntuHost,omitempty"`

	// SkipKubeletConfiguration overrides whether kubelet configuration changes are
	// skipped for this node.
	// +optional
	SkipKubeletConfiguration *bool `json:"skipKubeletConfiguration,omitempty"`

	// FailureDomain is the failure-domain group index (≥ 1) for this node.
	// Required when the parent StorageCluster has enableFailureDomains=true.
	// Overrides StorageNodeSet.spec.nodeFailureDomains[workerNode] when both are set.
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureDomain *int32 `json:"failureDomain,omitempty"`

	// Expand marks this node as a cluster-expansion add. When true the backend
	// node-add endpoint receives expand=true, triggering rebalancing behaviour
	// appropriate for in-place cluster growth. Overrides StorageNodeSet.spec.expand.
	// +optional
	Expand *bool `json:"expand,omitempty"`
}

// StorageNodeSpec defines the desired state of a StorageNode.
type StorageNodeSpec struct {
	// StorageNodeSetRef is the name of the owning StorageNodeSet. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	StorageNodeSetRef string `json:"storageNodeSetRef"`

	// WorkerNode is the Kubernetes node hostname this StorageNode runs on. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	WorkerNode string `json:"workerNode"`

	// SocketIndex is the NUMA socket index (0-based). Immutable.
	// +k8s:immutable
	// +optional
	SocketIndex *int32 `json:"socketIndex,omitempty"`

	// Overrides holds per-node configuration propagated from
	// StorageNodeSet.spec.nodeConfigs[workerNode] on every reconcile.
	// +optional
	Overrides *StorageNodeOverrides `json:"overrides,omitempty"`
}

// StorageNodeResources groups compute and storage resource fields reported by the backend.
type StorageNodeResources struct {
	// CPU is the number of SPDK CPU cores allocated to this node.
	// +optional
	CPU *int32 `json:"cpu,omitempty"`
	// Memory is the SPDK memory allocation reported by the backend.
	// +optional
	Memory string `json:"memory,omitempty"`
	// Volumes is the current number of logical volumes on this node.
	// +optional
	Volumes *int32 `json:"volumes,omitempty"`
	// Devices is the device summary (online/total) reported by the backend.
	// +optional
	Devices string `json:"devices,omitempty"`
}

// StorageNodePorts groups the network port and address fields reported by the backend.
type StorageNodePorts struct {
	// Management is the management IP address of the node.
	// +optional
	Management string `json:"management,omitempty"`
	// NvmeOf is the NVMe-oF fabric port.
	// +optional
	NvmeOf *int32 `json:"nvmeof,omitempty"`
	// Lvol is the logical-volume subsystem port.
	// +optional
	Lvol *int32 `json:"lvol,omitempty"`
	// Rpc is the RPC/management API port.
	// +optional
	Rpc *int32 `json:"rpc,omitempty"`
}

// StorageNodeStatus holds the observed state of a StorageNode.
type StorageNodeStatus struct {
	// UUID is the backend storage node UUID. Set once after node-add completes.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Status is the backend-reported node status (e.g. online, suspended, offline).
	// +optional
	Status string `json:"status,omitempty"`

	// Health is the backend-reported node health flag.
	// +optional
	Health bool `json:"health,omitempty"`

	// Hostname is the node hostname as reported by the backend.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// Uptime is the node uptime as reported by the backend.
	// +optional
	Uptime string `json:"uptime,omitempty"`

	// Resources groups compute and storage resource metrics.
	// +optional
	Resources *StorageNodeResources `json:"resources,omitempty"`

	// Ports groups network connectivity fields (addresses and ports).
	// +optional
	Ports *StorageNodePorts `json:"ports,omitempty"`

	// PostedAt is the timestamp when the node-add POST was sent.
	// Used as a provisioning guard against duplicate POSTs.
	// +optional
	PostedAt *metav1.Time `json:"postedAt,omitempty"`

	// ActiveOpsRef is the name of the currently active StorageNodeOps CR targeting
	// this node. Empty when no operation is in progress. Used for mutual exclusion.
	// +optional
	ActiveOpsRef string `json:"activeOpsRef,omitempty"`

	// LatencyMetrics holds the fio-measured baseline NVMe-oF latency for this node,
	// used by the volume rebalancer to make data-placement decisions.
	// +optional
	LatencyMetrics *NodeLatencyMetrics `json:"latencyMetrics,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sn
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".spec.workerNode"
// +kubebuilder:printcolumn:name="Socket",type=integer,JSONPath=".spec.socketIndex"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.uuid"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Health",type=boolean,JSONPath=".status.health"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNode is the Schema for a single backend storage node instance.
// One StorageNode CR exists per (workerNode, socketIndex) pair and is owned
// by the parent StorageNodeSet.
type StorageNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageNodeSpec   `json:"spec,omitempty"`
	Status StorageNodeStatus `json:"status,omitempty"`
}

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
