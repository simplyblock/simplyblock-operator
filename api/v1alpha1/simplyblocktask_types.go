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

// SimplyBlockTaskSpec defines the desired state of SimplyBlockTask
type SimplyBlockTaskSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	ClusterName string `json:"clusterName"`
	TaskID      string `json:"taskID,omitempty"`
	Subtasks    bool   `json:"subtasks,omitempty"`
}

// SimplyBlockTaskStatus defines the observed state of SimplyBlockTask.
type SimplyBlockTaskStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the SimplyBlockTask resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Tasks []TaskEntry `json:"tasks,omitempty"`
}

type TaskEntry struct {
	UUID       string       `json:"uuid,omitempty"`
	TaskType   string       `json:"taskType,omitempty"`
	TaskStatus string       `json:"taskStatus,omitempty"`
	TaskResult string       `json:"taskResult,omitempty"`
	Canceled   bool         `json:"canceled,omitempty"`
	ParentTask string       `json:"parentTask,omitempty"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	Retried    *int32       `json:"retried,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SimplyBlockTask is the Schema for the simplyblocktasks API
type SimplyBlockTask struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimplyBlockTask
	// +required
	Spec SimplyBlockTaskSpec `json:"spec"`

	// status defines the observed state of SimplyBlockTask
	// +optional
	Status SimplyBlockTaskStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimplyBlockTaskList contains a list of SimplyBlockTask
type SimplyBlockTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimplyBlockTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimplyBlockTask{}, &SimplyBlockTaskList{})
}
