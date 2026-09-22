// StorageClusterOps in the shape design-storagecluster.md Appendix B
// specifies: one operation performed against one StorageCluster, which runs to
// a terminal phase and stays afterward as the record of what was done.
//
// The version that shipped first carried only the property renames of
// design-property-renames.md §2.1, §2.2, and §2.5. What this adds is the rest
// of §5 and §7: a status.step holding a declared state machine's snapshot, an
// Aborted phase and the spec.abort that reaches it, the CancelTask action, and
// a rolling-restart status re-shaped from two draining lists into one immutable
// list and an index into it.
//
// Two fields go with that, and their absence is the point (§6.2).
// status.triggered and status.rollingRestart.phaseTriggered each answered
// "has this step's call already fired," and the persisted step answers it
// instead: every step's completion condition is a predicate over current state
// rather than an observation of a transition, and every call is skipped when
// its target is already at or past what the call would produce. A step recorded
// without its side effect having fired is therefore indistinguishable from one
// whose did, and safe.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// StorageClusterOpsPhase is the operation's own progress. Aborted is terminal
// and distinct from Failed, because a canceled operation did not go wrong.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageClusterOpsPhase string

const (
	// StorageClusterOpsPhasePending: the operation holds no lock and has
	// issued nothing. It is both where an operation starts and where it waits
	// behind another one's lock.
	StorageClusterOpsPhasePending StorageClusterOpsPhase = "Pending"

	// StorageClusterOpsPhaseRunning: the operation holds the lock and its
	// first side effect may have been issued.
	StorageClusterOpsPhaseRunning StorageClusterOpsPhase = "Running"

	StorageClusterOpsPhaseSucceeded StorageClusterOpsPhase = "Succeeded"
	StorageClusterOpsPhaseFailed    StorageClusterOpsPhase = "Failed"

	// StorageClusterOpsPhaseAborted: the operation was called off. A rolling
	// restart holding on a degraded cluster is the one operation an
	// administrator has a reason to stop, and stopping it is not a failure.
	StorageClusterOpsPhaseAborted StorageClusterOpsPhase = "Aborted"
)

// StorageClusterOpsAction is the operation a StorageClusterOps performs. The
// values are PascalCase, as every enum in the group is; v1alpha1 spelled them
// lowercase and hyphenated, and the conversion maps between the two.
// +kubebuilder:validation:Enum=Activate;Expand;Shutdown;Start;Restart;RollingRestart;CancelTask
type StorageClusterOpsAction string

const (
	StorageClusterOpsActionActivate       StorageClusterOpsAction = "Activate"
	StorageClusterOpsActionExpand         StorageClusterOpsAction = "Expand"
	StorageClusterOpsActionShutdown       StorageClusterOpsAction = "Shutdown"
	StorageClusterOpsActionStart          StorageClusterOpsAction = "Start"
	StorageClusterOpsActionRestart        StorageClusterOpsAction = "Restart"
	StorageClusterOpsActionRollingRestart StorageClusterOpsAction = "RollingRestart"

	// StorageClusterOpsActionCancelTask is the one action whose target is
	// inside the cluster: it names a task of the cluster's status.tasks by its
	// control-plane ID.
	StorageClusterOpsActionCancelTask StorageClusterOpsAction = "CancelTask"
)

// StorageClusterOpsStep is one step of a running cluster operation. The enum is
// the union of every action's steps; which steps belong to which action is
// declared by the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Awaiting;ShuttingDown;Starting;CheckingPeers;ShuttingDownNode;RefreshingPod;AwaitingPod;RestartingNode;Rebalancing
type StorageClusterOpsStep string

const (
	// Activate, Expand, Shutdown, Start, and CancelTask.
	StorageClusterOpsStepRequesting StorageClusterOpsStep = "Requesting"
	StorageClusterOpsStepAwaiting   StorageClusterOpsStep = "Awaiting"

	// Restart.
	StorageClusterOpsStepShuttingDown StorageClusterOpsStep = "ShuttingDown"
	StorageClusterOpsStepStarting     StorageClusterOpsStep = "Starting"

	// RollingRestart, one machine lifetime per node.
	StorageClusterOpsStepCheckingPeers    StorageClusterOpsStep = "CheckingPeers"
	StorageClusterOpsStepShuttingDownNode StorageClusterOpsStep = "ShuttingDownNode"
	StorageClusterOpsStepRefreshingPod    StorageClusterOpsStep = "RefreshingPod"
	StorageClusterOpsStepAwaitingPod      StorageClusterOpsStep = "AwaitingPod"
	StorageClusterOpsStepRestartingNode   StorageClusterOpsStep = "RestartingNode"
	StorageClusterOpsStepRebalancing      StorageClusterOpsStep = "Rebalancing"
)

// RollingRestartSpec parameterizes the RollingRestart action.
type RollingRestartSpec struct {
	// RefreshSNodeAPI restarts each node's storage-node DaemonSet pod between
	// its shutdown and its restart, so the latest image is running when the
	// node returns.
	// +optional
	RefreshSNodeAPI bool `json:"refreshSNodeAPI,omitempty"`
}

// CancelTaskSpec parameterizes the CancelTask action.
type CancelTaskSpec struct {
	// TaskID names the entry of StorageCluster.status.tasks to cancel, by the
	// control plane's identifier for it rather than by its position in the
	// list.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TaskID string `json:"taskID"`
}

// StorageClusterOpsSpec is one operation to perform against one StorageCluster.
type StorageClusterOpsSpec struct {
	// ClusterRef names the StorageCluster this operation acts on. The operation
	// never owns its target, because deleting the record of an operation must
	// not delete the cluster it operated on.
	//
	// Bounded at what a StorageCluster name may be, since a longer value names
	// nothing that can exist (design-api-upgrade.md §19.4).
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageClusterOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// Whether an abort is expressible from the current step is declared by that
	// action's graph rather than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// CancelTask parameterizes action CancelTask and is ignored by the others.
	// +optional
	CancelTask *CancelTaskSpec `json:"cancelTask,omitempty"`

	// RollingRestart parameterizes action RollingRestart and is ignored by the
	// others.
	// +optional
	RollingRestart *RollingRestartSpec `json:"rollingRestart,omitempty"`
}

// RollingRestartStatus is the walk's position over the cluster's nodes. Where
// the machine has got to within the node currently being restarted is
// status.step, and neither field is complete without the other.
type RollingRestartStatus struct {
	// Nodes is the ordered list of storage node UUIDs this action covers,
	// written once when the walk starts and not modified afterward, so a node
	// added mid-walk is not restarted and one removed mid-walk is skipped when
	// the walk reaches it.
	// +optional
	Nodes []string `json:"nodes,omitempty"`

	// NodeIndex is the position in Nodes of the node being restarted. Advancing
	// the walk increments it, and the walk is complete when it reaches
	// len(Nodes).
	//
	// No omitempty: zero is a valid index, and a field that disappears at zero
	// makes "the first node" and "unset" the same wire value.
	// +kubebuilder:validation:Minimum=0
	NodeIndex int32 `json:"nodeIndex"`
}

// StorageClusterOpsStatus is the observed state of one cluster operation.
type StorageClusterOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageClusterOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine, as the shared
	// statemachine.KubeSnapshot. The rule is what an Enum marker would do if a
	// marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Requesting','Awaiting','ShuttingDown','Starting','CheckingPeers','ShuttingDownNode','RefreshingPod','AwaitingPod','RestartingNode','Rebalancing']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// RollingRestart is the walk's position, set only for action
	// RollingRestart.
	// +optional
	RollingRestart *RollingRestartStatus `json:"rollingRestart,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation acquired its target's lock.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// v1alpha2 is the storage version in the manifests this repository ships, which
// are the ones a fresh install applies. A cluster installed today stores this
// shape from the first write and never converts anything, so the conversion
// webhook is inert there and is not deployed.
//
// An upgrade of an existing cluster is the other path, and it does not take this
// value. The upgrade tool applies these same CRDs with storage held at v1alpha1,
// because a server-side apply overwrites the live storage version and moving it
// before the conversion webhook is serving breaks every write. It flips to
// v1alpha2 with the storage rewrite once the migration has run
// (design-api-upgrade.md §24, design-property-renames.md §3.8).
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=scops
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageClusterOps is a single operation performed against one StorageCluster.
// It runs to a terminal phase and stays afterward as the audit record of what
// was done, to which cluster, with which parameters, and how it ended. Only one
// operation acts on a cluster at a time; a second is admitted, waits at
// Pending, and runs when the lock frees.
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
