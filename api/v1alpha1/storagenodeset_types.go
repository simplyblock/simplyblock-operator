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

// StorageNodeSetSpec is an alias for StorageNodeSpec — identical schema, new resource name.
type StorageNodeSetSpec = StorageNodeSpec

// StorageNodeSetStatus is an alias for StorageNodeStatus — identical schema, new resource name.
type StorageNodeSetStatus = StorageNodeStatus

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.action) && self.spec.action != \"\" && (!has(self.spec.nodeUUID) || self.spec.nodeUUID == \"\"))",message="nodeUUID is required when action is specified"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.action) && self.spec.action != \"\") || (has(self.spec.maxLogicalVolumeCount) && has(self.spec.workerNodes) && size(self.spec.workerNodes) > 0 && has(self.spec.mgmtIfname) && self.spec.mgmtIfname != \"\")",message="maxLogicalVolumeCount, workerNodes, and mgmtIfname are required when action is not specified"
// +operator-sdk:csv:customresourcedefinitions:displayName="Storage Node Set",resources={{ServiceAccount,v1,simplyblock-storage-node},{Service,v1,simplyblock-storage-node},{DaemonSet,v1,simplyblock-storage-node},{ClusterRole,v1,simplyblock-storage-node},{ClusterRoleBinding,v1,simplyblock-storage-node}}
// StorageNodeSet is the Schema for the storagenodessets API.
// It is the intended replacement for StorageNode and carries an identical spec and status.
type StorageNodeSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StorageNodeSet
	// +required
	Spec StorageNodeSetSpec `json:"spec"`

	// status defines the observed state of StorageNodeSet
	// +optional
	Status StorageNodeSetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StorageNodeSetList contains a list of StorageNodeSet
type StorageNodeSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StorageNodeSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageNodeSet{}, &StorageNodeSetList{})
}
