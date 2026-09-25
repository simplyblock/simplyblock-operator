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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// JournalManagerSpec, the journal tuning this document's node-set template
// states, is StorageNode's own type in storagenode_types.go. It was declared
// here while that kind was still v1alpha1, for the reason StripeSpec was, and
// moved to the kind that owns the concept once it arrived: a journal count and a
// per-device share are one node's on-disk layout, fixed when its devices were
// partitioned.

// StripeSpec, the erasure-coding layout this document's template states, is
// StorageCluster's own type in storagecluster_types.go. It was declared here
// while that kind was still v1alpha1, for the reason JournalManagerSpec is, and
// moved to the kind that owns the concept once it arrived.

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
// +kubebuilder:validation:Enum=Validating;CreatingCluster;AwaitingCluster;CreatingNodes;Activating
type ClusterDeploymentConfigStep string

const (
	ClusterDeploymentConfigStepValidating      ClusterDeploymentConfigStep = "Validating"
	ClusterDeploymentConfigStepCreatingCluster ClusterDeploymentConfigStep = "CreatingCluster"
	ClusterDeploymentConfigStepAwaitingCluster ClusterDeploymentConfigStep = "AwaitingCluster"
	ClusterDeploymentConfigStepCreatingNodes   ClusterDeploymentConfigStep = "CreatingNodes"

	// ClusterDeploymentConfigStepActivating waits for the nodes this document
	// created and then asks for the cluster to be activated.
	//
	// The document knows how many nodes it made, so it knows when the deployment
	// it describes is whole. Stopping at "the objects exist" would leave a
	// cluster that serves nothing behind a document reporting Expanded, with
	// nothing saying that one more thing is required of anybody.
	ClusterDeploymentConfigStepActivating ClusterDeploymentConfigStep = "Activating"
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

// HostOSFamily is the packaging tradition a Linux distribution belongs to.
//
// It is the coarse half of what a host OS is, and the half most decisions are
// actually about: what differs between Ubuntu and Debian is rarely what a
// storage node needs, and what differs between Ubuntu and Rocky always is.
// There is no member for a host with no package manager: Talos and Flatcar are
// not a family with no name, they are machines where the question does not
// arise, and a document describing one leaves the family unstated.
//
// +kubebuilder:validation:Enum=Debian;RedHat;SUSE;Alpine;Arch
type HostOSFamily string

const (
	HostOSFamilyDebian HostOSFamily = "Debian"
	HostOSFamilyRedHat HostOSFamily = "RedHat"
	HostOSFamilySUSE   HostOSFamily = "SUSE"
	HostOSFamilyAlpine HostOSFamily = "Alpine"
	HostOSFamilyArch   HostOSFamily = "Arch"
)

// DistroUbuntu is the one distribution the expansion decides anything by.
//
// Ubuntu keeps the NVMe-oF modules in a package the base install does not
// carry, so a storage node on one installs linux-modules-extra for its kernel
// before it starts and a node on anything else does not. That is what
// StorageCluster.spec.storageNodes.ubuntuHost states, and stating it is the
// whole of what spec.hostOS.distro is spent on.
const DistroUbuntu = "ubuntu"

// HostOSSpec is the operating system a deployment's workers run.
//
// It is a fact about the machines rather than about Kubernetes, which is why it
// is stated here and not derived from spec.environment: a fleet on OpenShift
// runs Red Hat Enterprise Linux CoreOS, and a fleet on K3s runs whatever its
// administrator installed. A discovery run fills it in from what the probes
// read, and fills it in only when every worker agrees, so a document that
// states one is a document whose fleet is uniform.
type HostOSSpec struct {
	// Distro is the distribution's os-release ID, lowercase and verbatim:
	// `ubuntu`, `rocky`, `rhel`, `talos`. It is what the expansion reads.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]*$`
	Distro string `json:"distro,omitempty"`

	// Family is the packaging tradition Distro belongs to. A discovery run
	// concludes it from the distribution itself, or from the distributions its
	// os-release says it is built on, which is what places a derivative this
	// product has never heard of. It is left unstated for a host with no
	// package manager.
	// +optional
	Family HostOSFamily `json:"family,omitempty"`
}

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
	// Name is the StorageCluster's name, and is therefore held to what such a
	// name may be rather than to what an object name may be. A longer value is a
	// document the API server accepts and a CreatingCluster step that can never
	// succeed, since the cluster it would write is one the API server refuses
	// (design-api-upgrade.md §19.4).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
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

	// EnableDriveFormat formats every device the document names before a storage
	// node takes it, which is how a drive carrying anything already is made
	// usable.
	//
	// It says what is wanted rather than how, because the how differs by device
	// class: an NVMe device is formatted to a 4K block size, and a logical block
	// device has its signatures wiped. One field covers both, so a document does
	// not have to know which class the expansion will resolve it to.
	//
	// It is on the document rather than defaulted further down because it is
	// destructive and the document is what somebody approves. A reviewer reading
	// a draft has to see that the drives it lists will be formatted, and be able
	// to strike it before approving; the cluster's own field is immutable once
	// the cluster exists, so a default nobody saw could not be undone either.
	// +optional
	EnableDriveFormat *bool `json:"enableDriveFormat,omitempty"`

	// EnableJournalDevice dedicates the smallest NVMe device on each of this
	// deployment's workers to the journal manager, instead of carving a journal
	// partition out of every device.
	//
	// It is here rather than on a node set because it is immutable on the cluster
	// it lands on, for the reason SocketsToUse is: the on-disk layout a fleet was
	// built with is not one a later document can vary. It also costs a drive of
	// capacity per node, which is a trade a reviewer approves rather than one a
	// default makes for them.
	// +optional
	EnableJournalDevice *bool `json:"enableJournalDevice,omitempty"`

	// SocketsToUse restricts the deployment to selected NUMA sockets, and empty
	// means socket 0 alone. With NodesPerSocket it decides how many storage nodes
	// each worker runs, so a group of two workers on a two-socket layout expands
	// to four nodes.
	//
	// It is here rather than on a node set because it is immutable on the cluster
	// it lands on: the layout a fleet was built with is not one a later document
	// can vary, and a reviewer should see it before the cluster exists.
	// +kubebuilder:validation:items:MaxLength=16
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +optional
	SocketsToUse []string `json:"socketsToUse,omitempty"`

	// NodesPerSocket is how many storage nodes run per NUMA socket. See
	// SocketsToUse, which it multiplies.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +optional
	NodesPerSocket *int32 `json:"nodesPerSocket,omitempty"`

	// NodeProvisioningBudget is how many workers the expansion may have in the
	// node-add process at once. It expands into the cluster's own
	// spec.storageNodes.nodeProvisioningBudget, whose meaning it shares: the cap
	// is counted by distinct worker, so a two-socket host spends one of the
	// budget, and a worker hosting a FoundationDB pod is sequential whatever the
	// budget says.
	//
	// It is on the document because a document is what states the size of a
	// deployment, and a deployment of thirty workers added one at a time is the
	// difference between an afternoon and a week. Omitted, the cluster's default
	// of one applies, which is the serial behavior.
	// +kubebuilder:validation:Minimum=1
	// +optional
	NodeProvisioningBudget *int32 `json:"nodeProvisioningBudget,omitempty"`

	// EnableChecksumValidation turns on inline CRC validation of every I/O, for
	// silent-data-error protection.
	//
	// It is on the document because it is immutable on the cluster it lands on:
	// the backend bakes the checksum method into each device when the cluster is
	// created and never re-applies it, so a cluster created without this is one
	// nobody can turn it on for. A deployment that wants its data checked has to
	// say so here or not at all.
	// +optional
	EnableChecksumValidation *bool `json:"enableChecksumValidation,omitempty"`

	// EnableAtomicity4K enforces 4K write atomicity on every device this
	// deployment names, which is what lets checksum validation run on devices
	// whose logical block size is under the data plane's 4K minimum.
	//
	// It is the route to checked I/O on a device that cannot be reformatted: a
	// logical block device's block size is fixed by the drive, and some NVMe
	// devices offer no 4K format either. Where a device can be reformatted,
	// EnableDriveFormat is the other route and this is unnecessary.
	//
	// It is an enforcement because the question is often unanswerable. A SATA
	// drive presenting 512-byte logical blocks over a 4K physical sector reports
	// 512 and nothing more, and a kernel older than 6.11 publishes no atomic
	// write attributes at all. Where a device does answer, the storage node's
	// report carries it, and a reviewer approves this against that rather than
	// against a vendor's datasheet -- because enforcing a guarantee the hardware
	// does not keep is how a torn write becomes a checksum that silently
	// disagrees with it.
	//
	// It means nothing unless EnableChecksumValidation is set, which is the
	// cluster's own rule and is left to the cluster to enforce.
	// +optional
	EnableAtomicity4K *bool `json:"enableAtomicity4K,omitempty"`

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

	// KMS selects where the cluster stores volume encryption keys. It is here
	// rather than left to be set on the StorageCluster afterward because the
	// expansion's own reconciler reads it back off that object on the very next
	// pass, before anything external could patch it in; stating it on the
	// document is what makes it present at the cluster's creation rather than a
	// race with one.
	// +optional
	KMS *KMSSpec `json:"kms,omitempty"`
}

// ImageSpec is one container image and when to pull it, which is the pair every
// image in this product is stated as.
//
// Both members are optional so that each can be stated without the other: an
// air-gapped deployment overrides the image and keeps the policy, and a
// development one keeps the image and pins the policy to IfNotPresent so a tag
// rebuilt in place is not picked up mid-deployment.
type ImageSpec struct {
	// Image is the repository and tag, optionally digest-pinned. An empty value
	// is not written downstream, so the field it would fill keeps its own
	// default rather than being overridden with nothing.
	//
	// The registry set is the one every image field in this API is held to,
	// which is what keeps a reviewed document from naming a build nobody
	// published.
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy is when that image is pulled. It defaults to Always,
	// because every image this product ships by default is a moving tag and a
	// node brought up after a release otherwise runs what its kubelet held.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=Always
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// DeploymentImages is every image a deployment pins, in one block.
//
// All three are software that runs on a storage node, which is what the slot
// names say and what an earlier spelling of the first one hid. They are together
// rather than each beside the object it configures because pinning images is one
// decision taken once: an air-gapped installation overrides all three against its
// own registry, and a reviewer reading the document has one place to check what
// this deployment will run. The expansion is what spends them on two different
// objects, since the three fields they land on are not all on the same kind.
//
// The control plane's own image is not here. A ClusterDeploymentConfig creates a
// StorageCluster and its StorageNodes and neither creates nor adopts the
// ControlPlane, so a slot for it would be a field the expansion has nowhere to
// write. It is ControlPlane.spec.source.local.image, and the CSI driver's is
// SimplyblockDriver.spec.image.
type DeploymentImages struct {
	// NodeAgent is the image the storage-node DaemonSet runs: the agent the
	// control plane drives a worker through, and the two init containers that
	// configure the host before it starts. The expansion writes it to
	// StorageCluster.spec.storageNodes, which is where the retired
	// StorageNodeSet.spec.clusterImage went.
	//
	// It is the same artifact the control plane itself runs, which is why
	// spec.storageNodes.image defaults to the ControlPlane singleton's: the agent
	// and the tasks that call it are one codebase, and version skew between them
	// is what breaks a node add.
	//
	// It is spent only where the document creates the cluster. A document that
	// names an existing one in ClusterRef adds nodes to a DaemonSet that is
	// already running under an image the cluster states, and this slot is ignored
	// the same way Cluster is.
	// +optional
	NodeAgent *ImageSpec `json:"nodeAgent,omitempty"`

	// SPDK is the SPDK image, which the expansion writes onto every
	// StorageNode.spec.config it creates rather than onto the cluster: the field
	// is per node so that a later rollout can walk the fleet one machine at a
	// time, and a document states the fleet's starting point.
	// +optional
	SPDK *ImageSpec `json:"spdk,omitempty"`

	// SPDKProxy is the SPDK proxy image, written per node for the reason SPDK is.
	// +optional
	SPDKProxy *ImageSpec `json:"spdkProxy,omitempty"`
}

// ClusterDeploymentConfigSpec is a whole simplyblock deployment as one
// reviewable document.
//
// The third rule is the device class one. A document describes one cluster and a
// cluster is built out of one class of backend storage, so every group of every
// node set names the same member of its DeviceSelection. The expansion reads the
// class off them and stamps it onto the cluster it creates, which is why the
// document carries no field for it.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || self == oldSelf",message="an approved deployment config is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || self.approved",message="approval cannot be withdrawn"
// +kubebuilder:validation:XValidation:rule="self.nodeSets.all(s, s.groups.all(g, !has(g.devices) || !has(g.devices.block))) || self.nodeSets.all(s, s.groups.all(g, !has(g.devices) || !has(g.devices.nvme)))",message="every group must name the same device class: all nvme or all block"
type ClusterDeploymentConfigSpec struct {
	// Approved is the review gate. A document is expanded only once it is set,
	// and is validated but otherwise inert before that, which is what makes
	// reviewing a wrong document safe.
	//
	// It is defaulted and serialized rather than omitted when false, and the
	// two are the same requirement read twice. A reviewer has to see the gate
	// they are being asked to open, and the rules above have to find the field
	// they read: a bool omitted when false is a key the apiserver never stores,
	// so a rule reading it fails rather than reading false, and the first rule
	// guarding approval denied every approval there could ever be. The has()
	// guards are what carry documents written before the default existed.
	// +optional
	// +kubebuilder:default=false
	Approved bool `json:"approved"`

	// Environment is the Kubernetes distribution this deployment targets. It is a
	// shorthand the expansion spends: it sets enableKubeletConfiguration,
	// enableCpuTopology, and openShiftCluster on the cluster the document
	// produces, after which nothing reads it again. The worker's host OS is not
	// among them and is stated in hostOS, because a distribution decides what
	// Kubernetes does to a machine and not which packages the machine has.
	// +optional
	Environment KubernetesEnvironment `json:"environment,omitempty"`

	// HostOS is the operating system the workers run, which decides what the
	// host itself offers rather than what Kubernetes does to it. The expansion
	// spends the distro on StorageCluster.spec.storageNodes.ubuntuHost and
	// carries the family for the reviewer reading the document.
	// +optional
	HostOS *HostOSSpec `json:"hostOS,omitempty"`

	// EdgeCluster states that this is an edge deployment. An edge deployment
	// differs from a datacenter one in topology and scale rather than in kind,
	// so it is the same StorageCluster with fewer, smaller nodes.
	// +optional
	EdgeCluster *bool `json:"edgeCluster,omitempty"`

	// ClusterRef names an existing StorageCluster this document adds nodes to.
	// Absent means the document creates the cluster in Cluster. Setting it to a
	// cluster that does not exist, or leaving it absent when one already does,
	// is refused rather than reconciled.
	//
	// The maximum is what a StorageCluster name may be and not the 253 an object
	// name may be: a reference between the two names nothing that can exist
	// (design-api-upgrade.md §19.4).
	// +kubebuilder:validation:MaxLength=63
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`

	// Cluster is the StorageCluster to create. Ignored when ClusterRef is set.
	// +optional
	Cluster *ClusterTemplate `json:"cluster,omitempty"`

	// Images are the container images this deployment pins. Unstated, each field
	// the expansion would write keeps its own default.
	// +optional
	Images *DeploymentImages `json:"images,omitempty"`

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
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Validating','CreatingCluster','AwaitingCluster','CreatingNodes','Activating']",message="unknown step"
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

	// ExpansionStartedAt is when the expansion machine was born, which is the
	// first reconcile after the document was approved. A document may sit as a
	// draft for as long as a review takes, so this is not creationTimestamp and
	// the difference is the whole point: how long a deployment takes is measured
	// from the moment somebody said yes.
	//
	// It is the start of §9.2's expansion_duration_seconds. A histogram needs an
	// instant that survives the operator restarting mid-expansion, which nothing
	// in memory and no step deadline supplies.
	// +optional
	ExpansionStartedAt *metav1.Time `json:"expansionStartedAt,omitempty"`
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
