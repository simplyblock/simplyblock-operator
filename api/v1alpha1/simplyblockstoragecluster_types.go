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

// SimplyBlockStorageClusterSpec defines the desired state of SimplyBlockStorageCluster
type SimplyBlockStorageClusterSpec struct {
	// Create-only
	MgmtIfc                string `json:"mgmtIfc,omitempty"`
	EnableNodeAffinity     *bool  `json:"enableNodeAffinity,omitempty"`
	StripeWdata            *int32 `json:"stripeWdata,omitempty"`
	StripeWparity          *int32 `json:"stripeWparity,omitempty"`
	HAType                 string `json:"haType,omitempty"`
	ClusterName            string `json:"clusterName,omitempty"`
	IsSingleNode           *bool  `json:"isSingleNode,omitempty"`
	StrictNodeAntiAffinity *bool  `json:"strictNodeAntiAffinity,omitempty"`
	QpairCount             *int32 `json:"qpairCount,omitempty"`
	DistrBs                *int32 `json:"distrBs,omitempty"`
	DistrChunkBs           *int32 `json:"distrChunkBs,omitempty"`
	BlkSize                *int32 `json:"blkSize,omitempty"`
	PageSizeInBlocks       *int32 `json:"pageSizeInBlocks,omitempty"`
	MaxQueueSize           *int32 `json:"maxQueueSize,omitempty"`
	InflightIOThreshold    *int32 `json:"inflightIOThreshold,omitempty"`
	Fabric                 string `json:"fabric,omitempty"`

	// Updatable
	QoSClasses             string `json:"qosClasses,omitempty"`
	CapWarn                *int32 `json:"capWarn,omitempty"`
	CapCrit                *int32 `json:"capCrit,omitempty"`
	ProvCapWarn            *int32 `json:"provCapWarn,omitempty"`
	ProvCapCrit            *int32 `json:"provCapCrit,omitempty"`
	LogDelInterval         string `json:"logDelInterval,omitempty"`
	MetricsRetentionPeriod string `json:"metricsRetentionPeriod,omitempty"`
	ClientQpairCount       *int32 `json:"clientQpairCount,omitempty"`
	IncludeStats           *bool  `json:"includeStats,omitempty"`
	StatsHistoryInSeconds  *int32 `json:"statsHistoryInSeconds,omitempty"`
	IncludeEventLog        *bool  `json:"includeEventLog,omitempty"`
	EventLogEntries        *int32 `json:"eventLogEntries,omitempty"`
}

// SimplyBlockStorageClusterStatus defines the observed state of SimplyBlockStorageCluster.
type SimplyBlockStorageClusterStatus struct {
	UUID         string       `json:"UUID,omitempty"`
	ClusterName  string       `json:"clusterName,omitempty"`
	Health       *bool        `json:"health,omitempty"`
	MgmtNodes    *int32       `json:"mgmtNodes,omitempty"`
	StorageNodes *int32       `json:"storageNodes,omitempty"`
	NQN          string       `json:"NQN,omitempty"`
	MgmtIp       string       `json:"mgmtIp,omitempty"`
	State        string       `json:"state,omitempty"`
	Rebalancing  *bool        `json:"rebalancing,omitempty"`
	MOD          string       `json:"MOD,omitempty"`
	SecretName   string       `json:"secretName,omitempty"`
	Message      string       `json:"message,omitempty"`
	LastUpdated  *metav1.Time `json:"lastUpdated,omitempty"`
	Created      *metav1.Time `json:"created,omitempty"`
	Configured   bool         `json:"configured,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SimplyBlockStorageCluster is the Schema for the simplyblockstorageclusters API
type SimplyBlockStorageCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockStorageCluster
	// +required
	Spec SimplyBlockStorageClusterSpec `json:"spec"`

	// status defines the observed state of SimplyBlockStorageCluster
	// +optional
	Status SimplyBlockStorageClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockStorageClusterList contains a list of SimplyBlockStorageCluster
type SimplyBlockStorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockStorageCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockStorageCluster{}, &SimplyBlockStorageClusterList{})
}
