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

// SimplyBlockStorageNodeSpec defines the desired state of StorageNode
type SimplyBlockStorageNodeSpec struct {
	ClusterName  string `json:"clusterName"`
	ClusterImage string `json:"clusterImage,omitempty"`
	// +kubebuilder:validation:Enum=shutdown;restart;suspend;resume;remove
	Action string `json:"action,omitempty"`
	// NodeUUID is required when action is specified
	NodeUUID string `json:"nodeUUID,omitempty"`

	UseSeparateJournalDevice *bool    `json:"useSeparateJournalDevice,omitempty"`
	MaxLVol                  *int32   `json:"maxLVol,omitempty"`
	MaxSize                  string   `json:"maxSize,omitempty"`
	SpdkImage                string   `json:"spdkImage,omitempty"`
	MgmtIfc                  string   `json:"mgmtIfc,omitempty"`
	Partitions               *int32   `json:"partitions,omitempty"`
	JMPercent                *int32   `json:"jmPercent,omitempty"`
	HAJM                     *bool    `json:"haJM,omitempty"`
	SPDKDebug                *bool    `json:"spdkDebug,omitempty"`
	IdDeviceByNQN            *bool    `json:"idDeviceByNQN,omitempty"`
	CoreIsolation            *bool    `json:"coreIsolation,omitempty"`
	CorePercentage           *int32   `json:"corePercentage,omitempty"`
	CoreMask                 string   `json:"coreMask,omitempty"`
	PcieAllowList            []string `json:"pcieAllowList,omitempty"`
	PcieDenyList             []string `json:"pcieDenyList,omitempty"`
	PcieModel                string   `json:"pcieModel,omitempty"`
	DriveSizeRange           string   `json:"driveSizeRange,omitempty"`
	SocketsToUse             *int32   `json:"socketsToUse,omitempty"`
	NodesPerSocket           *int32   `json:"nodesPerSocket,omitempty"`
	DataNIC                  []string `json:"dataNIC,omitempty"`
	HaJmCount                *int32   `json:"haJmCount,omitempty"`
	WorkerNodes              []string `json:"workerNodes,omitempty"`
	WorkerNode               string   `json:"workerNode,omitempty"`
	OpenShiftCluster         *bool    `json:"openShiftCluster,omitempty"`

	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// restart params
	AddPcieToAllowList []string `json:"addPcieToAllowList,omitempty"`
	NodeAddr           string   `json:"nodeAddr,omitempty"`
	Force              *bool    `json:"force,omitempty"`
}

// SimplyBlockStorageNodeStatus defines the observed state of StorageNode.
type SimplyBlockStorageNodeStatus struct {
	Nodes        []NodeStatus  `json:"nodes,omitempty"`
	ActionStatus *ActionStatus `json:"actionStatus,omitempty"`
}

type NodeStatus struct {
	UUID      string `json:"uuid,omitempty"`
	Health    bool   `json:"health,omitempty"`
	Status    string `json:"status,omitempty"`
	CPU       *int32 `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Volumes   *int32 `json:"volumes,omitempty"`
	RPC_PORT  *int32 `json:"rpc_port,omitempty"`
	LVOL_PORT *int32 `json:"lvol_port,omitempty"`
	NVMF_PORT *int32 `json:"nvmf_port,omitempty"`
	Devices   string `json:"devices,omitempty"`
	Uptime    string `json:"uptime,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	MgmtIp    string `json:"mgmtIp,omitempty"`
}

type ActionStatus struct {
	Action             string      `json:"action,omitempty"`
	NodeUUID           string      `json:"nodeUUID,omitempty"`
	State              string      `json:"state,omitempty"` // pending | running | success | failed
	Message            string      `json:"message,omitempty"`
	UpdatedAt          metav1.Time `json:"updatedAt,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Triggered          bool        `json:"triggered,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.action) && self.spec.action != \"\" && (!has(self.spec.nodeUUID) || self.spec.nodeUUID == \"\"))",message="nodeUUID is required when action is specified"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.action) && self.spec.action != \"\") || (has(self.spec.clusterImage) && self.spec.clusterImage != \"\" && has(self.spec.maxLVol) && has(self.spec.workerNodes) && size(self.spec.workerNodes) > 0)",message="clusterImage, maxLVol, and workerNodes are required when action is not specified"
// SimplyBlockStorageNode is the Schema for the storagenodes API
type SimplyBlockStorageNode struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageNode
	// +required
	Spec SimplyBlockStorageNodeSpec `json:"spec"`

	// status defines the observed state of StorageNode
	// +optional
	Status SimplyBlockStorageNodeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockStorageNodeList contains a list of StorageNode
type SimplyBlockStorageNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockStorageNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockStorageNode{}, &SimplyBlockStorageNodeList{})
}
