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

// SimplyBlockLvolSpec defines the desired state of SimplyBlockLvol
type SimplyBlockLvolSpec struct {
	ClusterName string `json:"clusterName"`
	PoolName    string `json:"poolName"`
}

// SimplyBlockLvolStatus defines the observed state of SimplyBlockLvol.
type SimplyBlockLvolStatus struct {
	Lvols      []LvolStatus `json:"lvols,omitempty"`
	Configured bool         `json:"configured,omitempty"`
}

type LvolStatus struct {
	UUID           string      `json:"uuid,omitempty"`
	LvolName       string      `json:"lvolName,omitempty"`
	NodeUUID       []string    `json:"nodeUUID,omitempty"`
	Hostname       string      `json:"hostname,omitempty"`
	ClonedFromSnap string      `json:"clonedFromSnap,omitempty"`
	SnapName       string      `json:"snapName,omitempty"`
	NQN            string      `json:"nqn,omitempty"`
	SubsysPort     string      `json:"subsysPort,omitempty"`
	NamespaceID    string      `json:"namespaceID,omitempty"`
	BlobID         string      `json:"blobID,omitempty"`
	PoolUUID       string      `json:"poolUUID,omitempty"`
	PoolName       string      `json:"poolName,omitempty"`
	PvcName        string      `json:"pvcName,omitempty"`
	Status         string      `json:"status,omitempty"`
	HAType         string      `json:"haType,omitempty"`
	Health         bool        `json:"health,omitempty"`
	IsCrypto       bool        `json:"isCrypto,omitempty"`
	Size           string      `json:"size,omitempty"`
	StripeWdata    int64       `json:"stripeWdata,omitempty"`
	StripeWparity  int64       `json:"stripeWparity,omitempty"`
	CreateDt       metav1.Time `json:"createDt,omitempty"`
	UpdateDt       metav1.Time `json:"updateDt,omitempty"`

	QosIOPS  int64 `json:"qosIOPS,omitempty"`
	QosWTP   int64 `json:"qosWTP,omitempty"`
	QosRTP   int64 `json:"qosRTP,omitempty"`
	QosRWTP  int64 `json:"qosRWTP,omitempty"`
	QosClass int64 `json:"qosClass,omitempty"`

	MaxNamespacesPerSubsystem int64  `json:"maxNamespacesPerSubsystem,omitempty"`
	Fabric                    string `json:"fabric,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="LVOLs",type=integer,JSONPath=".status.lvols.length()"

// SimplyBlockLvol is the Schema for the simplyblocklvols API
type SimplyBlockLvol struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockLvol
	// +required
	Spec SimplyBlockLvolSpec `json:"spec"`

	// status defines the observed state of SimplyBlockLvol
	// +optional
	Status SimplyBlockLvolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockLvolList contains a list of SimplyBlockLvol
type SimplyBlockLvolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockLvol `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockLvol{}, &SimplyBlockLvolList{})
}
