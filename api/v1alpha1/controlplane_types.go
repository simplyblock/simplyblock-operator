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

// ControlPlaneSpec holds configuration for the singleton ControlPlane resource
// created by the Helm chart.
type ControlPlaneSpec struct {
	// Image is the container image used for all simplyblock control-plane and
	// storage-node workloads (e.g. quay.io/simplyblock-io/simplyblock:26.2.2).
	// StorageNode CRs that omit spec.clusterImage inherit this value.
	// +optional
	Image string `json:"image,omitempty"`
}

// ControlPlaneStatus reflects the observed readiness of the simplyblock
// control plane (FDB + management API).
type ControlPlaneStatus struct {
	// Phase is Initializing while the control plane is not yet healthy,
	// and Ready once the FDB health check passes.
	// +kubebuilder:validation:Enum=Initializing;Ready
	Phase string `json:"phase,omitempty"`

	// Message contains a human-readable explanation of the current phase,
	// for example the FDB error returned by the health endpoint.
	Message string `json:"message,omitempty"`

	// LastChecked is the timestamp of the most recent FDB health probe.
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Initializing while FDB is not ready; Ready once the control plane is operational"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.message",description="Human-readable status detail"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ControlPlane is a singleton resource (one per namespace, named "simplyblock")
// that reflects the readiness of the simplyblock control plane. It is created
// automatically by the Helm chart and should not be created or deleted manually.
type ControlPlane struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec ControlPlaneSpec `json:"spec,omitempty"`

	// +optional
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane resources.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlane{}, &ControlPlaneList{})
}
