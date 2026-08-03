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

// LogicalVolumeNamePrefix prefixes a LogicalVolume CR name derived from a
// control-plane volume id, keeping the name distinct and RFC1123-safe.
const LogicalVolumeNamePrefix = "lv-"

// LogicalVolumeName returns the CR name mirroring the given control-plane volume
// id. VolumeIDFromName is its inverse.
func LogicalVolumeName(volumeID string) string { return LogicalVolumeNamePrefix + volumeID }

// LogicalVolumeSpec is the identity of the control-plane volume this CR mirrors.
// A LogicalVolume is a read model of a backend volume, created and reconciled
// from the control-plane SSE stream (see internal/cpinformer); the operator does
// not provision volumes through this CR. The three ids together form the CSI
// volume handle "clusterID:poolID:volumeID".
type LogicalVolumeSpec struct {
	// ClusterID is the backend cluster UUID that owns the volume.
	// +required
	ClusterID string `json:"clusterID"`
	// PoolID is the backend storage-pool UUID that owns the volume.
	// +required
	PoolID string `json:"poolID"`
	// VolumeID is the backend volume UUID.
	// +required
	VolumeID string `json:"volumeID"`
}

// LogicalVolumeStatus is the observed state of the volume, as last seen on the
// control-plane stream.
type LogicalVolumeStatus struct {
	// Name is the backend volume name.
	Name string `json:"name,omitempty"`
	// PoolName is the backend pool name the volume belongs to.
	PoolName string `json:"poolName,omitempty"`
	// SizeBytes is the provisioned size in bytes.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// NQN is the NVMe Qualified Name the volume is exported under.
	NQN string `json:"nqn,omitempty"`
	// State is the backend lifecycle status (e.g. "online", "deleted").
	State string `json:"state,omitempty"`
	// ObservedGeneration is the generation last reconciled into this status.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".status.name",description="Backend volume name"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state",description="Backend lifecycle status"
// +kubebuilder:printcolumn:name="Size",type="integer",JSONPath=".status.sizeBytes",description="Provisioned size in bytes"
// +kubebuilder:printcolumn:name="Pool",type="string",JSONPath=".status.poolName",description="Backend pool name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="Logical Volume"

// LogicalVolume is the Schema for the logicalvolumes API. It is a read model of a
// control-plane volume, kept in sync by the SSE informer.
type LogicalVolume struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the mirrored control-plane volume
	// +required
	Spec LogicalVolumeSpec `json:"spec"`

	// status defines the observed state of the volume
	// +optional
	Status LogicalVolumeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LogicalVolumeList contains a list of LogicalVolume
type LogicalVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LogicalVolume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LogicalVolume{}, &LogicalVolumeList{})
}
