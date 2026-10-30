// The stretched cluster, requested from the hub.
//
// A stretched cluster is one synchronously replicated backend cluster whose two
// sites are two Kubernetes clusters, both managed from one Open Cluster
// Management hub that also runs the control plane. Each site runs the operator
// as a standalone installation; the backend cluster is created by the site that
// leads and adopted by the site that follows. This kind is the hub-side request
// that names the two managed clusters, the site each represents, and which one
// leads. A controller on the hub carries it to both sites as two
// StorageSiteDeployment-shaped flows, one ManifestWork each, and projects both
// sites' answers back through ManagedClusterViews.
//
// Deleting the object withdraws nothing on the sites, as a StorageSiteDeployment
// does not. Specified by docs/designs/design-stretched-sync-replication-ramen.md.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StretchedRole is which of a stretched cluster's two sites owns the backend
// cluster's cluster-wide operations.
// +kubebuilder:validation:Enum=Leader;Follower
type StretchedRole string

const (
	// StretchedRoleLeader is the site whose operator creates the backend
	// cluster, activates it, and runs every cluster-wide operation.
	StretchedRoleLeader StretchedRole = "Leader"
	// StretchedRoleFollower is the site whose operator adopts the cluster the
	// leader created and manages only its own site's nodes.
	StretchedRoleFollower StretchedRole = "Follower"
)

// StretchedClusterPhase is the request's own progress across both sites.
// +kubebuilder:validation:Enum=Pending;Discovering;Drafted;LeaderCreating;FollowerJoining;Ready;Degraded;Failed
type StretchedClusterPhase string

const (
	// StretchedClusterPhasePending is a request no site has answered yet.
	StretchedClusterPhasePending StretchedClusterPhase = "Pending"
	// StretchedClusterPhaseDiscovering is at least one site still writing its
	// draft.
	StretchedClusterPhaseDiscovering StretchedClusterPhase = "Discovering"
	// StretchedClusterPhaseDrafted is both drafts written and awaiting approval.
	StretchedClusterPhaseDrafted StretchedClusterPhase = "Drafted"
	// StretchedClusterPhaseLeaderCreating is the approved leader's draft
	// expanding into the backend cluster.
	StretchedClusterPhaseLeaderCreating StretchedClusterPhase = "LeaderCreating"
	// StretchedClusterPhaseFollowerJoining is the backend cluster existing and
	// the follower adopting it and adding its nodes.
	StretchedClusterPhaseFollowerJoining StretchedClusterPhase = "FollowerJoining"
	// StretchedClusterPhaseReady is both sites' StorageClusters Online on the
	// same backend cluster.
	StretchedClusterPhaseReady StretchedClusterPhase = "Ready"
	// StretchedClusterPhaseDegraded is a ready cluster one of whose sites is
	// unreachable from the hub, recorded out by the control plane, or not
	// Online.
	StretchedClusterPhaseDegraded StretchedClusterPhase = "Degraded"
	// StretchedClusterPhaseFailed is a site that refused what the hub
	// delivered, or a draft that failed.
	StretchedClusterPhaseFailed StretchedClusterPhase = "Failed"
)

// StretchedSite is one of the two sites: a managed cluster, the backend site
// its nodes join, and its role.
type StretchedSite struct {
	// Cluster is the OCM ManagedCluster hosting this site. Its namespace on the
	// hub holds the site's ManifestWork and views. It identifies the entry, so
	// it cannot change.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Cluster string `json:"cluster"`

	// Site is the backend site name every storage node of this managed cluster
	// joins with. Immutable: it is a property of the installation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	// +k8s:immutable
	Site string `json:"site"`

	// Role is Leader or Follower. It may be swapped between the two sites after
	// creation, as one change to both entries, once both sites' clusters are
	// Ready.
	// +kubebuilder:validation:Required
	Role StretchedRole `json:"role"`

	// SiteNamespace is the simplyblock operator's namespace on the site, where
	// the discovery, the draft, and the StorageCluster live.
	// +kubebuilder:default=simplyblock
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SiteNamespace string `json:"siteNamespace,omitempty"`

	// Discover is the discovery this site runs: which workers are inspected.
	// Changing it before approval runs another discovery on this site only,
	// which rewrites this site's draft; after approval it is refused.
	// +optional
	Discover StorageSiteDiscovery `json:"discover,omitempty"`
}

// StretchedClusterSpec is the request for one stretched cluster.
// +kubebuilder:validation:XValidation:rule="self.sites.exists_one(s, s.role == 'Leader')",message="exactly one site must have role Leader"
// +kubebuilder:validation:XValidation:rule="self.sites[0].site != self.sites[1].site",message="the two sites must name different backend sites"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sites) || oldSelf.sites.all(o, self.sites.exists(s, s.cluster == o.cluster))",message="the managed clusters of a stretched cluster cannot be replaced"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || self.approved",message="approval is one-way: an approved stretched cluster cannot be un-approved"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approved) || !oldSelf.approved || oldSelf.sites.all(o, self.sites.exists(s, s.cluster == o.cluster && has(s.discover) == has(o.discover) && (!has(s.discover) || s.discover == o.discover)))",message="discover cannot change once the stretched cluster is approved"
type StretchedClusterSpec struct {
	// ClusterName is the backend cluster's name, and the name of the
	// StorageCluster on both sites. Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +k8s:immutable
	ClusterName string `json:"clusterName"`

	// Sites are the two managed clusters and the site each represents. Exactly
	// one has role Leader.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	// +listType=map
	// +listMapKey=cluster
	Sites []StretchedSite `json:"sites"`

	// Sizing is written onto both sites' drafts once they name nodes: the
	// fields no discovery can read. Synchronous replication is enabled on both
	// drafts whatever is written here.
	// +optional
	Sizing *StorageSiteSizing `json:"sizing,omitempty"`

	// Images are the images both sites' storage nodes run, written onto both
	// drafts with the sizing. A stretched cluster's two sites run one data plane,
	// so the images are stated once for both. Unstated, each site's expansion
	// keeps its own defaults.
	// +optional
	Images *DeploymentImages `json:"images,omitempty"`

	// Approved is the review gate for both drafts. One-way. The leader's draft
	// is approved as soon as this is set; the follower's only once the leader's
	// cluster exists, so the follower never races the create.
	// +kubebuilder:default=false
	// +optional
	Approved bool `json:"approved"`
}

// StretchedClusterSiteStatus is what one site reports back, projected.
type StretchedClusterSiteStatus struct {
	// Cluster is the managed cluster this entry reports.
	Cluster string `json:"cluster"`

	// Site is the backend site of this managed cluster.
	// +optional
	Site string `json:"site,omitempty"`

	// Role is the role last delivered to this site.
	// +optional
	Role StretchedRole `json:"role,omitempty"`

	// Phase is this site's own progress, in the phases a StorageSiteDeployment
	// reports for one site.
	// +optional
	Phase StorageSiteDeploymentPhase `json:"phase,omitempty"`

	// Message is the reason this site's phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// WorkName is the ManifestWork carrying the request to this site.
	// +optional
	WorkName string `json:"workName,omitempty"`

	// Reachable reports whether the hub's last read of the managed cluster found
	// it available. An unreachable API server is not a lost site: the control
	// plane judges that from the storage nodes, not from the hub's connection.
	// +optional
	Reachable *bool `json:"reachable,omitempty"`

	// SiteRecord is the control plane's record of this site, as this site's
	// StorageCluster reports it: empty while the site is in service, otherwise
	// the state of its drain or fence.
	// +optional
	SiteRecord string `json:"siteRecord,omitempty"`

	// Draft is the site's draft as the site reports it.
	// +optional
	Draft *StorageSiteDraft `json:"draft,omitempty"`

	// StorageCluster is the StorageCluster the site's approved draft produced.
	// +optional
	StorageCluster *StorageSiteCluster `json:"storageCluster,omitempty"`
}

// StretchedClusterStatus is what both sites report back, projected.
type StretchedClusterStatus struct {
	// Phase is the request's own progress across both sites.
	// +optional
	Phase StretchedClusterPhase `json:"phase,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the request moves.
	// +optional
	Message string `json:"message,omitempty"`

	// ClusterUUID is the backend cluster, once the leader's StorageCluster
	// reports it.
	// +optional
	ClusterUUID string `json:"clusterUUID,omitempty"`

	// LostSite is the site the control plane records as out of service, as a
	// surviving site's StorageCluster reports it. Empty while both sites serve.
	// The roles can be swapped away from a lost leader once its fence is done.
	// +optional
	LostSite string `json:"lostSite,omitempty"`

	// Sites reports each site, keyed by its managed cluster.
	// +optional
	// +kubebuilder:validation:MaxItems=2
	// +listType=map
	// +listMapKey=cluster
	Sites []StretchedClusterSiteStatus `json:"sites,omitempty"`

	// Conditions: Delivered (both works are applied), Drafted (both drafts name
	// nodes), Approved (both drafts are approved), Ready (both StorageClusters
	// are Online on one backend cluster).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbstr
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterName"
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="UUID",type=string,JSONPath=".status.clusterUUID",priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StretchedCluster requests one synchronously replicated storage cluster whose
// two sites are two OCM managed clusters: a discovery and a draft on each site,
// the sizing of both drafts, and the approval that creates the cluster on the
// leading site and joins it from the following one. The hub carries the request
// through OCM and projects both sites into the status. Deleting the request
// leaves both sites' storage alone.
type StretchedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StretchedClusterSpec   `json:"spec,omitempty"`
	Status StretchedClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StretchedClusterList contains a list of StretchedCluster.
type StretchedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StretchedCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StretchedCluster{}, &StretchedClusterList{})
}
