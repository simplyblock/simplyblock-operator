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

// PoolSpec defines the desired state of Pool
type PoolSpec struct {
	Name          string `json:"name"`
	ClusterName   string `json:"clusterName"`
	Status        string `json:"status,omitempty"`
	CapacityLimit string `json:"capacityLimit,omitempty"`
	QoSIOPSLimit  *int32 `json:"qosIOPSLimit,omitempty"`
	RWLimit       *int32 `json:"rwLimit,omitempty"`
	RLimit        *int32 `json:"rLimit,omitempty"`
	WLimit        *int32 `json:"wLimit,omitempty"`
	Action        string `json:"action,omitempty"`
}

// PoolStatus defines the observed state of Pool.
type PoolStatus struct {
	UUID   string `json:"uuid,omitempty"`
	Status string `json:"status,omitempty"`
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
