package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeMigrationPhase describes the lifecycle state of a VolumeMigration.
type VolumeMigrationPhase string

const (
	// VolumeMigrationPhasePending means the migration has been accepted but not
	// yet submitted to the storage API.
	VolumeMigrationPhasePending VolumeMigrationPhase = "Pending"
	// VolumeMigrationPhaseValidating means CreateMigration has been called and
	// the operator is validating the new NVMe-oF connection paths on the target
	// node before calling ContinueMigration.
	VolumeMigrationPhaseValidating VolumeMigrationPhase = "Validating"
	// VolumeMigrationPhaseRunning means ContinueMigration has been called and
	// the data migration is in progress.
	VolumeMigrationPhaseRunning VolumeMigrationPhase = "Running"
	// VolumeMigrationPhaseCompleted means the migration finished successfully.
	VolumeMigrationPhaseCompleted VolumeMigrationPhase = "Completed"
	// VolumeMigrationPhaseFailed means the migration finished with an error.
	VolumeMigrationPhaseFailed VolumeMigrationPhase = "Failed"
	// VolumeMigrationPhaseAborted means the migration was cancelled via spec.abort.
	VolumeMigrationPhaseAborted VolumeMigrationPhase = "Aborted"
)

// MigrationConnection holds the NVMe-oF connection parameters for one path
// on the migration target node, as returned by the storage API's CreateMigration.
// All fields are passed verbatim to `nvme connect` in the validation Job.
type MigrationConnection struct {
	NQN            string `json:"nqn"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	Transport      string `json:"transport"`
	NrIoQueues     int    `json:"nrIoQueues,omitempty"`
	ReconnectDelay int    `json:"reconnectDelay,omitempty"`
	CtrlLossTmo    int    `json:"ctrlLossTmo,omitempty"`
	FastIOFailTmo  int    `json:"fastIOFailTmo,omitempty"`
	KeepAliveTmo   int    `json:"keepAliveTmo,omitempty"`
}

// ValidationJob is one NVMe path-validation Job and the worker node it runs on.
// The node is a consumer of some volume in the migrated subsystem — the volume named
// in the spec, or one of its siblings sharing the same NVMe subsystem.
type ValidationJob struct {
	// Node is the Kubernetes node name the Job is pinned to.
	Node string `json:"node"`

	// JobName is the name of the Job object in the VolumeMigration's namespace.
	JobName string `json:"jobName"`

	// Succeeded records that this node's validation passed. It is kept because the
	// Job's own existence is not a reliable record: Jobs are reaped, and re-reading a
	// reaped Job would otherwise look like "never validated" and start it again.
	// +optional
	Succeeded bool `json:"succeeded,omitempty"`
}

// VolumeMigrationSpec defines the desired state of a VolumeMigration.
type VolumeMigrationSpec struct {
	// PVName is the name of the PersistentVolume whose backing logical volume
	// should be migrated. The PV must be provisioned by the simplyblock CSI driver.
	// +kubebuilder:validation:MinLength=1
	// +k8s:immutable
	PVName string `json:"pvName"`

	// TargetNodeUUID is the UUID of the storage node that should host the
	// volume after migration.
	// +kubebuilder:validation:MinLength=1
	// +k8s:immutable
	TargetNodeUUID string `json:"targetNodeUUID"`

	// Abort requests cancellation of an in-progress migration. Set to true to
	// cancel; the phase will transition to Aborted once the backend confirms.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// VolumeMigrationStatus defines the observed state of a VolumeMigration.
type VolumeMigrationStatus struct {
	// Phase is the current lifecycle phase of the migration.
	// +kubebuilder:validation:Enum=Pending;Validating;Running;Completed;Failed;Aborted
	Phase VolumeMigrationPhase `json:"phase,omitempty"`

	// MigrationUUID is the identifier returned by the storage API when the
	// migration was submitted. Used for polling and cancellation.
	MigrationUUID string `json:"migrationUUID,omitempty"`

	// ClusterUUID is the storage cluster UUID resolved from the PV.
	ClusterUUID string `json:"clusterUUID,omitempty"`

	// VolumeUUID is the logical volume UUID resolved from the PV's CSI volume handle.
	VolumeUUID string `json:"volumeUUID,omitempty"`

	// PoolUUID is the storage pool UUID that contains the volume.
	PoolUUID string `json:"poolUUID,omitempty"`

	// SubsystemNQN is the NQN of the volume's NVMe subsystem, resolved from the
	// storage API when the migration is submitted. The migration is addressed by
	// it, and every volume sharing the subsystem moves with it.
	SubsystemNQN string `json:"subsystemNQN,omitempty"`

	// SourceNodeUUID is the storage node UUID where the volume resided before
	// migration, as reported by the storage API.
	SourceNodeUUID string `json:"sourceNodeUUID,omitempty"`

	// MemberCount is the number of volumes (namespaces) in the migrated
	// subsystem, as reported by the storage API. More than one means the
	// migration moves sibling volumes along with this one.
	MemberCount int `json:"memberCount,omitempty"`

	// ErrorMessage holds the failure reason when Phase is Failed.
	ErrorMessage string `json:"errorMessage,omitempty"`

	// Connections holds the NVMe-oF connection parameters for the new target-side
	// paths returned by CreateMigration. Used during the Validating phase to
	// establish and verify the paths before calling ContinueMigration, and again to
	// release them if the migration never cuts over.
	//
	// These are the parameters the paths are actually connected with, not verbatim
	// what CreateMigration answered: ctrlLossTmo is replaced with the value every
	// path in this system uses, because a target path becomes the volume's data path
	// at cutover. The rest is passed through.
	Connections []MigrationConnection `json:"connections,omitempty"`

	// ValidationJobs are the Jobs that run `nvme connect` for each connection path
	// and validate ANA state before ContinueMigration is called — one per worker
	// node that consumes a volume of the migrated subsystem. A subsystem migrates
	// as a unit, so every consuming node must have the new paths before cutover;
	// all of these Jobs must succeed. Set during the Validating phase; cleared when
	// the phase advances to Running.
	ValidationJobs []ValidationJob `json:"validationJobs,omitempty"`

	// DeferredSince is when the storage API first refused to accept this migration
	// because the cluster was busy with work that ends on its own (a data realignment
	// or another node migration). While set, the migration is being retried and has
	// not started. It bounds the retrying: past a fixed window the migration fails
	// rather than waiting forever. Cleared once the migration is submitted.
	DeferredSince *metav1.Time `json:"deferredSince,omitempty"`

	// StartedAt is the time the migration was submitted to the storage API.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is the time the migration finished (successfully or not).
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vmig
// +kubebuilder:printcolumn:name="PV",type="string",JSONPath=".spec.pvName"
// +kubebuilder:printcolumn:name="Target Node",type="string",JSONPath=".spec.targetNodeUUID"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Volumes",type="integer",JSONPath=".status.memberCount",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VolumeMigration triggers a storage-node migration for a single PersistentVolume.
// Create a VolumeMigration to move a volume's backing logical volume to a different
// storage node. The controller resolves the PV to a logical volume UUID, submits the
// migration via the storage API, and tracks progress until completion or failure.
// Set spec.abort=true to cancel an in-progress migration.
type VolumeMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeMigrationSpec   `json:"spec,omitempty"`
	Status VolumeMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeMigrationList contains a list of VolumeMigration.
type VolumeMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeMigration{}, &VolumeMigrationList{})
}
