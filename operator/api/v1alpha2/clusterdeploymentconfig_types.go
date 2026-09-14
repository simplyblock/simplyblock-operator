// A whole simplyblock deployment written down as one reviewable document.
//
// The kind exists because deploying simplyblock meant writing a StorageCluster
// and a StorageNode per worker by hand, each repeating what the others already
// said, with nothing between the writing and the provisioning. This is the
// thing an administrator reads once, corrects, and approves, after which the
// operator expands it into those objects.
//
// It is ephemeral by design. Everything the expansion produces is
// self-describing, so the document may be edited or deleted once it has been
// expanded and nothing reads it afterward — which is what lets a discovery run
// write a second one rather than editing the first.
//
// Specified by
// operator/docs/designs/crd-redesign/design-clusterdeploymentconfig.md, whose
// Appendix A is this file.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// JournalManagerSpec configures the journal managers on a set of nodes.
//
// It is declared here rather than borrowed from v1alpha1, as
// design-clusterdeploymentconfig.md §Appendix A spells it. A v1alpha2 type whose
// fields are v1alpha1 types cannot be reshaped by the redesign without changing
// this version's wire format, and it makes the import cycle that the conversion
// webhook needs impossible: the spoke's ConvertTo and ConvertFrom have to be
// methods on the v1alpha1 type, so v1alpha1 imports v1alpha2 and v1alpha2 cannot
// import back.
type JournalManagerSpec struct {
	// Count is the number of journal managers to configure.
	// +optional
	Count *int32 `json:"count,omitempty"`
	// PercentPerDevice is the journal manager capacity percentage per device.
	// +optional
	PercentPerDevice *int32 `json:"percentPerDevice,omitempty"`
}

// StripeSpec is the erasure-coding layout. Declared here for the reason
// JournalManagerSpec is.
type StripeSpec struct {
	// DataChunks defines the number of data chunks in the erasure-coding layout.
	// +optional
	DataChunks *int32 `json:"dataChunks,omitempty"`
	// ParityChunks defines the number of parity chunks in the erasure-coding layout.
	// +optional
	ParityChunks *int32 `json:"parityChunks,omitempty"`
}

// ClusterDeploymentConfigPhase is where the operator has got to with this
// document.
// +kubebuilder:validation:Enum=Draft;Expanding;Expanded;Failed
type ClusterDeploymentConfigPhase string

const (
	// ClusterDeploymentConfigPhaseDraft is an unapproved document. It is
	// validated on every reconcile and expanded on none.
	ClusterDeploymentConfigPhaseDraft ClusterDeploymentConfigPhase = "Draft"

	// ClusterDeploymentConfigPhaseExpanding is an approved document being
	// turned into a cluster and its nodes.
	ClusterDeploymentConfigPhaseExpanding ClusterDeploymentConfigPhase = "Expanding"

	// ClusterDeploymentConfigPhaseExpanded is a document whose objects exist.
	ClusterDeploymentConfigPhaseExpanded ClusterDeploymentConfigPhase = "Expanded"

	// ClusterDeploymentConfigPhaseFailed is a document the expansion refused or
	// could not finish.
	ClusterDeploymentConfigPhaseFailed ClusterDeploymentConfigPhase = "Failed"
)

// ClusterDeploymentConfigStep is one step of the expansion path.
// +kubebuilder:validation:Enum=Validating;CreatingCluster;AwaitingCluster;CreatingNodes
type ClusterDeploymentConfigStep string

const (
	ClusterDeploymentConfigStepValidating      ClusterDeploymentConfigStep = "Validating"
	ClusterDeploymentConfigStepCreatingCluster ClusterDeploymentConfigStep = "CreatingCluster"
	ClusterDeploymentConfigStepAwaitingCluster ClusterDeploymentConfigStep = "AwaitingCluster"
	ClusterDeploymentConfigStepCreatingNodes   ClusterDeploymentConfigStep = "CreatingNodes"
)

// KubernetesEnvironment is the distribution a deployment targets. The values are
// the distributions' own names, which is the exception design-crd-model.md §7.8
// carries for a word this group did not invent.
// +kubebuilder:validation:Enum=Vanilla;OpenShift;Rancher;K3s;Talos
type KubernetesEnvironment string

const (
	KubernetesEnvironmentVanilla   KubernetesEnvironment = "Vanilla"
	KubernetesEnvironmentOpenShift KubernetesEnvironment = "OpenShift"
	KubernetesEnvironmentRancher   KubernetesEnvironment = "Rancher"
	KubernetesEnvironmentK3s       KubernetesEnvironment = "K3s"
	KubernetesEnvironmentTalos     KubernetesEnvironment = "Talos"
)

// DeviceSelection is the explicit list of storage devices a group's workers hand
// to simplyblock. It carries no filter of any kind: a document whose meaning
// depends on what the hardware turns out to be is not a document a reviewer can
// approve, so filtering happens in the discovery run that produces the list and
// what lands here is the result. It expands to the matching fields of
// StorageNode.spec.config, which carry the same meanings.
//
// One member and not both. A cluster is built out of one class of backend
// storage, so a group hands over NVMe devices or logical block devices, and the
// rule below is the half of that a single group can be checked against. That
// every group of the document agrees is the spec's rule.
//
// +kubebuilder:validation:XValidation:rule="has(self.nvme) != has(self.block)",message="a device selection names NVMe addresses or block devices, not both"
type DeviceSelection struct {
	// NVMe names NVMe devices by PCI address ("0000:5e:00.0").
	// +kubebuilder:validation:items:Pattern=`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`
	// +kubebuilder:validation:items:MaxLength=32
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	// +optional
	NVMe []string `json:"nvme,omitempty"`

	// Block names logical block devices by path ("/dev/sdb"). It expands into the
	// same config.deviceNames as NVMe, which takes a PCI address and a device
	// path in one list. It is the alternative to NVMe rather than a companion of
	// it: the two classes are not mixed within a cluster.
	// +kubebuilder:validation:items:Pattern=`^/dev/[a-zA-Z0-9._/-]+$`
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	// +optional
	Block []string `json:"block,omitempty"`
}

// NodeGroup is a set of workers that share one configuration, which is what
// makes ten identical machines one entry rather than ten.
type NodeGroup struct {
	// Name identifies the group within its node set, for a reader and for the
	// events a validation failure emits.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Workers are the Kubernetes worker hostnames in this group.
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=200
	// +listType=set
	// +kubebuilder:validation:Required
	Workers []string `json:"workers"`

	// MgmtInterface is the management network interface the storage nodes bind.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	MgmtInterface string `json:"mgmtInterface,omitempty"`

	// DataInterfaces are the data-plane network interfaces.
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:MaxItems=32
	// +optional
	DataInterfaces []string `json:"dataInterfaces,omitempty"`

	// Devices selects the storage devices every worker in the group uses.
	// +optional
	Devices *DeviceSelection `json:"devices,omitempty"`

	// FailureDomain is the label of the fault group every worker in this group
	// belongs to ("rack-b"), which is usually the name of the rack, zone, or
	// power feed they share. Discovery seeds it from topology.kubernetes.io/zone
	// and leaves it unset where the Kubernetes API carries no topology, which
	// holds provisioning with a clear reason rather than guessing. It expands
	// into StorageNode.spec.config.failureDomain, whose shape it shares.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([-_.a-zA-Z0-9]*[a-zA-Z0-9])?$`
	// +optional
	FailureDomain string `json:"failureDomain,omitempty"`

	// SpdkSystemMemory is the memory the control plane starts SPDK with on these
	// nodes.
	// +kubebuilder:validation:Pattern=`^[0-9]+(G|GI|GB|GiB|M|MI|MB|MiB|g|gi|gb|gib|m|mi|mb|mib)?$`
	// +kubebuilder:validation:MaxLength=32
	// +optional
	SpdkSystemMemory string `json:"spdkSystemMemory,omitempty"`

	// JournalManager tunes the journal managers on these nodes.
	// +optional
	JournalManager *JournalManagerSpec `json:"journalManager,omitempty"`
}

// NodeSet is the organizational grouping of a deployment, usually a rack: the
// workers a document adds or grows together. It carries no sizing, because sizing
// is uniform across a cluster and is stated once in ClusterTemplate.
type NodeSet struct {
	// Name is the node set's name. It is copied to StorageNode.spec.nodeSet, so
	// that a node can be traced back to the part of the document that produced
	// it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Groups are the sets of workers sharing one configuration.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:Required
	Groups []NodeGroup `json:"groups"`
}

// ClusterTemplate is the StorageCluster the expansion creates, where it creates
// one. It carries the layout fields a cluster cannot change later, so that a
// reviewer sees them before the cluster exists rather than after.
type ClusterTemplate struct {
	// Name is the StorageCluster's name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// MaxSubsystemCount is the maximum number of NVMe-oF subsystems each storage
	// node of this cluster serves. Required, because the StorageCluster's own
	// field is, and no StorageNode carries a copy of it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=75
	MaxSubsystemCount *int32 `json:"maxSubsystemCount"`

	// VCPUCount is the number of vCPUs allocated to SPDK on each storage node of
	// this cluster. It is stated here and nowhere below, because the control
	// plane assumes it uniform across a cluster's nodes; CreatingNodes copies it
	// into every StorageNode.spec.config.sizing it writes. Required, because the
	// StorageCluster's own field is.
	// +kubebuilder:validation:Required
	// The floor is 4 rather than a hardware limit: a node must carry one core
	// beyond this budget for the system, and the control plane's core layout
	// assigns no NVMe-oF poller core at all for a 2-vCPU budget.
	// +kubebuilder:validation:Minimum=4
	VCPUCount *int32 `json:"vcpuCount"`

	// MinHugePagesSize is the smallest huge-page allocation each storage node of
	// this cluster makes: 100G or 1T, where a bare number is gigabytes. Like
	// VCPUCount it is the cluster's and is copied onto every node the expansion
	// writes. Omitted, each node uses the computed minimum.
	// +kubebuilder:validation:MaxLength=32
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`

	// Stripe is the erasure-coding layout.
	// +optional
	Stripe *StripeSpec `json:"stripe,omitempty"`

	// FabricType is the storage fabric.
	// +kubebuilder:validation:MaxLength=32
	// +optional
	FabricType string `json:"fabricType,omitempty"`

	// EnableFailureDomains opts the cluster into failure-domain mode, in which
	// every group must label the fault group its workers belong to.
	// +optional
	EnableFailureDomains *bool `json:"enableFailureDomains,omitempty"`
}

// ClusterDeploymentConfigSpec is a whole simplyblock deployment as one
// reviewable document.
//
// The third rule is the device class one. A document describes one cluster and a
// cluster is built out of one class of backend storage, so every group of every
// node set names the same member of its DeviceSelection. The expansion reads the
// class off them and stamps it onto the cluster it creates, which is why the
// document carries no field for it.
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self == oldSelf",message="an approved deployment config is immutable"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
// +kubebuilder:validation:XValidation:rule="self.nodeSets.all(s, s.groups.all(g, !has(g.devices) || !has(g.devices.block))) || self.nodeSets.all(s, s.groups.all(g, !has(g.devices) || !has(g.devices.nvme)))",message="every group must name the same device class: all nvme or all block"
type ClusterDeploymentConfigSpec struct {
	// Approved is the review gate. A document is expanded only once it is set,
	// and is validated but otherwise inert before that, which is what makes
	// reviewing a wrong document safe.
	// +optional
	Approved bool `json:"approved,omitempty"`

	// Environment is the Kubernetes distribution this deployment targets. It is a
	// shorthand the expansion spends: it sets enableKubeletConfiguration,
	// enableCpuTopology, ubuntuHost, and openShiftCluster on every StorageNode
	// the document produces, after which nothing reads it again.
	// +optional
	Environment KubernetesEnvironment `json:"environment,omitempty"`

	// EdgeCluster states that this is an edge deployment. An edge deployment
	// differs from a datacenter one in topology and scale rather than in kind,
	// so it is the same StorageCluster with fewer, smaller nodes.
	// +optional
	EdgeCluster *bool `json:"edgeCluster,omitempty"`

	// ClusterRef names an existing StorageCluster this document adds nodes to.
	// Absent means the document creates the cluster in Cluster. Setting it to a
	// cluster that does not exist, or leaving it absent when one already does,
	// is refused rather than reconciled.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// Cluster is the StorageCluster to create. Ignored when ClusterRef is set.
	// +optional
	Cluster *ClusterTemplate `json:"cluster,omitempty"`

	// NodeSets are the nodes the deployment is made of.
	//
	// The upper bound is what makes the device-class rule above estimable: the
	// API server costs a CEL rule against the largest value the schema permits,
	// and a list with no bound is costed as unbounded, which the rule's nested
	// all() then multiplies past the budget.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:Required
	NodeSets []NodeSet `json:"nodeSets"`
}

// ClusterDeploymentConfigStatus is the observed state of the document.
type ClusterDeploymentConfigStatus struct {
	// Phase is the operator's own view of the document.
	// +optional
	Phase ClusterDeploymentConfigPhase `json:"phase,omitempty"`

	// Step is the position of the expansion machine within Expanding.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','CreatingCluster','AwaitingCluster','CreatingNodes']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ClusterRef names the StorageCluster the expansion produced or added to. It
	// is a record rather than a dependency: nothing resolves it after expansion,
	// which is what makes the document safe to delete.
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// NodeRefs names the StorageNode objects the expansion created, for the same
	// reason.
	// +optional
	// +listType=set
	NodeRefs []string `json:"nodeRefs,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the document moves, and never a log. On a Draft it is what validation
	// found, which is what a reviewer reads before approving.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cdc
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".status.clusterRef"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ClusterDeploymentConfig is a whole simplyblock deployment written down as one
// reviewable document: the environment, the cluster, and the node sets with
// their workers, interfaces, and devices. An administrator reviews it, approves
// it, and the operator expands it into a StorageCluster and its StorageNodes.
//
// It is ephemeral. Everything the expansion produces is self-describing, so the
// document can be edited or deleted once it has been expanded, and nothing reads
// it afterward.
type ClusterDeploymentConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterDeploymentConfigSpec   `json:"spec,omitempty"`
	Status ClusterDeploymentConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDeploymentConfigList contains a list of ClusterDeploymentConfig.
type ClusterDeploymentConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDeploymentConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterDeploymentConfig{}, &ClusterDeploymentConfigList{})
}
