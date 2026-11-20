// FleetMember: one enrolled Kubernetes cluster, addressed by the Open Cluster
// Management ManagedCluster it is.
//
// Its spec holds only what cannot be observed, which is which cluster it is and
// what a detachment does to what the fleet applied there. Everything else about
// a member is reported, and the status splits into three blocks that come from
// different places and go stale independently: the link block is the member's
// own report, the inventory block summarizes its discovery reports, and the
// storage block is read from the control plane. Each carries when it was last
// current, because a member's add-on losing the hub and that member's storage
// nodes losing the control plane are usually the same network event, and a
// console that renders one panel out of the three loses the distinction.
//
// This kind is the root of the ownership tree the hub keeps: every object that
// names a member is owned by it, so detaching collects the member's deployments,
// its classes, and its operations.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fm
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Workers",type=integer,JSONPath=".status.inventory.eligibleWorkers"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.storage.nodesOnline"
// +kubebuilder:printcolumn:name="Contact",type=date,JSONPath=".status.link.lastContact"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// FleetMember is one enrolled Kubernetes cluster. Its spec says which cluster
// and what a detachment does, because everything else about a member is
// observed rather than declared.
type FleetMember struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetMemberSpec   `json:"spec,omitempty"`
	Status FleetMemberStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetMemberList is a list of FleetMember.
type FleetMemberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetMember `json:"items"`
}

// FleetMemberSpec identifies the Kubernetes cluster a member is.
type FleetMemberSpec struct {
	// ClusterRef names the Open Cluster Management ManagedCluster this member is.
	// That kind is cluster-scoped and its names are unique, so a bare name is
	// unambiguous.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClusterRef string `json:"clusterRef"`

	// DetachPolicy is what happens to what the fleet applied when this member is
	// deleted. Retain orphans it, leaving a working standalone deployment.
	// +kubebuilder:default=Retain
	// +optional
	DetachPolicy FleetDetachPolicy `json:"detachPolicy,omitempty"`

	// InventoryRefreshInterval is how often the hub asks the member to re-probe
	// its workers. Absent inherits the fleet's default, and zero means never.
	// +optional
	InventoryRefreshInterval *metav1.Duration `json:"inventoryRefreshInterval,omitempty"`
}

// FleetDetachPolicy is what a member's deletion does to what the fleet applied.
// +kubebuilder:validation:Enum=Retain;Delete
type FleetDetachPolicy string

const (
	// FleetDetachPolicyRetain orphans what the fleet applied, so the member is
	// left a working standalone deployment.
	FleetDetachPolicyRetain FleetDetachPolicy = "Retain"
	// FleetDetachPolicyDelete removes what the fleet applied.
	FleetDetachPolicyDelete FleetDetachPolicy = "Delete"
)

// FleetMemberStatus is the member's readiness and three blocks that come from
// different places and go stale independently.
type FleetMemberStatus struct {
	// Phase is the member's readiness to be deployed into.
	// +optional
	Phase FleetMemberPhase `json:"phase,omitempty"`

	// Link is what the add-on reports about itself, and it goes stale with it.
	// +optional
	Link *FleetMemberLink `json:"link,omitempty"`

	// Inventory is a summary and never the reports, which stay in the member.
	// +optional
	Inventory *FleetMemberInventory `json:"inventory,omitempty"`

	// Storage is read from the control plane rather than from the member.
	// +optional
	Storage *FleetMemberStorage `json:"storage,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FleetMemberPhase is a member's readiness to be deployed into.
// +kubebuilder:validation:Enum=Pending;Enrolling;Ready;Degraded;Unreachable;Detaching
type FleetMemberPhase string

const (
	// FleetMemberPhasePending means the ManagedCluster is not accepted yet.
	FleetMemberPhasePending FleetMemberPhase = "Pending"
	// FleetMemberPhaseEnrolling means the add-on is being installed.
	FleetMemberPhaseEnrolling FleetMemberPhase = "Enrolling"
	// FleetMemberPhaseReady means the member can be deployed into.
	FleetMemberPhaseReady FleetMemberPhase = "Ready"
	// FleetMemberPhaseDegraded means the member answers, and something in it does not.
	FleetMemberPhaseDegraded FleetMemberPhase = "Degraded"
	// FleetMemberPhaseUnreachable means the member is not answering the hub.
	FleetMemberPhaseUnreachable FleetMemberPhase = "Unreachable"
	// FleetMemberPhaseDetaching means the member's deletion is being performed.
	FleetMemberPhaseDetaching FleetMemberPhase = "Detaching"
)

// FleetMemberLink is what the add-on reports about the member it runs in.
type FleetMemberLink struct {
	// AddOnVersion is the add-on's own version.
	// +optional
	AddOnVersion string `json:"addOnVersion,omitempty"`

	// OperatorVersion is the simplyblock operator the member runs.
	// +optional
	OperatorVersion string `json:"operatorVersion,omitempty"`

	// DriverVersion is the CSI driver the member runs.
	// +optional
	DriverVersion string `json:"driverVersion,omitempty"`

	// StorageAPIVersions are the versions of storage.simplyblock.io the member
	// serves, which is what makes schema skew visible before a payload is pruned
	// by it rather than after.
	// +optional
	StorageAPIVersions []string `json:"storageAPIVersions,omitempty"`

	// LastContact is when the add-on last reported.
	// +optional
	LastContact *metav1.Time `json:"lastContact,omitempty"`
}

// FleetMemberInventory summarizes what a member's workers have. The reports
// themselves stay in the member, because a fifty-worker set is tens of
// megabytes and one member's copy is all a composition needs.
type FleetMemberInventory struct {
	// Workers is how many the member has.
	// +optional
	Workers int32 `json:"workers,omitempty"`

	// EligibleWorkers is how many a cluster could be built on.
	// +optional
	EligibleWorkers int32 `json:"eligibleWorkers,omitempty"`

	// DeviceClasses are the classes present, so a fleet-wide list can answer
	// which members could take a cluster of a class without fetching a report.
	// +optional
	DeviceClasses []string `json:"deviceClasses,omitempty"`

	// RawCapacity is the sum of the candidate devices.
	// +optional
	RawCapacity *resource.Quantity `json:"rawCapacity,omitempty"`

	// Revision identifies the report set this summary was computed from.
	// +optional
	Revision string `json:"revision,omitempty"`

	// ObservedAt is when the summary was computed, which is what stops a stale
	// one reading as current.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// UnreadableReports counts workers whose report could not be parsed, which
	// is otherwise reported only by an event inside the member.
	// +optional
	UnreadableReports int32 `json:"unreadableReports,omitempty"`
}

// FleetMemberStorage is the control plane's count of what the member holds.
type FleetMemberStorage struct {
	// Clusters is how many storage clusters the member holds.
	// +optional
	Clusters int32 `json:"clusters,omitempty"`

	// Nodes is how many storage nodes those clusters have.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`

	// NodesOnline is how many of them answer.
	// +optional
	NodesOnline int32 `json:"nodesOnline,omitempty"`

	// Pools is how many pools the member's clusters carry.
	// +optional
	Pools int32 `json:"pools,omitempty"`

	// DevicesDegraded is how many devices are serving and should not be.
	// +optional
	DevicesDegraded int32 `json:"devicesDegraded,omitempty"`

	// ObservedAt is when the control plane was last read, so that a stale
	// projection is distinguishable from a current one.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

func init() {
	SchemeBuilder.Register(&FleetMember{}, &FleetMemberList{})
}
