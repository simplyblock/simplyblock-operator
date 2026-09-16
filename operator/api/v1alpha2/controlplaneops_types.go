// ControlPlaneOps: one imperative operation performed against the control plane.
//
// Most of what an administrator does to a control plane is expressible as
// desired state, because the entity re-applies what it installed on every pass:
// changing the image is an edit, scaling FoundationDB is an edit, and an object
// somebody deleted by hand is put back. What is left is what this kind carries,
// and it is three things: recycling a workload, moving it to a new version, and
// asking FoundationDB for a backup.
//
// Every action requires a control plane this cluster hosts, since each acts on
// something the operator installed. An operation naming a remote one is rejected
// at admission rather than created and failed, which is what
// ControlPlaneOpsValidator is for.
//
// The kind is introduced by the redesign, so it has no v1alpha1 spelling, no
// spoke to convert from, and no Hub method: its CRD declares one version.
//
// design-controlplane.md §6 and Appendix B are the specification.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// ControlPlaneOpsAction is the operation a ControlPlaneOps performs. Every
// action acts on what the operator installed, so every action requires a managed
// control plane, and the validating webhook of §6 rejects an operation naming an
// external one at creation rather than letting it be created and fail.
// +kubebuilder:validation:Enum=Restart;Upgrade;Backup
type ControlPlaneOpsAction string

const (
	// ControlPlaneOpsActionRestart recycles a wedged workload. Its scope is
	// spec.restart.components, and an empty list recycles the whole control
	// plane.
	ControlPlaneOpsActionRestart ControlPlaneOpsAction = "Restart"
	// ControlPlaneOpsActionUpgrade moves the control plane to a new version and
	// verifies afterward that the version it reports is the one asked for.
	ControlPlaneOpsActionUpgrade ControlPlaneOpsAction = "Upgrade"
	// ControlPlaneOpsActionBackup asks FoundationDB for a backup outside
	// whatever schedule exists. It asks rather than implements: the snapshot is
	// the FoundationDB operator's to take.
	ControlPlaneOpsActionBackup ControlPlaneOpsAction = "Backup"
)

// ControlPlaneOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type ControlPlaneOpsPhase string

const (
	// ControlPlaneOpsPhasePending is an operation waiting for its target's lock.
	ControlPlaneOpsPhasePending ControlPlaneOpsPhase = "Pending"
	// ControlPlaneOpsPhaseRunning is an operation holding the lock and working.
	ControlPlaneOpsPhaseRunning ControlPlaneOpsPhase = "Running"
	// ControlPlaneOpsPhaseSucceeded is a finished operation that did what it
	// said.
	ControlPlaneOpsPhaseSucceeded ControlPlaneOpsPhase = "Succeeded"
	// ControlPlaneOpsPhaseFailed is a finished operation that did not.
	ControlPlaneOpsPhaseFailed ControlPlaneOpsPhase = "Failed"
	// ControlPlaneOpsPhaseAborted is an operation stopped on request, whose
	// unwind has finished.
	ControlPlaneOpsPhaseAborted ControlPlaneOpsPhase = "Aborted"
)

// ControlPlaneOpsStep is one step of a running control-plane operation. Which
// steps belong to which action is declared by that action's graph rather than by
// this type, which is why the enum stays flat as actions are added.
// +kubebuilder:validation:Enum=Draining;Restarting;Awaiting;Preflight;Applying;Verifying;Requesting
type ControlPlaneOpsStep string

const (
	// ControlPlaneOpsStepDraining holds while another operation in the namespace
	// is still running. Restart and Upgrade both recycle the management API, so
	// both drain.
	ControlPlaneOpsStepDraining ControlPlaneOpsStep = "Draining"

	// ControlPlaneOpsStepRestarting rolls the workloads the action named.
	ControlPlaneOpsStepRestarting ControlPlaneOpsStep = "Restarting"
	// ControlPlaneOpsStepAwaiting waits for what the previous step asked for:
	// the recycled pods coming back, or the backup reporting a snapshot.
	ControlPlaneOpsStepAwaiting ControlPlaneOpsStep = "Awaiting"

	// ControlPlaneOpsStepPreflight reads live state, which admission cannot: it
	// holds until the control plane is Available, and fails when the requested
	// image is the one already running.
	ControlPlaneOpsStepPreflight ControlPlaneOpsStep = "Preflight"
	// ControlPlaneOpsStepApplying writes the new image onto the entity, which is
	// what rolls the Deployment.
	ControlPlaneOpsStepApplying ControlPlaneOpsStep = "Applying"
	// ControlPlaneOpsStepVerifying re-probes and compares the reported version
	// against the one asked for, which is what makes an upgrade more than an
	// image bump.
	ControlPlaneOpsStepVerifying ControlPlaneOpsStep = "Verifying"

	// ControlPlaneOpsStepRequesting creates or triggers the FoundationDBBackup.
	// Awaiting is shared with Restart.
	ControlPlaneOpsStepRequesting ControlPlaneOpsStep = "Requesting"
)

// UpgradeSpec parameterizes the Upgrade action and is ignored by the others.
type UpgradeSpec struct {
	// Image is the version to move to. It replaces
	// ControlPlane.spec.source.managed.image when the operation succeeds, so the
	// entity keeps describing what is running.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +kubebuilder:validation:Required
	Image string `json:"image"`
}

// RestartSpec parameterizes the Restart action and is ignored by the others.
type RestartSpec struct {
	// Components names the workloads to recycle, from the table in §4.3. Empty
	// recycles the whole control plane. Naming only components that table marks
	// non-essential skips the drain, because recycling them interrupts nothing.
	// +listType=set
	// +optional
	Components []string `json:"components,omitempty"`
}

// BackupSpec parameterizes the Backup action and is ignored by the others.
type BackupSpec struct {
	// BlobStore is the destination, in the form the FoundationDBBackup CRD takes
	// it. The operator copies it through rather than interpreting it, since the
	// backup is the FoundationDB operator's to perform.
	// +kubebuilder:validation:Required
	BlobStore string `json:"blobStore"`

	// BackupName is the FoundationDBBackup to create or trigger. Absent uses the
	// one already configured for the cluster, and fails when there is none and
	// no name to create.
	// +optional
	BackupName string `json:"backupName,omitempty"`
}

// ControlPlaneOpsSpec is one operation to perform against the control plane.
//
// Everything except spec.abort is frozen once the object is admitted, which is
// what makes the status an audit of the request that ran rather than of whatever
// the object says now. The parameters are consumed several steps apart:
// Preflight reads spec.upgrade.image and Applying writes it, and Draining reads
// spec.restart.components before Restarting recycles them. An edit in between
// produces an operation that checked one thing and did another.
//
// The rules are declared here rather than as +k8s:immutable on each field.
// controller-gen emits that marker's rules in an order that varies between runs
// once a type carries several, and it freezes a block whole; what has to be
// frozen is each block's presence together with its contents.
// +kubebuilder:validation:XValidation:rule="has(self.upgrade) == has(oldSelf.upgrade) && (!has(self.upgrade) || self.upgrade == oldSelf.upgrade)",message="spec.upgrade is immutable: Preflight checked the image the operation was admitted with, and Applying writes it several steps later"
// +kubebuilder:validation:XValidation:rule="has(self.restart) == has(oldSelf.restart) && (!has(self.restart) || self.restart == oldSelf.restart)",message="spec.restart is immutable: the drain is decided from the component list, so widening it afterward skips a drain the wider list would have required"
// +kubebuilder:validation:XValidation:rule="has(self.backup) == has(oldSelf.backup) && (!has(self.backup) || self.backup == oldSelf.backup)",message="spec.backup is immutable: the destination is what Requesting created the FoundationDBBackup against"
type ControlPlaneOpsSpec struct {
	// ControlPlaneRef names the ControlPlane this operation acts on, in this
	// object's own namespace. The operation never owns its target, because
	// deleting the record of an operation must not delete the control plane it
	// operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ControlPlaneRef string `json:"controlPlaneRef"`

	// Action is the operation to perform. Immutable, so that the status describes
	// the operation that ran.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action ControlPlaneOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// the one field of this spec an update may change, because it is the one that
	// is meant to be set after the operation started. Whether an abort is
	// expressible from the current step is declared by that action's graph rather
	// than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Upgrade parameterizes action Upgrade and is ignored by the others.
	// +optional
	Upgrade *UpgradeSpec `json:"upgrade,omitempty"`

	// Restart parameterizes action Restart and is ignored by the others.
	// +optional
	Restart *RestartSpec `json:"restart,omitempty"`

	// Backup parameterizes action Backup and is ignored by the others.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`
}

// ControlPlaneOpsStatus is the observed state of one control-plane operation.
type ControlPlaneOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase ControlPlaneOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs. The rule repeats the
	// ControlPlaneOpsStep enum because a marker cannot reach a field of the
	// shared snapshot type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Draining','Restarting','Awaiting','Preflight','Applying','Verifying','Requesting']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// BackupRef names the FoundationDBBackup a Backup run created or triggered.
	// The operation does not own it, because deleting the record of a backup
	// must not delete the backup's configuration.
	// +optional
	BackupRef string `json:"backupRef,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=cpops
// +kubebuilder:printcolumn:name="ControlPlane",type=string,JSONPath=".spec.controlPlaneRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControlPlaneOps is a single operation performed against the control plane. It
// runs to a terminal phase and stays afterward as the audit record of what was
// done, with which parameters, and how it ended. Only one may be active per
// control plane at a time, which the entity's status.activeOpsRef enforces.
type ControlPlaneOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneOpsSpec   `json:"spec,omitempty"`
	Status ControlPlaneOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneOpsList contains a list of ControlPlaneOps.
type ControlPlaneOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlaneOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlaneOps{}, &ControlPlaneOpsList{})
}
