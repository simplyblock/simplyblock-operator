// ControlPlaneOps: one imperative operation performed against the control plane.
//
// Most of what an administrator does to a control plane is expressible as
// desired state, because the entity re-applies what it installed on every pass:
// changing the image is an edit, scaling FoundationDB is an edit, and an object
// somebody deleted by hand is put back. What is left is what this kind carries,
// and it is four things: restarting a workload, moving it to a new version,
// asking FoundationDB for a backup, and taking a FoundationDB host through a
// Kubernetes node drain.
//
// Every action requires a control plane this cluster hosts, since each acts on
// something the operator installed. An operation naming a remote one is rejected
// at admission rather than created and failed, which is what
// ControlPlaneOpsValidator is for.
//
// The kind is introduced by the redesign, so it has no v1alpha1 spelling, no
// spoke to convert from, and no Hub method: its CRD declares one version.
//
// design-controlplane.md §6 and Appendix B, and design-fdb-host-maintenance.md
// §4 and Appendix A, are the specification.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/crdtypes"
	"github.com/simplyblock/atlas/statemachine"
)

// ControlPlaneOpsAction is the operation a ControlPlaneOps performs. Every
// action acts on what the operator installed, so every action requires a managed
// control plane. An operation naming a control plane the operator did not
// install is rejected at creation rather than created and failed.
// +kubebuilder:validation:Enum=Restart;Upgrade;Backup;HostMaintenance
type ControlPlaneOpsAction string

const (
	// ControlPlaneOpsActionRestart restarts a wedged workload. Its scope is
	// spec.restart.components, and an empty list restarts the whole control
	// plane.
	ControlPlaneOpsActionRestart ControlPlaneOpsAction = "Restart"
	// ControlPlaneOpsActionUpgrade moves the control plane to a new version and
	// verifies afterward that the version it reports is the one asked for.
	ControlPlaneOpsActionUpgrade ControlPlaneOpsAction = "Upgrade"
	// ControlPlaneOpsActionBackup asks FoundationDB for a backup outside
	// whatever schedule exists. It asks rather than implements: the snapshot is
	// the FoundationDB operator's to take.
	ControlPlaneOpsActionBackup ControlPlaneOpsAction = "Backup"
	// ControlPlaneOpsActionHostMaintenance takes a Kubernetes worker that runs
	// FoundationDB processes through a node drain: it holds the eviction of
	// those pods from the cordon on, and when the drain reaches the worker it
	// sets FoundationDB's maintenance mode for the worker's zone, moves the
	// coordinators the worker holds onto other hosts, and releases the
	// eviction. It clears the maintenance mode once the worker's processes are
	// back. The operator raises it when such a worker is cordoned.
	ControlPlaneOpsActionHostMaintenance ControlPlaneOpsAction = "HostMaintenance"
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
// +kubebuilder:validation:Enum=Draining;Restarting;Awaiting;Preflight;Applying;Verifying;Requesting;Holding;EnteringMaintenance;MovingCoordinators;ExcludingProcesses;Releasing;AwaitingHost;IncludingProcesses;LeavingMaintenance;Cleanup;Completed
type ControlPlaneOpsStep string

const (
	// ControlPlaneOpsStepDraining holds while another operation in the namespace
	// is still running. Restart and Upgrade both restart the management API, so
	// both drain.
	ControlPlaneOpsStepDraining ControlPlaneOpsStep = "Draining"

	// ControlPlaneOpsStepRestarting rolls the workloads the action named.
	ControlPlaneOpsStepRestarting ControlPlaneOpsStep = "Restarting"
	// ControlPlaneOpsStepAwaiting waits for what the previous step asked for:
	// the restarted pods coming back, or the backup reporting a snapshot.
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

	// ControlPlaneOpsStepHolding labels the worker's FoundationDB pods, puts up
	// the budget that holds their eviction, and waits for a drain of the worker
	// to be observed. It is the one step of a HostMaintenance window from which
	// an abort is possible, because nothing has been done to the database yet.
	ControlPlaneOpsStepHolding ControlPlaneOpsStep = "Holding"
	// ControlPlaneOpsStepEnteringMaintenance sets FoundationDB's maintenance
	// mode for the worker's zone, so that data distribution leaves the host's
	// storage data alone while the host is away.
	ControlPlaneOpsStepEnteringMaintenance ControlPlaneOpsStep = "EnteringMaintenance"
	// ControlPlaneOpsStepMovingCoordinators writes a coordinator set that names
	// no process on the worker, and completes once the cluster file agrees.
	ControlPlaneOpsStepMovingCoordinators ControlPlaneOpsStep = "MovingCoordinators"
	// ControlPlaneOpsStepExcludingProcesses excludes the worker's log and
	// stateless processes, so that their roles move while they are alive and
	// the eviction kills processes that hold nothing.
	ControlPlaneOpsStepExcludingProcesses ControlPlaneOpsStep = "ExcludingProcesses"
	// ControlPlaneOpsStepReleasing takes the budget down, which is what lets
	// the drain proceed.
	ControlPlaneOpsStepReleasing ControlPlaneOpsStep = "Releasing"
	// ControlPlaneOpsStepAwaitingHost waits for the worker's FoundationDB
	// process groups to report back, on the same node or a replacement.
	ControlPlaneOpsStepAwaitingHost ControlPlaneOpsStep = "AwaitingHost"
	// ControlPlaneOpsStepIncludingProcesses lifts the exclusion the window
	// wrote, once the processes are back to take roles again.
	ControlPlaneOpsStepIncludingProcesses ControlPlaneOpsStep = "IncludingProcesses"
	// ControlPlaneOpsStepLeavingMaintenance clears the maintenance mode the
	// window set, when the active zone is still the window's own.
	ControlPlaneOpsStepLeavingMaintenance ControlPlaneOpsStep = "LeavingMaintenance"
	// ControlPlaneOpsStepCleanup removes whatever marker the release left
	// behind. It is the last step of a window that succeeded.
	ControlPlaneOpsStepCleanup ControlPlaneOpsStep = "Cleanup"

	// ControlPlaneOpsStepCompleted is the terminal step of an operation that succeeded.
	ControlPlaneOpsStepCompleted ControlPlaneOpsStep = "Completed"
)

// HostMaintenanceCoordinatorPlacement is how a HostMaintenance window placed
// the coordinators it moved.
// +kubebuilder:validation:Enum=Spread;SharedHost;Unchanged
type HostMaintenanceCoordinatorPlacement string

const (
	// HostMaintenanceCoordinatorPlacementSpread is a set with one coordinator
	// per host, which the FoundationDB operator keeps as it is.
	HostMaintenanceCoordinatorPlacementSpread HostMaintenanceCoordinatorPlacement = "Spread"
	// HostMaintenanceCoordinatorPlacementSharedHost is a set in which two
	// coordinators share a host, written when fewer hosts than coordinators
	// remain off the leaving workers. The FoundationDB operator re-elects a
	// spread set once the worker's processes are back.
	HostMaintenanceCoordinatorPlacementSharedHost HostMaintenanceCoordinatorPlacement = "SharedHost"
	// HostMaintenanceCoordinatorPlacementUnchanged is a window that wrote no
	// set: the worker held no coordinator, too few processes exist off the
	// leaving workers, or the command failed. The operation's events say which.
	HostMaintenanceCoordinatorPlacementUnchanged HostMaintenanceCoordinatorPlacement = "Unchanged"
)

// HostMaintenanceDrainSignal is how a HostMaintenance window observed the
// drain of its worker.
// +kubebuilder:validation:Enum=Sentinel;PodDisruption
type HostMaintenanceDrainSignal string

const (
	// HostMaintenanceDrainSignalSentinel is the eviction of the sentinel pod the
	// window placed on the worker beside the held FoundationDB pods.
	HostMaintenanceDrainSignalSentinel HostMaintenanceDrainSignal = "Sentinel"
	// HostMaintenanceDrainSignalPodDisruption is the eviction of another pod on
	// the worker, seen as that pod's DisruptionTarget condition.
	HostMaintenanceDrainSignalPodDisruption HostMaintenanceDrainSignal = "PodDisruption"
)

// UpgradeSpec parameterizes the Upgrade action and is ignored by the others.
type UpgradeSpec struct {
	// Image is the version to move to. It replaces
	// ControlPlane.spec.source.managed.image when the operation succeeds, so the
	// entity keeps describing what is running.
	// +kubebuilder:validation:Required
	Image crdtypes.ImageReference `json:"image"`
}

// RestartSpec parameterizes the Restart action and is ignored by the others.
type RestartSpec struct {
	// Components names the workloads to restart. Empty restarts the whole
	// control plane. Naming only components that status.components reports as
	// non-essential skips the drain, because restarting them interrupts nothing.
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

// HostMaintenanceSpec parameterizes the HostMaintenance action and is ignored
// by the others.
type HostMaintenanceSpec struct {
	// WorkerNode is the Kubernetes Node whose FoundationDB processes the window
	// takes roles off. It is the Node's name, and the zone the window maintains
	// is the Node's kubernetes.io/hostname label.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	WorkerNode string `json:"workerNode"`
}

// ControlPlaneOpsSpec is one operation to perform against the control plane.
//
// Everything except spec.abort is frozen once the object is admitted, which is
// what makes the status an audit of the request that ran rather than of whatever
// the object says now. The parameters are consumed several steps apart:
// Preflight reads spec.upgrade.image and Applying writes it, Draining reads
// spec.restart.components before Restarting rolls them, and Holding labels the
// pods of spec.hostMaintenance.workerNode that Releasing unlabels. An edit in
// between produces an operation that checked one thing and did another.
//
// What is frozen is each parameter block's presence together with its
// contents.
// +kubebuilder:validation:XValidation:rule="has(self.upgrade) == has(oldSelf.upgrade) && (!has(self.upgrade) || self.upgrade == oldSelf.upgrade)",message="spec.upgrade is immutable: Preflight checked the image the operation was admitted with, and Applying writes it several steps later"
// +kubebuilder:validation:XValidation:rule="has(self.restart) == has(oldSelf.restart) && (!has(self.restart) || self.restart == oldSelf.restart)",message="spec.restart is immutable: the drain is decided from the component list, so widening it afterward skips a drain the wider list would have required"
// +kubebuilder:validation:XValidation:rule="has(self.backup) == has(oldSelf.backup) && (!has(self.backup) || self.backup == oldSelf.backup)",message="spec.backup is immutable: the destination is what Requesting created the FoundationDBBackup against"
// +kubebuilder:validation:XValidation:rule="has(self.hostMaintenance) == has(oldSelf.hostMaintenance) && (!has(self.hostMaintenance) || self.hostMaintenance == oldSelf.hostMaintenance)",message="spec.hostMaintenance is immutable: the budget and the maintenance zone were raised for the worker the operation was admitted with"
// +kubebuilder:validation:XValidation:rule="self.action != 'HostMaintenance' || has(self.hostMaintenance)",message="action HostMaintenance needs spec.hostMaintenance, which names the worker"
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

	// HostMaintenance parameterizes action HostMaintenance and is ignored by
	// the others.
	// +optional
	HostMaintenance *HostMaintenanceSpec `json:"hostMaintenance,omitempty"`
}

// HostMaintenanceStatus is what a HostMaintenance window did, written as it
// goes so that a later step acts on what an earlier one recorded.
type HostMaintenanceStatus struct {
	// Zone is the FoundationDB fault domain the window maintains, read from
	// the worker's kubernetes.io/hostname label when the window started.
	// +optional
	Zone string `json:"zone,omitempty"`

	// ProcessGroups are the FoundationDB process groups whose fault domain was
	// the zone when the window started. They are what AwaitingHost waits for,
	// wherever they come back.
	// +listType=set
	// +optional
	ProcessGroups []string `json:"processGroups,omitempty"`

	// DrainObservedAt is when a drain of the worker was first seen. Empty while
	// the window holds without one.
	// +optional
	DrainObservedAt *metav1.Time `json:"drainObservedAt,omitempty"`

	// DrainObservedBy is which signal saw the drain: the eviction of the
	// window's sentinel pod, or the eviction of another pod on the worker.
	// +optional
	DrainObservedBy HostMaintenanceDrainSignal `json:"drainObservedBy,omitempty"`

	// Coordinators is the set the window wrote, as the addresses the cluster
	// file carries. Empty when the window wrote none.
	// +listType=set
	// +optional
	Coordinators []string `json:"coordinators,omitempty"`

	// CoordinatorPlacement is how the set was placed: one per host, two on
	// one host, or not written at all.
	// +optional
	CoordinatorPlacement HostMaintenanceCoordinatorPlacement `json:"coordinatorPlacement,omitempty"`

	// ExcludedProcesses are the process groups the window excluded before the
	// release, by ID, which IncludingProcesses and the terminal teardown
	// include again. Empty when the host had only storage processes.
	// +listType=set
	// +optional
	ExcludedProcesses []string `json:"excludedProcesses,omitempty"`

	// MaintenanceUntil is when the maintenance mode the window set expires on
	// its own. Empty when the mode could not be set.
	// +optional
	MaintenanceUntil *metav1.Time `json:"maintenanceUntil,omitempty"`
}

// ControlPlaneOpsStatus is the observed state of one control-plane operation.
type ControlPlaneOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase ControlPlaneOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs, so a step reported
	// here is a step that started.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Draining','Restarting','Awaiting','Preflight','Applying','Verifying','Requesting','Holding','EnteringMaintenance','MovingCoordinators','ExcludingProcesses','Releasing','AwaitingHost','IncludingProcesses','LeavingMaintenance','Cleanup','Completed']",message="unknown step"
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

	// HostMaintenance is what a HostMaintenance window did, written as it goes.
	// +optional
	HostMaintenance *HostMaintenanceStatus `json:"hostMaintenance,omitempty"`

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
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".spec.hostMaintenance.workerNode",priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControlPlaneOps is a single operation performed against the control plane. It
// runs to a terminal phase and stays afterward as the audit record of what was
// done, with which parameters, and how it ended. Only one may act on the
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
