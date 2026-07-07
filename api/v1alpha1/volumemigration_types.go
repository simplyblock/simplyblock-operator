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

// VolumeMigrationSpec defines the desired state of a VolumeMigration.
type VolumeMigrationSpec struct {
	// PVName is the name of the PersistentVolume whose backing logical volume
	// should be migrated. The PV must be provisioned by the simplyblock CSI driver.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pvName is immutable once set"
	PVName string `json:"pvName"`

	// TargetNodeUUID is the UUID of the storage node that should host the
	// volume after migration.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetNodeUUID is immutable once set"
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

	// SourceNodeUUID is the storage node UUID where the volume resided before
	// migration, as reported by the storage API.
	SourceNodeUUID string `json:"sourceNodeUUID,omitempty"`

	// SnapsTotal is the total number of snapshots to migrate, as reported by the API.
	SnapsTotal int `json:"snapsTotal,omitempty"`

	// SnapsMigrated is the number of snapshots migrated so far.
	SnapsMigrated int `json:"snapsMigrated,omitempty"`

	// ErrorMessage holds the failure reason when Phase is Failed.
	ErrorMessage string `json:"errorMessage,omitempty"`

	// Connections holds the NVMe-oF connection parameters for the new target-side
	// paths returned by CreateMigration. Used during the Validating phase to
	// establish and verify the paths before calling ContinueMigration.
	Connections []MigrationConnection `json:"connections,omitempty"`

	// ValidationJobName is the name of the Job that runs `nvme connect` for each
	// connection path and validates ANA state before ContinueMigration is called.
	// Set during the Validating phase; cleared when the phase advances to Running.
	ValidationJobName string `json:"validationJobName,omitempty"`

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
// +kubebuilder:printcolumn:name="Snaps",type="integer",JSONPath=".status.snapsMigrated",priority=1
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
