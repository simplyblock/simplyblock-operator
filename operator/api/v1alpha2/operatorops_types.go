// One operation performed against the operator itself.
//
// Every other Ops kind in this group names the entity it acts on. This one acts
// on the operator process, which is not a resource in this API group, and the
// absent target reference is the signal: the kind's name is the target.
//
// Today, it carries one action. A discovery run inspects the workers of a
// Kubernetes cluster and writes a ClusterDeploymentConfig describing what they
// have, which is a one-shot operation against no resource that produces a
// resource — exactly the shape an Ops kind exists for.
//
// Specified by
// operator/docs/designs/crd-redesign/design-clusterdeploymentconfig.md, whose
// Appendix B is this file.

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// OperatorOpsAction is the operation an OperatorOps performs.
//
// The field stays with one value in it. Every Ops kind in this group is
// dispatched on spec.action, so a kind that left its single action implicit
// would be the one kind whose shape has to change the day it gains a second.
// +kubebuilder:validation:Enum=Discover
type OperatorOpsAction string

const (
	OperatorOpsActionDiscover OperatorOpsAction = "Discover"
)

// OperatorOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type OperatorOpsPhase string

const (
	OperatorOpsPhasePending   OperatorOpsPhase = "Pending"
	OperatorOpsPhaseRunning   OperatorOpsPhase = "Running"
	OperatorOpsPhaseSucceeded OperatorOpsPhase = "Succeeded"
	OperatorOpsPhaseFailed    OperatorOpsPhase = "Failed"
	OperatorOpsPhaseAborted   OperatorOpsPhase = "Aborted"
)

// OperatorOpsStep is one step of a running operator operation. Which steps
// belong to which action is declared by that action's graph rather than by this
// type, which is why the enum stays flat as actions are added.
// +kubebuilder:validation:Enum=Inspecting;Probing;Writing
type OperatorOpsStep string

const (
	// OperatorOpsStepInspecting reads the cluster: which workers there are,
	// which distribution installed the kubelet, and what is already taken.
	OperatorOpsStepInspecting OperatorOpsStep = "Inspecting"

	// OperatorOpsStepProbing runs one probe Job per worker and waits for the
	// reports.
	OperatorOpsStepProbing OperatorOpsStep = "Probing"

	// OperatorOpsStepWriting turns the reports into a ClusterDeploymentConfig.
	OperatorOpsStepWriting OperatorOpsStep = "Writing"
)

// DeviceFilter narrows what a discovery run reports as a candidate device. It is
// an input to discovery and never appears in the document discovery writes: a
// ClusterDeploymentConfig carries the explicit list the filter produced, not the
// rule that produced it.
//
// The filters come in two sets, one per device class, and a run scans one class.
// The two rules below reject the set belonging to the class this run is not
// scanning, because a filter that will never be applied is one an administrator
// reads as having narrowed a draft that was never narrowed.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.enableLogicalBlockDevices) && self.enableLogicalBlockDevices) || !(has(self.pcieAllowList) || has(self.pcieDenyList) || has(self.pcieModel))",message="the PCI filters select NVMe devices and cannot be combined with enableLogicalBlockDevices; use blockAllowList and blockDenyList"
// +kubebuilder:validation:XValidation:rule="(has(self.enableLogicalBlockDevices) && self.enableLogicalBlockDevices) || !(has(self.blockAllowList) || has(self.blockDenyList))",message="blockAllowList and blockDenyList select logical block devices and require enableLogicalBlockDevices"
type DeviceFilter struct {
	// EnableLogicalBlockDevices scans a worker's available logical block devices
	// instead of its available NVMe devices. It selects the class rather than
	// adding one, because the draft a run writes describes one cluster and a
	// cluster is built out of one class. Unset scans NVMe, so that upgrading to
	// 26.4 does not change what a discovery run reports.
	// +optional
	EnableLogicalBlockDevices *bool `json:"enableLogicalBlockDevices,omitempty"`

	// EnablePartitionedDevices reports devices carrying a partition table
	// alongside the available ones, for the administrator who knows the table is
	// stale and intends to hand the device over anyway. It is the only one of the
	// three availability conditions that can be waived: a mounted or otherwise
	// busy device is never reported, because simplyblock taking it would corrupt
	// whatever is using it.
	// +optional
	EnablePartitionedDevices *bool `json:"enablePartitionedDevices,omitempty"`

	// PcieAllowList restricts candidates to these PCI addresses. This and the two
	// PCI filters below narrow the NVMe class alone, because a logical block
	// device has no PCI address to match, so setting any of them on a run that
	// scans the block class is rejected by the rule on this type.
	// +listType=set
	// +optional
	PcieAllowList []string `json:"pcieAllowList,omitempty"`

	// PcieDenyList excludes these PCI addresses. On a fleet that is uniform
	// about which slot holds the boot device, this is what keeps that device out
	// of every group of every draft.
	// +listType=set
	// +optional
	PcieDenyList []string `json:"pcieDenyList,omitempty"`

	// PcieModel restricts candidates to devices whose PCI model string matches.
	// +optional
	PcieModel string `json:"pcieModel,omitempty"`

	// BlockAllowList restricts candidates to these device paths ("/dev/sdb").
	// This and BlockDenyList are the block class's half of the filter, and they
	// require EnableLogicalBlockDevices for the same reason the PCI filters
	// forbid it.
	// +kubebuilder:validation:items:Pattern=`^/dev/[a-zA-Z0-9._/-]+$`
	// +listType=set
	// +optional
	BlockAllowList []string `json:"blockAllowList,omitempty"`

	// BlockDenyList excludes these device paths. On a fleet that boots from
	// /dev/sda, this is the one entry that keeps the root disk out of every group
	// of every draft.
	// +kubebuilder:validation:items:Pattern=`^/dev/[a-zA-Z0-9._/-]+$`
	// +listType=set
	// +optional
	BlockDenyList []string `json:"blockDenyList,omitempty"`

	// DriveSizeRange restricts candidates by size ("100G-2T"). Unlike the
	// per-class filters, it applies to whichever class the run is scanning.
	// +optional
	DriveSizeRange string `json:"driveSizeRange,omitempty"`
}

// DiscoverSpec parameterizes the Discover action.
//
// Which workers a run inspects is stated one of two ways and never both: by name
// in Workers, or by label in NodeSelector. They are exclusive rather than
// intersected because the intersection of a name list and a label selector is a
// question nobody asks deliberately, and reading one as narrowing the other
// would make a run inspect fewer machines than either field says.
// +kubebuilder:validation:XValidation:rule="!(has(self.workers) && size(self.workers) > 0 && has(self.nodeSelector) && size(self.nodeSelector) > 0)",message="spec.discover names workers and also carries a nodeSelector; state one or the other"
type DiscoverSpec struct {
	// ConfigName is the ClusterDeploymentConfig to write. Absent generates one
	// from the run's timestamp, so that a second discovery never overwrites the
	// first, which may have been reviewed and corrected.
	// +optional
	ConfigName string `json:"configName,omitempty"`

	// Workers are the workers to inspect, by node name.
	//
	// It is the answer to inspecting two named machines, which a label selector
	// can only express by labeling them first: a selector's entries are ANDed,
	// so two hostnames in one selector match nothing at all.
	//
	// A named worker that does not exist, or that is declined for one of the
	// reasons any worker is declined, is reported by the same event the selector
	// path reports it by. Naming a worker is a statement about which machines to
	// consider, not a claim that each of them will be used.
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	Workers []string `json:"workers,omitempty"`

	// NodeSelector restricts which workers are inspected. Empty inspects every
	// schedulable worker, and it is exclusive with Workers.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are what the probe pods tolerate, and what the draft states
	// for the storage nodes it proposes.
	//
	// A probe is pinned to its worker with spec.nodeName rather than scheduled
	// onto it, which bypasses the scheduler and not the taints: a NoSchedule
	// taint still keeps the pod off, and a NoExecute taint evicts one that
	// landed. A fleet that dedicates machines to storage taints them, so a run
	// against one that tolerates nothing inspects nothing.
	//
	// They reach the draft as well, because the taints a run was allowed to
	// probe through are the taints the cluster it proposes has to live with.
	// Stating them in one place is what keeps a reviewer from approving a
	// document whose DaemonSet schedules nowhere.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// EnableControlPlaneNodes lets the run consider machines that run the API
	// server and etcd.
	//
	// It is off by default because a storage node is a data path, and putting one
	// on an etcd host is a placement almost nobody intends. The approval gate is a
	// poor place to catch it: a fifty-worker draft is not a document anybody reads
	// closely enough to spot three control-plane nodes in it. A combined three-node
	// or single-node deployment is the case that wants it, and those are set up
	// deliberately.
	//
	// There is no field beside it for infrastructure nodes, because those are used
	// without asking: an OpenShift infra node is the tier a cluster's own
	// infrastructure runs on, and simplyblock storage is infrastructure. A fleet
	// with disks in its infra nodes meant those disks to be the storage, so a draft
	// proposes them ahead of the workers rather than leaving them out.
	// +optional
	EnableControlPlaneNodes *bool `json:"enableControlPlaneNodes,omitempty"`

	// DeviceFilter narrows which of an inspected worker's devices reach the
	// draft. Empty reports every device the worker advertises, including the one
	// it boots from, which is what the approval gate then has to catch.
	// +optional
	DeviceFilter *DeviceFilter `json:"deviceFilter,omitempty"`

	// ClusterRef names an existing StorageCluster the draft grows rather than
	// creates. It is copied to the draft's own clusterRef, so that re-running
	// discovery after an expansion produces a growth document naming the same
	// cluster.
	//
	// Bounded at what a StorageCluster name may be, since a longer value names
	// nothing that can exist (design-api-upgrade.md §19.4).
	// +kubebuilder:validation:MaxLength=63
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`
}

// OperatorOpsSpec is one operation to perform against the operator itself.
//
// It carries no target reference. Every other Ops kind in this group names the
// entity it acts on; this one acts on the operator process, which is not a
// resource in this API group, and the absent field is the signal.
type OperatorOpsSpec struct {
	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action OperatorOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	// Whether an abort is expressible from the current step is declared by that
	// action's graph rather than checked here. Discovery changes nothing, so an
	// aborted run leaves behind at most the config it had already written.
	// +optional
	Abort bool `json:"abort,omitempty"`

	// Discover parameterizes action Discover.
	// +optional
	Discover *DiscoverSpec `json:"discover,omitempty"`
}

// OperatorOpsStatus is the observed state of one operator operation.
type OperatorOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase OperatorOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Inspecting','Probing','Writing']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ConfigRef names the ClusterDeploymentConfig a Discover run wrote.
	// +optional
	ConfigRef string `json:"configRef,omitempty"`

	// Workers are the workers this run is inspecting, decided once in
	// Inspecting so that a node joining the cluster mid-run does not change
	// what the run is about.
	// +optional
	// +listType=set
	Workers []string `json:"workers,omitempty"`

	// Environment is the Kubernetes distribution Inspecting concluded, which
	// Writing copies into the draft. It is recorded here as well so that a run
	// that failed later still says what it found.
	// +optional
	Environment KubernetesEnvironment `json:"environment,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=oops
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Config",type=string,JSONPath=".status.configRef"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// OperatorOps is a single operation performed against the operator itself,
// which today means a discovery run that writes a ClusterDeploymentConfig. It
// runs to a terminal phase and stays afterward as the audit record.
type OperatorOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OperatorOpsSpec   `json:"spec,omitempty"`
	Status OperatorOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OperatorOpsList contains a list of OperatorOps.
type OperatorOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OperatorOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OperatorOps{}, &OperatorOpsList{})
}
