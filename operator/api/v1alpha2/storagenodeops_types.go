// StorageNodeOps with the four properties the CRD redesign renames or regroups
// on it (design-storagenode.md §6.1 and §6.3):
//
//   - spec.storageNodeRef becomes spec.nodeRef. The kind is already named for the
//     node, so the prefix repeated it.
//   - spec.drain becomes spec.remove, matching the action it parameterizes, and
//     DrainOpsSpec becomes RemoveSpec with it.
//   - spec.targetWorkerNode and spec.newSsdPcie regroup under spec.migrate, so
//     that a parameter block belongs to the action that reads it.
//   - spec.action becomes a named enum whose values are PascalCase.
//
// The step machine is not here. status.subPhase becoming a status.step object,
// the Aborted phase, spec.abort, the HostMaintenance action, and the drain status
// regrouping are all the Ops shape of design-crd-model.md §9.5 rather than
// renames (design-property-renames.md §2.7).

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageNodeOpsPhase is the lifecycle phase of a StorageNodeOps.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StorageNodeOpsPhase string

const (
	StorageNodeOpsPhasePending   StorageNodeOpsPhase = "Pending"
	StorageNodeOpsPhaseRunning   StorageNodeOpsPhase = "Running"
	StorageNodeOpsPhaseSucceeded StorageNodeOpsPhase = "Succeeded"
	StorageNodeOpsPhaseFailed    StorageNodeOpsPhase = "Failed"
)

// StorageNodeOpsAction is the operation a StorageNodeOps performs. The values are
// PascalCase, as every enum in the group is; v1alpha1 spelled them lowercase, and
// the conversion maps between the two.
// +kubebuilder:validation:Enum=Shutdown;Restart;Suspend;Resume;Remove;Migrate
type StorageNodeOpsAction string

const (
	StorageNodeOpsActionShutdown StorageNodeOpsAction = "Shutdown"
	StorageNodeOpsActionRestart  StorageNodeOpsAction = "Restart"
	StorageNodeOpsActionSuspend  StorageNodeOpsAction = "Suspend"
	StorageNodeOpsActionResume   StorageNodeOpsAction = "Resume"
	StorageNodeOpsActionRemove   StorageNodeOpsAction = "Remove"
	StorageNodeOpsActionMigrate  StorageNodeOpsAction = "Migrate"
)

// StorageNodeOpsSubPhase is the active sub-phase during a running op: the drain
// steps when action=Remove, and the Preparing → Migrating → Promoting steps when
// action=Migrate.
// +kubebuilder:validation:Enum=Validating;Suspending;Migrating;Verifying;Removing;Preparing;Restarting;Promoting
type StorageNodeOpsSubPhase string

const (
	StorageNodeOpsSubPhaseValidating StorageNodeOpsSubPhase = "Validating"
	StorageNodeOpsSubPhaseSuspending StorageNodeOpsSubPhase = "Suspending"
	StorageNodeOpsSubPhaseMigrating  StorageNodeOpsSubPhase = "Migrating"
	StorageNodeOpsSubPhaseVerifying  StorageNodeOpsSubPhase = "Verifying"
	StorageNodeOpsSubPhaseRemoving   StorageNodeOpsSubPhase = "Removing"
	// StorageNodeOpsSubPhasePreparing marks that a migrate op is preparing the
	// target worker: cloning per-node config, labeling it into the storage
	// plane, and waiting until its storage-node-api pod is Ready and its per-pod
	// DNS name is published in the EndpointSlice — the precondition for the
	// control-plane restart to resolve node_address.
	StorageNodeOpsSubPhasePreparing StorageNodeOpsSubPhase = "Preparing"
	// StorageNodeOpsSubPhaseRestarting marks that a migrate op has issued the
	// control-plane restart and confirmed the node entered in_restart; it is
	// now waiting for the node to come back online on the target host. The
	// restart is asynchronous, so the op only advances to Promoting after the
	// node has left online (restart started) and returned to online (restart
	// finished) — issuing /promote earlier races the in-flight restart's node
	// writes and leaves the relocated devices stuck in `new`.
	StorageNodeOpsSubPhaseRestarting StorageNodeOpsSubPhase = "Restarting"
	// StorageNodeOpsSubPhasePromoting marks that a migrate op has issued the
	// control-plane /promote for the relocated node (guards against re-promoting).
	StorageNodeOpsSubPhasePromoting StorageNodeOpsSubPhase = "Promoting"
)

// MigrateSpec parameterizes the Migrate action and is ignored by the others.
//
// A migration is NOT a drain or a remove: the storage node keeps its backend UUID
// and its partition and logical-volume assignments follow it. The operator issues
// a control-plane restart pointed at the target host's storage-node-api
// (node_address), waits for the node to come back online there, then promotes it
// (starting a rebalance) and re-points the StorageNode's spec.workerNode and the
// owning StorageNodeSet.workerNodes from the source worker to this one. No fresh
// storage node is provisioned and no VolumeMigration CRs are created.
type MigrateSpec struct {
	// TargetWorkerNode is the Kubernetes worker hostname the storage node is
	// relocated onto.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	TargetWorkerNode string `json:"targetWorkerNode"`

	// NewSsdPcie lists additional NVMe PCIe addresses to bind on the target host
	// during a migration. Passed through to the control-plane restart as
	// new_ssd_pcie.
	// +optional
	NewSsdPcie []string `json:"newSsdPcie,omitempty"`
}

// RemoveSpec parameterizes the Remove action and is ignored by the others.
type RemoveSpec struct {
	// SystemVolumeFilterRegex is a Go regular expression matched against backend
	// volume names. Matching volumes are treated as system volumes: excluded from
	// drain migration and deleted inline during the Verifying phase.
	// Defaults to `^sb-fio-baseline-.*`.
	// +optional
	SystemVolumeFilterRegex *string `json:"systemVolumeFilterRegex,omitempty"`
}

// StorageNodeOpsSpec defines the desired state of a StorageNodeOps.
type StorageNodeOpsSpec struct {
	// NodeRef is the name of the target StorageNode. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	NodeRef string `json:"nodeRef"`

	// Action is the operation to perform. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageNodeOpsAction `json:"action"`

	// Force enables forced execution where the backend supports it.
	// +optional
	Force *bool `json:"force,omitempty"`

	// ReattachVolume reattaches volumes during the node restart.
	// Applicable when action=Restart or action=Migrate.
	// +optional
	ReattachVolume *bool `json:"reattachVolume,omitempty"`

	// Migrate parameterizes action Migrate and is ignored by the others.
	// +optional
	Migrate *MigrateSpec `json:"migrate,omitempty"`

	// Remove parameterizes action Remove and is ignored by the others.
	// +optional
	Remove *RemoveSpec `json:"remove,omitempty"`
}

// MigrateParams returns the Migrate block, or a zero-valued one when the
// operation did not set it.
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

// StorageNodeOpsStatus holds the observed state of a StorageNodeOps.
type StorageNodeOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase StorageNodeOpsPhase `json:"phase,omitempty"`

	// SubPhase tracks the active drain step when action=Remove and phase=Running.
	// +optional
	SubPhase StorageNodeOpsSubPhase `json:"subPhase,omitempty"`

	// Message is a human-readable description of the current state or failure reason.
	// +optional
	Message string `json:"message,omitempty"`

	// VolumesMigrated is the count of volumes successfully migrated (drain only).
	// +optional
	VolumesMigrated int `json:"volumesMigrated,omitempty"`

	// VolumesPending is the count of volumes awaiting migration (drain only).
	// +optional
	VolumesPending int `json:"volumesPending,omitempty"`

	// Triggered indicates the backend action POST has been sent (used during
	// Suspending to avoid duplicate POSTs across reconcile iterations).
	// +optional
	Triggered bool `json:"triggered,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished (successfully or not).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snops
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SubPhase",type=string,JSONPath=".status.subPhase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageNodeOps is a one-shot operational CR targeting a single StorageNode.
// Analogous to a Kubernetes Job — it drives an action (Shutdown, Restart, Suspend,
// Resume, Remove, Migrate) to completion and records the result. Only one
// StorageNodeOps can be active per StorageNode at a time.
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
