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

// TaskSpec defines the desired state of Task
type TaskSpec struct {
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Cluster Name"
	// ClusterName is the target storage cluster name.
	ClusterName string `json:"clusterName"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Task ID"
	// TaskID filters results to a specific backend task when set.
	TaskID string `json:"taskID,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Subtasks"
	// Subtasks includes related child subtasks when supported by the backend.
	// FIXME: Unused for now
	Subtasks bool `json:"subtasks,omitempty"`
}

// TaskStatus defines the observed state of Task.
type TaskStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Tasks"
	// Tasks is the currently reported task list for the query scope.
	Tasks []TaskEntry `json:"tasks,omitempty"`
}

type TaskEntry struct {
	// UUID is the backend task UUID.
	UUID string `json:"uuid,omitempty"`
	// TaskType is the backend task function/type name.
	TaskType string `json:"taskType,omitempty"`
	// TaskStatus is the backend lifecycle status for the task.
	TaskStatus string `json:"taskStatus,omitempty"`
	// TaskResult is the backend result payload/message.
	TaskResult string `json:"taskResult,omitempty"`
	// Canceled indicates whether the task was canceled.
	Canceled bool `json:"canceled,omitempty"`
	// ParentTask is the parent task UUID when this task is a subtask.
	// FIXME: Unused for now
	ParentTask string `json:"parentTask,omitempty"`
	// StartedAt is the backend-reported task start timestamp.
	// FIXME: Unused for now
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// Retried is the number of retry attempts made for the task.
	Retried *int32 `json:"retried,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +operator-sdk:csv:customresourcedefinitions:displayName="Task"

// Task is the Schema for the tasks API
type Task struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Task
	// +required
	Spec TaskSpec `json:"spec"`

	// status defines the observed state of Task
	// +optional
	Status TaskStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TaskList contains a list of Task
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Task `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
