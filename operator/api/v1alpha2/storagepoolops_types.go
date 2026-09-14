// StoragePoolOps: one imperative operation performed against one StoragePool.
//
// It replaces the unused spec.action field the registered pool carried, which is
// the construction design-crd-model.md §3 rejects because it allows one
// operation at a time, keeps no history, and cannot distinguish in-progress from
// done.
//
// The kind is introduced by the redesign, so it has no v1alpha1 spelling, no
// spoke to convert from, and no Hub method: its CRD declares one version.
//
// None of its actions exists yet, and that is deliberate rather than unfinished.
// A pool has fewer operations than a node because most of what changes about a
// pool is desired state: raising its capacity is an edit, restricting its nodes
// is an edit, and the node list is resolved on every reconcile. What is left is
// Rebalance, kept as a worked example of what a pool-level operation would look
// like, because it is the one with a real motivation and keeping it is cheaper
// than rediscovering it. design-storagepool.md §7 is the specification.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// StoragePoolOpsAction is the operation a StoragePoolOps performs. The kind
// holds one action and that action is provisional: nothing implements it and
// nothing depends on it. It is declared because the motivation is real and
// cheaper to keep than to rediscover, not because it is work in progress.
// +kubebuilder:validation:Enum=Rebalance
type StoragePoolOpsAction string

const (
	// StoragePoolOpsActionRebalance moves the pool's volumes off nodes
	// spec.allowedNodes no longer lists. It is a fan-out of PersistentVolumeOps
	// rather than a backend call: the control plane has no pool-granularity
	// rebalance and does not need one, because moving a volume is already an
	// operation this group has. The decision is the pool's and only the
	// execution is per-volume, which is why the action lives on this kind.
	StoragePoolOpsActionRebalance StoragePoolOpsAction = "Rebalance"
)

// StoragePoolOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StoragePoolOpsPhase string

const (
	// StoragePoolOpsPhasePending is an operation waiting for its target's lock.
	StoragePoolOpsPhasePending StoragePoolOpsPhase = "Pending"
	// StoragePoolOpsPhaseRunning is an operation holding the lock and working.
	StoragePoolOpsPhaseRunning StoragePoolOpsPhase = "Running"
	// StoragePoolOpsPhaseSucceeded is a finished operation that did what it said.
	StoragePoolOpsPhaseSucceeded StoragePoolOpsPhase = "Succeeded"
	// StoragePoolOpsPhaseFailed is a finished operation that did not.
	StoragePoolOpsPhaseFailed StoragePoolOpsPhase = "Failed"
	// StoragePoolOpsPhaseAborted is an operation stopped on request, whose
	// unwind has finished.
	StoragePoolOpsPhaseAborted StoragePoolOpsPhase = "Aborted"
)

// StoragePoolOpsStep is one step of a running pool operation.
// +kubebuilder:validation:Enum=Validating;Migrating
type StoragePoolOpsStep string

const (
	// StoragePoolOpsStepValidating works out which of the pool's volumes sit on
	// nodes that are no longer allowed, and writes the list before moving
	// anything.
	StoragePoolOpsStepValidating StoragePoolOpsStep = "Validating"
	// StoragePoolOpsStepMigrating creates one PersistentVolumeOps per volume in
	// that list and waits for each to reach a terminal phase, which is the same
	// fan-out a node drain performs.
	StoragePoolOpsStepMigrating StoragePoolOpsStep = "Migrating"
)

// StoragePoolOpsSpec is one operation to perform against one StoragePool.
type StoragePoolOpsSpec struct {
	// PoolRef names the StoragePool this operation acts on, in this object's own
	// namespace. The operation never owns its target, because deleting the
	// record of an operation must not delete the pool it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	PoolRef string `json:"poolRef"`

	// Action is the operation to perform. Immutable: an operation that changed
	// what it was doing halfway through would have a status describing neither.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StoragePoolOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// StoragePoolOpsStatus is the observed state of one pool operation.
type StoragePoolOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StoragePoolOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs. The rule repeats the
	// StoragePoolOpsStep enum because a marker cannot reach a field of the
	// shared snapshot type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Migrating']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

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

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=spops
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".spec.poolRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StoragePoolOps is a single operation performed against one StoragePool.
// Analogous to a Kubernetes Job: it drives an action to completion and records
// the result, and only one StoragePoolOps may be active per StoragePool at a
// time, which the pool's status.activeOpsRef enforces.
type StoragePoolOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StoragePoolOpsSpec   `json:"spec,omitempty"`
	Status StoragePoolOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StoragePoolOpsList contains a list of StoragePoolOps.
type StoragePoolOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StoragePoolOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StoragePoolOps{}, &StoragePoolOpsList{})
}
