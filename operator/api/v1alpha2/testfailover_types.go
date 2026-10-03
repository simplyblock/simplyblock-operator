// One non-disruptive test-failover drill.
//
// A test failover proves an application can be recovered from a point-in-time
// copy, in isolation, without disturbing the running production. The recovery
// point is always a snapshot and the result is always a clone, so the source is
// never touched. The hub coordinates the drill: it reads the source on its
// cluster, resolves a recovery point on the recovery cluster's backend, clones
// it there, and places the clone as a bound PVC in an isolated namespace on the
// recovery cluster. Deleting the object reclaims the clone and any snapshot the
// drill took.
//
// Specified by operator/docs/designs/design-test-failover.md, whose Appendix A
// is this file.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// TestFailoverScope selects what a drill recovers.
// +kubebuilder:validation:Enum=Volume;Group
type TestFailoverScope string

const (
	// TestFailoverScopeVolume recovers a single source volume, named by a PVC.
	TestFailoverScopeVolume TestFailoverScope = "Volume"

	// TestFailoverScopeGroup recovers a consistency group from one
	// group-consistent point.
	TestFailoverScopeGroup TestFailoverScope = "Group"
)

// TestFailoverPhase is the drill's own progress.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;TearingDown
type TestFailoverPhase string

const (
	TestFailoverPhasePending      TestFailoverPhase = "Pending"
	TestFailoverPhaseProvisioning TestFailoverPhase = "Provisioning"
	TestFailoverPhaseReady        TestFailoverPhase = "Ready"
	TestFailoverPhaseFailed       TestFailoverPhase = "Failed"
	TestFailoverPhaseTearingDown  TestFailoverPhase = "TearingDown"
)

// TestFailoverStep is one step of a running drill. Which steps belong to which
// phase of the flow is declared by the drill's graph rather than by this type,
// which is why the enum stays flat.
// +kubebuilder:validation:Enum=ResolvingSource;ResolvingPoint;Shipping;Cloning;Placing;Releasing
type TestFailoverStep string

const (
	// TestFailoverStepResolvingSource reads the source PVC and PV on the source
	// cluster through a ManagedClusterView to learn the source volume's handle.
	TestFailoverStepResolvingSource TestFailoverStep = "ResolvingSource"

	// TestFailoverStepResolvingPoint resolves the recovery point on the recovery
	// cluster's backend: a fresh source snapshot, or the latest replicated one.
	TestFailoverStepResolvingPoint TestFailoverStep = "ResolvingPoint"

	// TestFailoverStepShipping ships the recovery point to a backend that holds no
	// copy of it (the cross-cluster, separate-backend case).
	TestFailoverStepShipping TestFailoverStep = "Shipping"

	// TestFailoverStepCloning clones the recovery point into a writable volume on
	// the recovery cluster's backend.
	TestFailoverStepCloning TestFailoverStep = "Cloning"

	// TestFailoverStepPlacing delivers the bubble PV and PVC to the recovery
	// cluster and waits for the PVC to bind.
	TestFailoverStepPlacing TestFailoverStep = "Placing"

	// TestFailoverStepReleasing tears the drill down: removes the placed objects,
	// reclaims the clone, and deletes any snapshot the drill took.
	TestFailoverStepReleasing TestFailoverStep = "Releasing"
)

// TestFailoverSpec is the request for one non-disruptive test-failover drill.
//
// The source is named by where it runs and what it is, so the hub can find it
// without anyone extracting a backend handle by hand. SourceNamespace is
// required for a Volume drill, where the source is a PVC, and unused for a Group
// drill, where SourceRef names a consistency group.
// +kubebuilder:validation:XValidation:rule="self.scope != 'Volume' || has(self.sourceNamespace)",message="sourceNamespace is required for scope=Volume"
type TestFailoverSpec struct {
	// Scope selects what the drill recovers. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Scope TestFailoverScope `json:"scope"`

	// SourceCluster is the OCM ManagedCluster the source runs on. The hub reads
	// the source there through a ManagedClusterView. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	SourceCluster string `json:"sourceCluster"`

	// SourceNamespace is the namespace of the source PVC on SourceCluster.
	// Required for scope=Volume. Immutable.
	// +optional
	// +k8s:immutable
	SourceNamespace string `json:"sourceNamespace,omitempty"`

	// SourceRef names the source on SourceCluster: a PersistentVolumeClaim in
	// SourceNamespace (scope=Volume), or a consistency group (scope=Group).
	// Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	SourceRef string `json:"sourceRef"`

	// BubbleCluster is the OCM ManagedCluster to recover onto: a DR target holding
	// the replicated point, or another cluster. It must differ from SourceCluster;
	// test-failover recovers onto a different cluster, never in place. Immutable.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	BubbleCluster string `json:"bubbleCluster"`

	// BubbleNamespace is the namespace on the bubble cluster where the recovered
	// PVCs are created. Immutable.
	// +kubebuilder:default=bubble
	// +optional
	// +k8s:immutable
	BubbleNamespace string `json:"bubbleNamespace,omitempty"`

	// TTLSeconds is an optional maximum lifetime: the drill is torn down after it
	// even without a delete, so a forgotten drill cannot hold a clone forever.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
}

// TestFailoverClone is one recovered volume: the source it came from, the
// snapshot and clone the drill built, and the PVC placed on the bubble cluster.
type TestFailoverClone struct {
	// SourceRef is the source volume, or group member, the recovered volume maps
	// to.
	SourceRef string `json:"sourceRef"`

	// SourceHandle is the source volume's backend handle, read from its PV.
	// +optional
	SourceHandle string `json:"sourceHandle,omitempty"`

	// SnapshotID is the recovery-point snapshot: the replicated snapshot already on
	// the bubble cluster's backend that the clone is built from.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// CloneID is the backend id of the writable clone.
	// +optional
	CloneID string `json:"cloneID,omitempty"`

	// PVCName is the bound PVC in the bubble namespace on the bubble cluster.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// SizeBytes is the recovered volume's size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// SourceVolumeContext is the source PV's CSI volumeAttributes, minus the
	// identity and provisioner keys, carried onto the bubble PV so the node plugin
	// receives a non-nil VolumeContext when it stages the clone. The clone's own
	// identity (NQN, connections, nsId, and so on) is re-resolved from the clone
	// handle at stage time, so only the class-level parameters are carried; the
	// identity keys are dropped so a failed clone lookup can never point the mount
	// back at the source.
	// +optional
	SourceVolumeContext map[string]string `json:"sourceVolumeContext,omitempty"`

	// SourceFSType is the source PV's CSI fsType, carried onto the bubble PV so the
	// node plugin stages the clone with the filesystem it actually carries. The
	// clone is a block copy of the source, so its filesystem is the source's; an
	// empty fsType makes the node plugin default to ext4 and refuse to mount an XFS
	// volume.
	// +optional
	SourceFSType string `json:"sourceFSType,omitempty"`
	// SourceVolumeMode is the source PV's volumeMode (Filesystem or Block),
	// carried onto the bubble PV and PVC. A VM's disk is a Block claim; a bubble
	// claim that omitted the mode defaulted to Filesystem and the kubelet asked
	// the node plugin to mount a raw guest disk (2026-10-03).
	// +optional
	SourceVolumeMode string `json:"sourceVolumeMode,omitempty"`
}

// TestFailoverReport is the evidence a drill produces.
type TestFailoverReport struct {
	// BubbleCluster is the cluster the drill recovered onto.
	// +optional
	BubbleCluster string `json:"bubbleCluster,omitempty"`

	// RecoveryPoint is the snapshot or group generation the drill recovered.
	// +optional
	RecoveryPoint string `json:"recoveryPoint,omitempty"`

	// RecoveryPointTime is when that point was taken.
	// +optional
	RecoveryPointTime *metav1.Time `json:"recoveryPointTime,omitempty"`

	// RecoveryPointAgeSeconds is the drill time minus the recovery-point time.
	// +optional
	RecoveryPointAgeSeconds int64 `json:"recoveryPointAgeSeconds,omitempty"`

	// InvariantsHeld is true only when the source fingerprint taken before the
	// drill matches the one taken at Ready. A Ready drill with this false is a
	// defect.
	// +optional
	InvariantsHeld bool `json:"invariantsHeld,omitempty"`
}

// TestFailoverStatus is the observed state of one drill.
type TestFailoverStatus struct {
	// Phase is the drill's own progress.
	// +optional
	Phase TestFailoverPhase `json:"phase,omitempty"`

	// Step is the position of the running drill's state machine.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['ResolvingSource','ResolvingPoint','Shipping','Cloning','Placing','Releasing']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as the
	// drill moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// Triggered records that the current step's side effect was issued, so a
	// restart does not repeat it.
	// +optional
	Triggered bool `json:"triggered,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Clones is one entry per recovered volume.
	// +optional
	// +listType=map
	// +listMapKey=sourceRef
	Clones []TestFailoverClone `json:"clones,omitempty"`

	// Report is the drill's evidence, populated as it reaches Ready.
	// +optional
	Report *TestFailoverReport `json:"report,omitempty"`

	// StartedAt is when the drill started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// ReadyAt is when every recovered PVC became bound.
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`

	// CompletedAt is when the drill reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tfo
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=".spec.scope"
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=".spec.sourceRef"
// +kubebuilder:printcolumn:name="On",type=string,JSONPath=".spec.sourceCluster"
// +kubebuilder:printcolumn:name="Bubble",type=string,JSONPath=".spec.bubbleCluster"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// TestFailover is a one-way, non-disruptive test-failover drill. It recovers a
// source volume, or a consistency group, from a snapshot into an isolated
// namespace on a chosen cluster as bound PVCs, without touching the source. The
// hub reads the source on its cluster and places the bubble on the recovery
// cluster through OCM. It runs to a terminal phase, or holds Ready until it is
// deleted, and deletion reclaims the clones and any snapshots the drill took.
type TestFailover struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TestFailoverSpec   `json:"spec,omitempty"`
	Status TestFailoverStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TestFailoverList contains a list of TestFailover.
type TestFailoverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TestFailover `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TestFailover{}, &TestFailoverList{})
}
