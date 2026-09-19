// StorageNodeOps in the shape design-storagenode.md Appendix B specifies: a
// single operation performed against one StorageNode, which runs to a terminal
// phase and stays afterward as the audit record of what was done.
//
// What moves against the registered v1alpha1 type is §15.2 of that document. The
// property renames landed first (spec.storageNodeRef becoming spec.nodeRef,
// spec.drain becoming spec.remove, the migrate parameters regrouping under
// spec.migrate, and the action enum becoming PascalCase), and the step machine is
// what arrives here: status.subPhase becomes a status.step holding a declared
// statemachine graph per action, status.triggered goes with nothing replacing it,
// the Aborted phase and the spec.abort that reaches it arrive, the seventh
// HostMaintenance action retires a controller of its own, and the two drain
// counters regroup under status.drain.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// StorageNodeOpsAction is the operation a StorageNodeOps performs. Values are
// PascalCase, which is the casing every enum this API group defines carries;
// v1alpha1 spelled them lowercase, and the conversion maps between the two.
// +kubebuilder:validation:Enum=Shutdown;Restart;Suspend;Resume;Remove;Migrate;HostMaintenance
type StorageNodeOpsAction string

const (
	StorageNodeOpsActionShutdown StorageNodeOpsAction = "Shutdown"
	StorageNodeOpsActionRestart  StorageNodeOpsAction = "Restart"
	StorageNodeOpsActionSuspend  StorageNodeOpsAction = "Suspend"
	StorageNodeOpsActionResume   StorageNodeOpsAction = "Resume"
	StorageNodeOpsActionRemove   StorageNodeOpsAction = "Remove"
	StorageNodeOpsActionMigrate  StorageNodeOpsAction = "Migrate"

	// StorageNodeOpsActionHostMaintenance takes a node down deliberately so that
	// its Kubernetes worker can be drained and rebooted, then brings it back. The
	// operator raises it when it sees the worker cordoned, and a user creating
	// one by hand behaves identically.
	StorageNodeOpsActionHostMaintenance StorageNodeOpsAction = "HostMaintenance"
)

// StorageNodeOpsPhase is the operation's own progress. Aborted is terminal and
// distinct from Failed, because a canceled operation did not go wrong.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageNodeOpsPhase string

const (
	// StorageNodeOpsPhasePending: the operation holds no lock and has issued
	// nothing.
	StorageNodeOpsPhasePending StorageNodeOpsPhase = "Pending"

	// StorageNodeOpsPhaseRunning: it holds its node's lock and its first side
	// effect may have been issued.
	StorageNodeOpsPhaseRunning StorageNodeOpsPhase = "Running"

	StorageNodeOpsPhaseSucceeded StorageNodeOpsPhase = "Succeeded"
	StorageNodeOpsPhaseFailed    StorageNodeOpsPhase = "Failed"

	// StorageNodeOpsPhaseAborted: called off rather than gone wrong. A drain an
	// administrator stops after an hour reported as Failed would sit in the same
	// bucket as one the control plane rejected.
	StorageNodeOpsPhaseAborted StorageNodeOpsPhase = "Aborted"
)

// StorageNodeOpsStep is one step of a running node operation. The enum is the
// union of every action's steps; which steps belong to which action is declared by
// the graph rather than by this type.
// +kubebuilder:validation:Enum=Requesting;Awaiting;Validating;Suspending;MigratingVolumes;Verifying;Removing;Preparing;Relocating;AwaitingNode;Promoting;Holding;ShuttingDown;Releasing;AwaitingHost;Restarting;Cleanup
type StorageNodeOpsStep string

const (
	// Shutdown, Restart, Suspend, and Resume.
	StorageNodeOpsStepRequesting StorageNodeOpsStep = "Requesting"
	StorageNodeOpsStepAwaiting   StorageNodeOpsStep = "Awaiting"

	// Remove.
	StorageNodeOpsStepValidating       StorageNodeOpsStep = "Validating"
	StorageNodeOpsStepSuspending       StorageNodeOpsStep = "Suspending"
	StorageNodeOpsStepMigratingVolumes StorageNodeOpsStep = "MigratingVolumes"
	StorageNodeOpsStepVerifying        StorageNodeOpsStep = "Verifying"
	StorageNodeOpsStepRemoving         StorageNodeOpsStep = "Removing"

	// Migrate.
	StorageNodeOpsStepPreparing    StorageNodeOpsStep = "Preparing"
	StorageNodeOpsStepRelocating   StorageNodeOpsStep = "Relocating"
	StorageNodeOpsStepAwaitingNode StorageNodeOpsStep = "AwaitingNode"
	StorageNodeOpsStepPromoting    StorageNodeOpsStep = "Promoting"

	// HostMaintenance.
	StorageNodeOpsStepHolding      StorageNodeOpsStep = "Holding"
	StorageNodeOpsStepShuttingDown StorageNodeOpsStep = "ShuttingDown"
	StorageNodeOpsStepReleasing    StorageNodeOpsStep = "Releasing"
	StorageNodeOpsStepAwaitingHost StorageNodeOpsStep = "AwaitingHost"
	StorageNodeOpsStepRestarting   StorageNodeOpsStep = "Restarting"
	StorageNodeOpsStepCleanup      StorageNodeOpsStep = "Cleanup"
)

// MigrateSpec parameterizes the Migrate action and is ignored by the others.
//
// A migration is not a removal followed by an add: the node keeps its backend
// UUID, its partitions, and its logical-volume assignments, and what changes is
// the machine the SPDK process runs on. No PersistentVolumeOps is created.
type MigrateSpec struct {
	// TargetWorkerNode is the Kubernetes worker the node is relocated onto.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetWorkerNode string `json:"targetWorkerNode"`

	// NewSsdPcie lists additional NVMe PCI addresses to bind on the target host,
	// passed through to the control-plane restart as new_ssd_pcie and merged into
	// the node's effective allow list so they survive a later rebuild.
	// +optional
	NewSsdPcie []string `json:"newSsdPcie,omitempty"`
}

// RemoveSpec parameterizes the Remove action and is ignored by the others.
type RemoveSpec struct {
	// SystemVolumeFilterRegex matches backend volume names that are system
	// volumes: excluded from the drain's migration and deleted during
	// verification rather than blocking it.
	// +kubebuilder:default=`^sb-fio-baseline-.*`
	// +optional
	SystemVolumeFilterRegex *string `json:"systemVolumeFilterRegex,omitempty"`
}

// StorageNodeOpsSpec is one operation to perform against one StorageNode.
type StorageNodeOpsSpec struct {
	// NodeRef names the StorageNode this operation acts on. The operation never
	// owns its target, because deleting the record of an operation must not delete
	// the node it operated on.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	NodeRef string `json:"nodeRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageNodeOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// the only mutable field on this spec, because it is the only thing about an
	// operation that can legitimately be decided after it started. Whether an
	// abort is expressible from the current step is declared by that action's
	// graph rather than checked here.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Force passes the control plane's force flag where the action supports it.
	// Migrate defaults it to true, because the control plane rejects a non-forced
	// restart of a node that is not already offline.
	// +optional
	Force *bool `json:"force,omitempty"`

	// ReattachVolume asks the control plane to reattach this node's volumes as
	// part of a restart. Applies to Restart, Migrate, and HostMaintenance.
	// +optional
	ReattachVolume *bool `json:"reattachVolume,omitempty"`

	// Migrate parameterizes action Migrate and is ignored by the others.
	// +optional
	Migrate *MigrateSpec `json:"migrate,omitempty"`

	// Remove parameterizes action Remove and is ignored by the others.
	// +optional
	Remove *RemoveSpec `json:"remove,omitempty"`
}

// MigrateParams returns the Migrate block, or a zero-valued one when the operation
// did not set it.
//
// The block is optional on every action, so four of the migrate workflow's steps
// would otherwise each need their own nil check. A Migrate operation that arrives
// without one has an empty target worker, which the workflow already rejects with
// a message naming the field — a better outcome than a nil dereference, and the
// same one the flat v1alpha1 fields produced.
func (s *StorageNodeOpsSpec) MigrateParams() MigrateSpec {
	if s.Migrate == nil {
		return MigrateSpec{}
	}
	return *s.Migrate
}

// RemoveParams returns the Remove block, or a zero-valued one when the operation
// did not set it, for the reason MigrateParams does.
func (s *StorageNodeOpsSpec) RemoveParams() RemoveSpec {
	if s.Remove == nil {
		return RemoveSpec{}
	}
	return *s.Remove
}

// DrainStatus is the drain's progress over the volumes on the node being removed.
// Neither field takes omitempty: zero is meaningful for both, and a field that
// disappears at zero makes "nothing to move" and "not yet counted" the same wire
// value.
type DrainStatus struct {
	// VolumesTotal is the number of PV-managed volumes the drain has to move,
	// written once at the end of Validating and not modified afterward.
	// +kubebuilder:validation:Minimum=0
	VolumesTotal int32 `json:"volumesTotal"`

	// VolumesMigrated is how many of them have completed.
	// +kubebuilder:validation:Minimum=0
	VolumesMigrated int32 `json:"volumesMigrated"`
}

// StorageNodeOpsStatus is the observed state of one node operation.
type StorageNodeOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageNodeOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine, as the shared
	// statemachine.KubeSnapshot. The rule is what an Enum marker would do if a
	// marker could reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Requesting','Awaiting','Validating','Suspending','MigratingVolumes','Verifying','Removing','Preparing','Relocating','AwaitingNode','Promoting','Holding','ShuttingDown','Releasing','AwaitingHost','Restarting','Cleanup']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as the
	// operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// Drain is the drain's progress over the node's volumes, set only for action
	// Remove.
	// +optional
	Drain *DrainStatus `json:"drain,omitempty"`

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
// +kubebuilder:resource:scope=Namespaced,shortName=snops
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNodeOps is a single operation performed against one StorageNode. It runs
// to a terminal phase and stays afterward as the audit record of what was done, to
// which node, with which parameters, and how it ended.
type StorageNodeOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageNodeOpsSpec   `json:"spec,omitempty"`
	Status StorageNodeOpsStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for StorageNodeOps.
func (*StorageNodeOps) Hub() {}

// +kubebuilder:object:root=true

// StorageNodeOpsList contains a list of StorageNodeOps.
type StorageNodeOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageNodeOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageNodeOps{}, &StorageNodeOpsList{})
}
