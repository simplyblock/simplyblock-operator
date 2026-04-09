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

// PoolQoSThroughputSpec defines throughput QosSpec limits in MiB/s.
type PoolQoSThroughputSpec struct {
	// Read is the read throughput limit for the pool.
	Read *int32 `json:"read,omitempty"`
	// ReadWrite is the combined read/write throughput limit for the pool.
	ReadWrite *int32 `json:"readWrite,omitempty"`
	// Write is the write throughput limit for the pool.
	Write *int32 `json:"write,omitempty"`
}

// PoolQoSSpec defines pool QosSpec limits.
type PoolQoSSpec struct {
	// IOPS is the IOPS limit for the pool.
	IOPS *int32 `json:"iops,omitempty"`
	// Throughput contains throughput limits for the pool.
	Throughput *PoolQoSThroughputSpec `json:"throughput,omitempty"`
}

// PoolQoSThroughputStatus defines observed throughput QosSpec values in MiB/s.
type PoolQoSThroughputStatus struct {
	// Read is the observed/configured read throughput value.
	Read *int32 `json:"read,omitempty"`
	// ReadWrite is the observed/configured combined read/write throughput value.
	ReadWrite *int32 `json:"readWrite,omitempty"`
	// Write is the observed/configured write throughput value.
	Write *int32 `json:"write,omitempty"`
}

// PoolQoSStatus defines observed pool QosSpec values.
type PoolQoSStatus struct {
	// Host is the backend host handling pool QosSpec enforcement.
	Host string `json:"host,omitempty"`
	// IOPS is the observed/configured IOPS value.
	IOPS *int32 `json:"iops,omitempty"`
	// Throughput contains observed/configured throughput values.
	Throughput *PoolQoSThroughputStatus `json:"throughput,omitempty"`
}

// PoolSpec defines the desired state of Pool
type PoolSpec struct {
	// Name is the backend pool name.
	Name string `json:"name"`
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// Status is an optional desired-status hint for backend workflows.
	Status string `json:"status,omitempty"`
	// CapacityLimit is the maximum pool capacity.
	CapacityLimit string `json:"capacityLimit,omitempty"`
	// QosSpec defines QosSpec limits for the pool.
	QosSpec *PoolQoSSpec `json:"qos,omitempty"`
	// Action triggers an imperative pool operation.
	Action string `json:"action,omitempty"`
}

// PoolStatus defines the observed state of Pool.
type PoolStatus struct {
	// UUID is the backend pool UUID.
	UUID string `json:"uuid,omitempty"`
	// Status is the backend lifecycle status.
	Status string `json:"status,omitempty"`
	// QoS contains observed/configured QoS values.
	QoS *PoolQoSStatus `json:"qos,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Pool is the Schema for the pools API
type Pool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Pool
	// +required
	Spec PoolSpec `json:"spec"`

	// status defines the observed state of Pool
	// +optional
	Status PoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PoolList contains a list of Pool
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Pool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Pool{}, &PoolList{})
}
