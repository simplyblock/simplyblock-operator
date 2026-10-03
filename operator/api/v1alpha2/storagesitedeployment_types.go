// The storage deployment of a managed site, requested from the hub.
//
// A site's storage cluster is built from objects that live on the site's API
// server: an OperatorOps discovery, the ClusterDeploymentConfig draft it
// writes, and the StorageCluster the approved draft expands into. A hub that
// manages the site through Open Cluster Management does not reach that API
// server, so this kind is the hub-side request: it names the site and the
// sizing, and a controller carries the request to the site through a
// ManifestWork and projects the site's answer back through ManagedClusterViews.
// Approval is the same one-way gate the draft has on the site; it is flipped
// here and delivered there.
//
// Deleting the object withdraws nothing on the site: the storage cluster it
// requested stays, as a storage cluster is never torn down by deleting a
// request. Specified by docs/design/control-center-managed-discovery.md of the
// simplyblock-dr repository.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageSiteDeploymentPhase is the request's own progress.
// +kubebuilder:validation:Enum=Pending;Discovering;Drafted;Deploying;Online;Failed
type StorageSiteDeploymentPhase string

const (
	// StorageSiteDeploymentPhasePending is the request before the hub delivered
	// anything to the site.
	StorageSiteDeploymentPhasePending StorageSiteDeploymentPhase = "Pending"

	// StorageSiteDeploymentPhaseDiscovering is the discovery running on the site:
	// the draft is not written yet, or names no node yet.
	StorageSiteDeploymentPhaseDiscovering StorageSiteDeploymentPhase = "Discovering"

	// StorageSiteDeploymentPhaseDrafted is a draft with nodes on the site, sized
	// as the request says, awaiting approval.
	StorageSiteDeploymentPhaseDrafted StorageSiteDeploymentPhase = "Drafted"

	// StorageSiteDeploymentPhaseDeploying is an approved draft expanding into a
	// StorageCluster that is not Online yet.
	StorageSiteDeploymentPhaseDeploying StorageSiteDeploymentPhase = "Deploying"

	// StorageSiteDeploymentPhaseOnline is the StorageCluster Online on the site.
	StorageSiteDeploymentPhaseOnline StorageSiteDeploymentPhase = "Online"

	// StorageSiteDeploymentPhaseFailed is the site's own failure: the draft or the
	// StorageCluster failed, or the hub could not deliver the request.
	StorageSiteDeploymentPhaseFailed StorageSiteDeploymentPhase = "Failed"
)

// StorageSiteDiscovery is the discovery the site runs: which nodes are
// inspected. It is the hub-side form of OperatorOps.spec.discover.
type StorageSiteDiscovery struct {
	// EnableControlPlaneNodes lets the discovery consider the nodes that run the
	// API server. Every server of a small distribution is one, so a three-node
	// site has no storage without it.
	// +optional
	EnableControlPlaneNodes *bool `json:"enableControlPlaneNodes,omitempty"`

	// Workers limits the discovery to these nodes. Empty is every worker.
	// +optional
	// +listType=set
	Workers []string `json:"workers,omitempty"`

	// NodeSelector limits the discovery to the nodes carrying these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// StorageSiteSizing is the cluster template written onto the draft before it
// is approved: the fields of ClusterDeploymentConfig.spec.cluster a reviewer
// decides. Absent fields keep what the discovery wrote.
type StorageSiteSizing struct {
	// Name is the StorageCluster's name on the site.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Name string `json:"name,omitempty"`

	// VCPUCount is the number of vCPUs each storage node takes.
	// +kubebuilder:validation:Minimum=1
	// +optional
	VCPUCount *int32 `json:"vcpuCount,omitempty"`

	// MinHugePagesSize is the hugepage memory each storage node takes, as a
	// quantity ("8G").
	// +optional
	MinHugePagesSize string `json:"minHugePagesSize,omitempty"`

	// MaxSubsystemCount is the number of NVMe-oF subsystems each node serves.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxSubsystemCount *int32 `json:"maxSubsystemCount,omitempty"`

	// EnableDriveFormat lets the deployment format the devices it takes.
	// +optional
	EnableDriveFormat *bool `json:"enableDriveFormat,omitempty"`

	// EnableJournalDevice dedicates one device per node to the journal.
	// +optional
	EnableJournalDevice *bool `json:"enableJournalDevice,omitempty"`

	// Stripe is the erasure-coding layout.
	// +optional
	Stripe *StripeSpec `json:"stripe,omitempty"`
}

// StorageSiteDeploymentSpec is the request for one site's storage cluster.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || self.approved",message="approval is one-way: an approved deployment cannot be un-approved"
type StorageSiteDeploymentSpec struct {
	// Cluster is the OCM ManagedCluster the storage is deployed on. The request's
	// ManifestWork and views live in its namespace on the hub. Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cluster is immutable"
	Cluster string `json:"cluster"`

	// SiteNamespace is the simplyblock operator's namespace on the site, where
	// the discovery and the draft live.
	// +kubebuilder:default=simplyblock
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SiteNamespace string `json:"siteNamespace,omitempty"`

	// DraftName is the ClusterDeploymentConfig the discovery writes on the site
	// and the request sizes and approves. Immutable.
	// +kubebuilder:default=site-draft
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="draftName is immutable"
	// +optional
	DraftName string `json:"draftName,omitempty"`

	// Discover is the discovery the site runs first. Changing it runs another
	// discovery, which rewrites the draft.
	// +optional
	Discover StorageSiteDiscovery `json:"discover,omitempty"`

	// Sizing is written onto the draft's cluster template once the draft exists,
	// so the reviewer sees the sized draft before approving it.
	// +optional
	Sizing *StorageSiteSizing `json:"sizing,omitempty"`

	// Approved is the review gate, delivered to the draft on the site. One-way,
	// as the draft's own gate is.
	// +kubebuilder:default=false
	// +optional
	Approved bool `json:"approved"`
}

// StorageSiteDraft is the draft as the site reports it.
type StorageSiteDraft struct {
	// Name is the ClusterDeploymentConfig on the site.
	Name string `json:"name"`

	// Phase is the draft's own phase on the site (Draft, Expanding, Expanded,
	// Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message is what the site says about the draft: validation findings while
	// it is a draft, the expansion's step afterwards.
	// +optional
	Message string `json:"message,omitempty"`

	// Approved is whether the draft is approved on the site.
	// +optional
	Approved bool `json:"approved,omitempty"`

	// Cluster is the draft's cluster template, with the sizing applied.
	// +optional
	Cluster *ClusterTemplate `json:"cluster,omitempty"`

	// NodeSets are the nodes and devices the discovery found, for review.
	// +optional
	NodeSets []NodeSet `json:"nodeSets,omitempty"`

	// NodeRefs are the StorageNode objects the expansion created.
	// +optional
	// +listType=set
	NodeRefs []string `json:"nodeRefs,omitempty"`
}

// StorageSiteNode is one storage node of the deployed cluster, as the site
// reports it.
type StorageSiteNode struct {
	// Name is the StorageNode object on the site.
	Name string `json:"name"`

	// Phase is the node's phase on the site.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Hostname is the Kubernetes node it runs on.
	// +optional
	Hostname string `json:"hostname,omitempty"`
}

// StorageSiteCluster is the StorageCluster the approved draft produced.
type StorageSiteCluster struct {
	// Name is the StorageCluster object on the site.
	Name string `json:"name"`

	// UUID is the storage cluster's id in the control plane, which a
	// StorageClass names in cluster_id.
	// +optional
	UUID string `json:"uuid,omitempty"`

	// Phase is the StorageCluster's phase on the site.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Pool is the pool the cluster was created with, which a StorageClass names
	// in pool_name.
	// +optional
	Pool string `json:"pool,omitempty"`

	// Nodes are the cluster's storage nodes.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []StorageSiteNode `json:"nodes,omitempty"`
}

// StorageSiteDeploymentStatus is what the site reports back, projected.
type StorageSiteDeploymentStatus struct {
	// Phase is the request's own progress.
	// +optional
	Phase StorageSiteDeploymentPhase `json:"phase,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the request moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// WorkName is the ManifestWork carrying the request to the site.
	// +optional
	WorkName string `json:"workName,omitempty"`

	// Draft is the draft as the site reports it.
	// +optional
	Draft *StorageSiteDraft `json:"draft,omitempty"`

	// StorageCluster is the cluster the approved draft produced.
	// +optional
	StorageCluster *StorageSiteCluster `json:"storageCluster,omitempty"`

	// Conditions: Delivered (the work is applied on the site), Discovered (the
	// draft names nodes), Approved (the site's draft is approved), Ready (the
	// StorageCluster is Online).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbsd
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.cluster"
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Draft",type=string,JSONPath=".status.draft.phase"
// +kubebuilder:printcolumn:name="Storage",type=string,JSONPath=".status.storageCluster.phase"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageSiteDeployment requests a managed site's storage cluster from the hub:
// a discovery on the site, the sizing of the draft it writes, and the approval
// that expands the draft into a StorageCluster. The hub carries the request
// through OCM and projects the site's draft and cluster into the status.
// Deleting the request leaves the storage cluster alone.
type StorageSiteDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageSiteDeploymentSpec   `json:"spec,omitempty"`
	Status StorageSiteDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageSiteDeploymentList contains a list of StorageSiteDeployment.
type StorageSiteDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageSiteDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageSiteDeployment{}, &StorageSiteDeploymentList{})
}
