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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SimplyBlockDeviceSpec defines the desired state of SimplyBlockDevice
type SimplyBlockDeviceSpec struct {
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// NodeUUID scopes operations to a single storage node when set.
	NodeUUID string `json:"nodeUUID,omitempty"`
	// DeviceID is the backend device identifier used for actions.
	DeviceID string `json:"deviceID,omitempty"`
	// +kubebuilder:validation:Enum=remove;restart
	// Action triggers an imperative device operation.
	Action string `json:"action,omitempty"`
}

// SimplyBlockDeviceStatus defines the observed state of SimplyBlockDevice.
type SimplyBlockDeviceStatus struct {
	// Nodes contains observed devices grouped by storage node.
	Nodes []NodeDevices `json:"nodes,omitempty"`
	// ActionStatus tracks the lifecycle of the latest device action.
	ActionStatus *ActionStatus `json:"actionStatus,omitempty"`
}

type NodeDevices struct {
	// NodeUUID is the backend node UUID owning the listed devices.
	NodeUUID string `json:"nodeUUID,omitempty"`
	// Devices is the observed device inventory for the node.
	Devices []DeviceInfo `json:"devices,omitempty"`
}

type DeviceInfo struct {
	// UUID is the backend device UUID.
	UUID string `json:"uuid,omitempty"`
	// Health is the backend health indicator for the device.
	Health string `json:"health,omitempty"`
	// Size is the formatted device capacity value.
	Size string `json:"size,omitempty"`
	// Model is the reported device model.
	Model string `json:"model,omitempty"`
	// Utilization is the backend utilization metric.
	Utilization int64 `json:"utilization,omitempty"`
	// Status is the backend lifecycle status of the device.
	Status string `json:"status,omitempty"`
	// Stats is the time-series/statistics collection for the device.
	Stats []DeviceStats `json:"stats,omitempty"`
}

type DeviceIOPSStats struct {
	// Read is the read IOPS metric.
	// FIXME: Unused for now
	Read int64 `json:"read,omitempty"`
	// Write is the write IOPS metric.
	// FIXME: Unused for now
	Write int64 `json:"write,omitempty"`
}

type DeviceThroughputStats struct {
	// Read is the read throughput metric.
	// FIXME: Unused for now
	Read int64 `json:"read,omitempty"`
	// Write is the write throughput metric.
	// FIXME: Unused for now
	Write int64 `json:"write,omitempty"`
}

type DeviceStats struct {
	// IOPS contains read/write IOPS values.
	// FIXME: Unused for now
	IOPS DeviceIOPSStats `json:"iops,omitempty"`
	// Throughput contains read/write throughput values.
	// FIXME: Unused for now
	Throughput DeviceThroughputStats `json:"throughput,omitempty"`
	// UtilizedCapacity is the used-capacity metric for the device.
	// FIXME: Unused for now
	UtilizedCapacity int64 `json:"utilizedCapacity,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.action) && self.spec.action != \"\" && ((!has(self.spec.nodeUUID) || self.spec.nodeUUID == \"\") || (!has(self.spec.deviceID) || self.spec.deviceID == \"\")))",message="nodeUUID and deviceID are required when action is specified"
// SimplyBlockDevice is the Schema for the simplyblockdevices API
type SimplyBlockDevice struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockDevice
	// +required
	Spec SimplyBlockDeviceSpec `json:"spec"`

	// status defines the observed state of SimplyBlockDevice
	// +optional
	Status SimplyBlockDeviceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockDeviceList contains a list of SimplyBlockDevice
type SimplyBlockDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockDevice{}, &SimplyBlockDeviceList{})
}
