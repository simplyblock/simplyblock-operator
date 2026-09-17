// PersistentVolumeOps: one imperative operation performed against one
// PersistentVolume, which today means moving its backing logical volume to
// another storage node.
//
// It is the one Ops kind in this group whose target is a core Kubernetes type
// rather than a kind this group defines, and almost everything unusual about it
// follows from that. Its target cannot carry a status.activeOpsRef, so the lock
// moves to an annotation on the volume. It is cluster-scoped because a
// PersistentVolume is, which in turn costs it the owner reference its creator
// would otherwise hold, so the creator is named in the spec instead. And it is
// told no cluster: the volume's CSI handle carries the cluster, pool, and
// volume UUIDs, and that is where all three come from.
//
// The kind is introduced by the redesign, so it has no v1alpha1 spelling, no
// spoke to convert from, and no Hub method: its CRD declares one version.
//
// The registered VolumeMigration stays registered and keeps running beside it
// for at least one release. The two do not convert into one another — a rename
// and a scope change make a new CRD rather than a new version — so in-flight
// migrations are drained on the old kind rather than carried over, and nothing
// creates objects of both kinds for one volume.
//
// design-persistentvolumeops.md is the specification.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/statemachine"
)

// PersistentVolumeOpsLock is the annotation on a PersistentVolume naming the
// operation currently allowed to act on it, and absent when none is.
//
// It is what status.activeOpsRef is for every other kind in this group: taken
// with an optimistic-lock patch so two reconcilers cannot both win, released
// only while it still names the releaser, and released on every terminal path
// including deletion. It sits in metadata rather than status because a
// PersistentVolume is a core type this operator must not add fields to, and it
// touches neither spec nor status, so it cannot conflict with the provisioner
// or the volume's own controllers (design-persistentvolumeops.md §6).
const PersistentVolumeOpsLock = "storage.simplyblock.io/active-ops"

// PersistentVolumeOpsManagedByLabel carries the kind of the controller that
// created an operation, which is what a watch mapping and a List select on: a
// reference in the spec cannot be selected on, and a label value admits no
// namespace separator (design-crd-model.md §7.3).
//
// It narrows rather than identifies. The label finds the operations some drain
// created, and the UID in spec.creatorRef says which drain.
const PersistentVolumeOpsManagedByLabel = "storage.simplyblock.io/managed-by"

// PersistentVolumeOpsAction is the operation a PersistentVolumeOps performs.
// The kind is named for its target rather than for the action so that carrying
// a second one later would not rename it; it carries one, and no second one is
// planned (design-persistentvolumeops.md §4.1).
// +kubebuilder:validation:Enum=Migrate
type PersistentVolumeOpsAction string

const (
	// PersistentVolumeOpsActionMigrate moves the volume's backing logical
	// volume to a different storage node.
	PersistentVolumeOpsActionMigrate PersistentVolumeOpsAction = "Migrate"
)

// PersistentVolumeOpsPhase is the operation's own progress. Succeeded rather
// than the registered kind's Completed, so that every Ops kind in this group
// reports the same five phases and an alert on completion matches one value.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type PersistentVolumeOpsPhase string

const (
	// PersistentVolumeOpsPhasePending is an operation waiting for the volume's
	// lock, or holding nothing yet.
	PersistentVolumeOpsPhasePending PersistentVolumeOpsPhase = "Pending"
	// PersistentVolumeOpsPhaseRunning is an operation holding the lock and
	// working.
	PersistentVolumeOpsPhaseRunning PersistentVolumeOpsPhase = "Running"
	// PersistentVolumeOpsPhaseSucceeded is a finished operation that did what
	// it said.
	PersistentVolumeOpsPhaseSucceeded PersistentVolumeOpsPhase = "Succeeded"
	// PersistentVolumeOpsPhaseFailed is a finished operation that did not.
	PersistentVolumeOpsPhaseFailed PersistentVolumeOpsPhase = "Failed"
	// PersistentVolumeOpsPhaseAborted is an operation stopped on request, or
	// stopped because its volume went away, whose unwind has finished.
	PersistentVolumeOpsPhaseAborted PersistentVolumeOpsPhase = "Aborted"
)

// PersistentVolumeOpsStep is one step of a running volume operation. The
// registered kind merges these with the phases above into one enum, so that
// Validating sits beside Completed and neither can be read without the other's
// values in mind.
// +kubebuilder:validation:Enum=Validating;Migrating;Verifying
type PersistentVolumeOpsStep string

const (
	// PersistentVolumeOpsStepValidating creates the backend migration and
	// starts a Job on every node consuming the subsystem, to check that each of
	// them can reach the target on the paths the migration published.
	PersistentVolumeOpsStepValidating PersistentVolumeOpsStep = "Validating"
	// PersistentVolumeOpsStepMigrating continues the migration, which is what
	// starts the data copy. It is not called Running, because status.phase
	// already has that value and a step sharing a phase's name is the confusion
	// this split exists to end.
	PersistentVolumeOpsStepMigrating PersistentVolumeOpsStep = "Migrating"
	// PersistentVolumeOpsStepVerifying deletes the validation Jobs and confirms
	// no path was left connected. It is a declared step rather than a deferred
	// call because a crash between the copy finishing and the cleanup must
	// restart into it: paths that outlived their Jobs poisoned the data path
	// and blocked every later migration of the volume.
	PersistentVolumeOpsStepVerifying PersistentVolumeOpsStep = "Verifying"
)

// StorageNodeReference locates a StorageNode from a cluster-scoped object.
//
// Every other reference in this API group is a bare string, which works because
// both objects are namespaced and a name means the same namespace. A
// cluster-scoped object has no namespace for a bare name to mean, and two
// clusters in two namespaces may each hold a node called worker-3, so this one
// carries both — for the same reason pv.spec.claimRef does in core Kubernetes.
type StorageNodeReference struct {
	// Namespace is where the StorageNode lives, which is the namespace of the
	// StorageCluster that owns it.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name is the StorageNode object's name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// MigrateVolumeSpec parameterizes the Migrate action.
type MigrateVolumeSpec struct {
	// TargetNodeRef locates the StorageNode to move the volume's backing
	// logical volume to. It names the Kubernetes object rather than the backend
	// UUID, so that a migration can be written by hand without looking one up;
	// the controller resolves the UUID from the node's status. The node's
	// cluster must be the volume's, which the webhook checks rather than the
	// type, because that is a fact about two other objects.
	//
	// The marker sits on the reference rather than on its fields, so the pair
	// is immutable together and a target cannot be half-changed into a name in
	// one cluster and a namespace in another.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetNodeRef StorageNodeReference `json:"targetNodeRef"`
}

// CreatorReference names the object that created a PersistentVolumeOps.
//
// It exists because a cluster-scoped object cannot be owned by a namespaced
// one: Kubernetes treats such a reference as unresolvable and garbage-collects
// the dependent. Core Kubernetes has the same situation twice and answers it
// the same way — a PersistentVolume names its claim through spec.claimRef and a
// VolumeSnapshotContent names its snapshot through spec.volumeSnapshotRef, both
// with a UID — and that shape carries over here unchanged
// (design-persistentvolumeops.md §11.1).
type CreatorReference struct {
	// Kind is the creating object's kind, which is StorageNodeOps for a drain.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace is where the creator lives.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name is the creator's object name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID is what makes the reference address one creator rather than one name:
	// a creator deleted and recreated under the same name must not inherit the
	// fan-out it did not issue, and cascade a delete over it.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`
}

// PersistentVolumeOpsSpec is one operation to perform against one
// PersistentVolume.
//
// The rule keeps the action and its parameter block in agreement, which is a
// statement about this object alone and so belongs on the type rather than in
// the webhook.
// +kubebuilder:validation:XValidation:rule="self.action == 'Migrate' ? has(self.migrate) : !has(self.migrate)",message="migrate is required for action Migrate and must be absent otherwise"
type PersistentVolumeOpsSpec struct {
	// PersistentVolumeName names the PersistentVolume this operation acts on.
	// It is a name rather than a reference because a PersistentVolume is
	// cluster-scoped, and it names the volume rather than the claim because a
	// claim can be deleted while its volume is retained — under a Retain
	// reclaim policy the logical volume still occupies capacity on a node that
	// may be draining, and is still worth moving.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	PersistentVolumeName string `json:"persistentVolumeName"`

	// Action is the operation to perform. Immutable: an operation that changed
	// what it was doing halfway through would have a status describing neither.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action PersistentVolumeOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind. It is
	// expressible from Validating and Migrating and not from Verifying, which
	// the action's graph declares rather than this field: once the copy has
	// finished, the volume has moved and there is nothing to undo.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Migrate parameterizes action Migrate.
	// +optional
	Migrate *MigrateVolumeSpec `json:"migrate,omitempty"`

	// CreatorRef names the object that created this one, and that object's
	// finalizer is what aborts and deletes this one when it goes. Absent on an
	// operation written by hand, which has no creator to cascade from.
	// Immutable, because an operation changing whose fan-out it belongs to
	// would change who cascades over it.
	// +optional
	// +k8s:immutable
	CreatorRef *CreatorReference `json:"creatorRef,omitempty"`
}

// MigrationConnection is one NVMe-oF path the migration published on the
// target.
//
// The connect parameters travel with the address because the path is connected
// on a consuming host rather than here, and what is recorded has to be the
// connect that will actually be made: the host attaches every path with the
// same controller-loss timeout the CSI driver uses, which is not the hour the
// control plane answers with, and a record of the control plane's answer would
// describe a connect nobody performs.
type MigrationConnection struct {
	// NQN is the subsystem the target answers this path for.
	// +optional
	NQN string `json:"nqn,omitempty"`
	// Address and Port are where it answers.
	// +optional
	Address string `json:"address,omitempty"`
	// +optional
	Port *int32 `json:"port,omitempty"`
	// Transport is the fabric, which is TCP.
	// +optional
	Transport string `json:"transport,omitempty"`

	// +optional
	NrIOQueues *int32 `json:"nrIOQueues,omitempty"`
	// +optional
	ReconnectDelaySeconds *int32 `json:"reconnectDelaySeconds,omitempty"`
	// CtrlLossTimeoutSeconds and FastIOFailTimeoutSeconds are pointers because
	// zero is a choice ("fail I/O immediately") rather than a missing value.
	// +optional
	CtrlLossTimeoutSeconds *int32 `json:"ctrlLossTimeoutSeconds,omitempty"`
	// +optional
	FastIOFailTimeoutSeconds *int32 `json:"fastIOFailTimeoutSeconds,omitempty"`
	// +optional
	KeepAliveTimeoutSeconds *int32 `json:"keepAliveTimeoutSeconds,omitempty"`
}

// ValidationJob is one Job started to check a path is reachable from one node.
//
// It is tracked so that Verifying can delete every Job the operation started,
// including after a restart. The namespace is recorded for the same reason
// spec.migrate.targetNodeRef carries one: a Job is namespaced and this object
// is not, so a name alone would not locate it.
type ValidationJob struct {
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Node is the worker the Job is pinned to, which is a node consuming one of
	// the migrated subsystem's volumes.
	// +optional
	Node string `json:"node,omitempty"`

	// Succeeded records a node whose paths were checked and found ready, so a
	// restart does not run the check again on a node that already passed and
	// whose Job its own TTL may already have reaped.
	// +optional
	Succeeded bool `json:"succeeded,omitempty"`
}

// MigrationStatus is everything about the migration rather than about the
// operation. It is durable working state: a controller that restarts mid-copy
// reads it to find the backend migration it started and the Jobs it has to
// clean up (design-crd-model.md §3.1).
type MigrationStatus struct {
	// MigrationUUID is the control plane's identifier for the copy.
	// +optional
	MigrationUUID string `json:"migrationUUID,omitempty"`

	// ClusterUUID, PoolUUID, and VolumeUUID are the three parts of the volume's
	// CSI volume handle, recorded so that later steps address the backend
	// without re-reading the PersistentVolume, and so that a failed operation
	// says which volume it was working on after the volume is gone.
	// +optional
	ClusterUUID string `json:"clusterUUID,omitempty"`
	// +optional
	PoolUUID string `json:"poolUUID,omitempty"`
	// +optional
	VolumeUUID string `json:"volumeUUID,omitempty"`

	// SubsystemNQN is the volume's NVMe-oF subsystem, which is what the
	// migration is addressed by: the control plane migrates a subsystem rather
	// than one volume inside it.
	// +optional
	SubsystemNQN string `json:"subsystemNQN,omitempty"`

	// SourceNodeUUID is where the volume was before the move, recorded so that
	// a failure says what it was and not only what it was going to be.
	// +optional
	SourceNodeUUID string `json:"sourceNodeUUID,omitempty"`

	// TargetNodeUUID is the backend identifier resolved from
	// spec.migrate.targetNodeRef, recorded so the later steps address the
	// target without resolving the node object again.
	// +optional
	TargetNodeUUID string `json:"targetNodeUUID,omitempty"`

	// MemberCount is how many volumes the migrated NVMe-oF subsystem holds, as
	// the control plane reports it. More than one member means the sibling
	// volumes move along with the named one, so the count is both the
	// operation's blast radius and the term the copy's deadline scales by.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MemberCount *int32 `json:"memberCount,omitempty"`

	// Connections are the paths the migration published on the target.
	// Verifying confirms none of them is left connected.
	// +optional
	Connections []MigrationConnection `json:"connections,omitempty"`

	// ValidationJobs are the Jobs started to check those paths.
	// +optional
	ValidationJobs []ValidationJob `json:"validationJobs,omitempty"`
}

// PersistentVolumeOpsStatus is the observed state of one volume operation.
type PersistentVolumeOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase PersistentVolumeOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine, as the shared
	// statemachine.KubeSnapshot. It is persisted before the side effect that
	// step performs. The rule is what an Enum marker would do if a marker could
	// reach a field of a shared type.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','Migrating','Verifying']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Migration is everything about the migration rather than about the
	// operation.
	// +optional
	Migration *MigrationStatus `json:"migration,omitempty"`

	// DeferredSince is when the operation was first held — behind another
	// operation's lock, or behind a control plane that is not accepting
	// migrations yet. It is what the auto-rebalancer reads to decide whether a
	// migration has waited long enough to give up on, and it is in status
	// rather than in memory because the operator may restart and an observer
	// needs to see that the operation is waiting and since when.
	// +optional
	DeferredSince *metav1.Time `json:"deferredSince,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pvops
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".spec.persistentVolumeName"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.migrate.targetNodeRef.name"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// PersistentVolumeOps is a single operation performed against one
// PersistentVolume. It is the one Ops kind in this group whose target is a core
// Kubernetes type rather than a kind this group defines, so it locks its target
// with an annotation rather than a status field, is cluster-scoped because its
// target is, cannot be owned by the namespaced operation that created it, and
// derives its cluster, pool, and volume from the volume's CSI handle rather
// than being told.
type PersistentVolumeOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersistentVolumeOpsSpec   `json:"spec,omitempty"`
	Status PersistentVolumeOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PersistentVolumeOpsList contains a list of PersistentVolumeOps.
type PersistentVolumeOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PersistentVolumeOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PersistentVolumeOps{}, &PersistentVolumeOpsList{})
}
