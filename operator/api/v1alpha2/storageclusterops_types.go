// StorageClusterOps with the three properties the CRD redesign renames on it
// (design-storagecluster.md §5.3 and §7):
//
//   - spec.nodeRollingRestart becomes spec.rollingRestart, and its type loses the
//     Node prefix with it. The action restarts the cluster's nodes, so "node" in
//     the name said nothing the action did not.
//   - status.nodeRollingRestartStatus becomes status.rollingRestart, matching.
//   - spec.action becomes a named enum whose values are PascalCase, and
//     node-rolling-restart becomes RollingRestart.
//
// The status block's own re-shaping — nodes and nodeIndex replacing pendingNodes
// and processedNodes — is not here. That is the rolling restart's state machine
// rather than a rename, and it lands with the step machine
// (design-property-renames.md §2.2). The same goes for the removals of
// status.triggered and phaseTriggered.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageClusterOpsPhase is the lifecycle phase of a StorageClusterOps.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StorageClusterOpsPhase string

const (
	StorageClusterOpsPhasePending   StorageClusterOpsPhase = "Pending"
	StorageClusterOpsPhaseRunning   StorageClusterOpsPhase = "Running"
	StorageClusterOpsPhaseSucceeded StorageClusterOpsPhase = "Succeeded"
	StorageClusterOpsPhaseFailed    StorageClusterOpsPhase = "Failed"
)

// StorageClusterOpsAction is the operation a StorageClusterOps performs. The
// values are PascalCase, as every enum in the group is; v1alpha1 spelled them
// lowercase and hyphenated, and the conversion maps between the two.
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart
type StorageClusterOpsAction string

const (
	StorageClusterOpsActionActivate       StorageClusterOpsAction = "Activate"
	StorageClusterOpsActionExpand         StorageClusterOpsAction = "Expand"
	StorageClusterOpsActionShutdown       StorageClusterOpsAction = "Shutdown"
	StorageClusterOpsActionStart          StorageClusterOpsAction = "Start"
	StorageClusterOpsActionRestart        StorageClusterOpsAction = "Restart"
	StorageClusterOpsActionRollingRestart StorageClusterOpsAction = "RollingRestart"
)

// RollingRestartStatus tracks in-progress state for the RollingRestart action.
// All fields are persisted in the StorageClusterOps status so the reconciler
// can resume after a requeue or operator restart.
type RollingRestartStatus struct {
	// PendingNodes is the ordered list of node UUIDs still to be restarted.
	PendingNodes []string `json:"pendingNodes,omitempty"`
	// ProcessedNodes is the list of node UUIDs already restarted.
	ProcessedNodes []string `json:"processedNodes,omitempty"`
	// NodePhase is the current step for the node being restarted:
	// "snode-refresh" | "snode-refresh-wait" | "shutting-down" | "restarting" | "rebalancing"
	NodePhase string `json:"nodePhase,omitempty"`
	// PhaseTriggered indicates the API call for the current NodePhase was already sent.
	PhaseTriggered bool `json:"phaseTriggered,omitempty"`
}

// RollingRestartSpec configures the RollingRestart action behavior.
type RollingRestartSpec struct {
	// RefreshSNodeAPI restarts the storage-node DaemonSet pod on each node
	// after the backend node is shut down and before it is restarted, ensuring
	// the latest image is running before the node comes back online.
	// +optional
	RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`
}

// StorageClusterOpsSpec defines the desired state of a StorageClusterOps.
type StorageClusterOpsSpec struct {
	// ClusterRef is the name of the target StorageCluster. Immutable.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	ClusterRef string `json:"clusterRef"`

	// Action is the operation to perform. Immutable.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	Action StorageClusterOpsAction `json:"action"`

	// RollingRestart configures behavior specific to the RollingRestart action.
	// Ignored for all other actions.
	// +optional
	RollingRestart *RollingRestartSpec `json:"rollingRestart,omitempty"`
}

// StorageClusterOpsStatus holds the observed state of a StorageClusterOps.
type StorageClusterOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase StorageClusterOpsPhase `json:"phase,omitempty"`

	// Triggered indicates the backend POST has been sent for this operation.
	// Guards against duplicate backend calls on retry.
	// +optional
	Triggered bool `json:"triggered,omitempty"`

	// Message is a human-readable description of the current state or failure reason.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished (successfully or not).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// RollingRestart tracks per-node progress for the RollingRestart action.
	// Nil for all other actions.
	// +optional
	RollingRestart *RollingRestartStatus `json:"rollingRestart,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=scops
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageClusterOps is a one-shot operational CR targeting a single StorageCluster.
// Analogous to a Kubernetes Job — it drives a cluster-level operation (Activate, Expand,
// Shutdown, Restart, RollingRestart) to completion and records the result. Only one
// StorageClusterOps can be active per cluster at a time.
type StorageClusterOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClusterOpsSpec   `json:"spec,omitempty"`
	Status StorageClusterOpsStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for StorageClusterOps.
func (*StorageClusterOps) Hub() {}

// +kubebuilder:object:root=true

// StorageClusterOpsList contains a list of StorageClusterOps.
type StorageClusterOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClusterOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageClusterOps{}, &StorageClusterOpsList{})
}
