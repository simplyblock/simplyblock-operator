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

// LvolSpec defines the desired state of Lvol
type LvolSpec struct {
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// PoolName is the target storage pool name.
	PoolName string `json:"poolName"`
}

// LvolStatus defines the observed state of Lvol.
type LvolStatus struct {
	// Lvols contains observed logical volume entries.
	Lvols []LvolEntry `json:"lvols,omitempty"`
	// Configured indicates whether initial Lvol reconciliation has completed.
	Configured bool `json:"configured,omitempty"`
}

type LvolQoSThroughput struct {
	// Read is the read throughput limit/metric.
	Read int64 `json:"read,omitempty"`
	// ReadWrite is the combined read/write throughput limit/metric.
	ReadWrite int64 `json:"readWrite,omitempty"`
	// Write is the write throughput limit/metric.
	Write int64 `json:"write,omitempty"`
}

type LvolQoS struct {
	// Class is the QosSpec class identifier.
	Class int64 `json:"class,omitempty"`
	// IOPS is the IOPS limit/metric.
	IOPS int64 `json:"iops,omitempty"`
	// Throughput contains throughput limits/metrics.
	Throughput LvolQoSThroughput `json:"throughput,omitempty"`
}

type LvolEntry struct {
	// UUID is the backend logical volume UUID.
	UUID string `json:"uuid,omitempty"`
	// LvolName is the logical volume name.
	LvolName string `json:"lvolName,omitempty"`
	// NodeUUID is the set of node UUIDs associated with the volume.
	NodeUUID []string `json:"nodeUUID,omitempty"`
	// Hostname is the node hostname associated with the volume.
	Hostname string `json:"hostname,omitempty"`
	// ClonedFromSnapshot is the source snapshot name/ID for clones.
	ClonedFromSnapshot string `json:"clonedFromSnapshot,omitempty"`
	// SourceSnapshotName is the source snapshot name used for this volume.
	SourceSnapshotName string `json:"sourceSnapshotName,omitempty"`
	// NQN is the NVMe Qualified Name for the volume.
	NQN string `json:"nqn,omitempty"`
	// SubsysPort is the NVMe subsystem/listener port.
	SubsysPort int64 `json:"subsysPort,omitempty"`
	// NamespaceID is the NVMe namespace identifier.
	NamespaceID int64 `json:"namespaceID,omitempty"`
	// BlobID is the backend blob identifier.
	BlobID int64 `json:"blobID,omitempty"`
	// PoolUUID is the backend storage pool UUID.
	PoolUUID string `json:"poolUUID,omitempty"`
	// PoolName is the storage pool name.
	PoolName string `json:"poolName,omitempty"`
	// PvcName is the bound Kubernetes PVC name when applicable.
	PvcName string `json:"pvcName,omitempty"`
	// Status is the backend lifecycle status.
	Status string `json:"status,omitempty"`
	// HA indicates whether high availability is enabled.
	HA bool `json:"ha,omitempty"`
	// Health indicates current health-check state.
	Health bool `json:"health,omitempty"`
	// IsEncrypted indicates whether encryption is enabled for the volume.
	IsEncrypted bool `json:"encrypted,omitempty"`
	// Size is the formatted volume size.
	Size string `json:"size,omitempty"`
	// Created is the backend creation timestamp.
	// FIXME: Unused for now
	Created metav1.Time `json:"created,omitempty"`
	// Updated is the backend last-update timestamp.
	// FIXME: Unused for now
	Updated metav1.Time `json:"updated,omitempty"`
	// QoS contains quality-of-service limits/metrics.
	QoS LvolQoS `json:"qos,omitempty"`
	// ErasureCodingScheme is the erasure coding layout, for example "2x1".
	ErasureCodingScheme string `json:"erasureCodingScheme,omitempty"`

	// MaxNamespacesPerSubsystem is the max number of namespaces per subsystem.
	MaxNamespacesPerSubsystem int64 `json:"maxNamespacesPerSubsystem,omitempty"`
	// Fabric is the storage fabric/protocol in use.
	Fabric string `json:"fabric,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="LVOLs",type=integer,JSONPath=".status.lvols.length()"

// Lvol is the Schema for the lvols API
type Lvol struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Lvol
	// +required
	Spec LvolSpec `json:"spec"`

	// status defines the observed state of Lvol
	// +optional
	Status LvolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LvolList contains a list of Lvol
type LvolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Lvol `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lvol{}, &LvolList{})
}
