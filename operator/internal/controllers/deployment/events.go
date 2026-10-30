// The reasons this package emits events under.
//
// They are collected here rather than declared beside the code that raises them
// because a reason is a contract with whoever is reading `kubectl describe`: it is
// what an administrator greps for and what an alert matches on, so it outlives the
// function that happens to raise it today.
//
// Two kinds carry them. A document's own validation and expansion go on the
// ClusterDeploymentConfig, which is what a reviewer has open; a discovery run's go
// on the OperatorOps, which outlives the run as its record.
//
// design-clusterdeploymentconfig.md §9.1 is the specification.

package deployment

const (
	// What a draft's validation found. These are the whole value of the review
	// gate: a document that names a worker which does not exist should say so
	// while it is still a draft, rather than after somebody approved it.
	WorkerNotFound = "WorkerNotFound"

	// NoManagementInterface is a group that names no interface for the storage
	// nodes to bind their management address to.
	NoManagementInterface = "NoManagementInterface"

	// NetworkAttachmentNotDeployable is a group that attaches its storage nodes
	// through NetworkAttachmentDefinitions, which the expansion does not build
	// yet.
	NetworkAttachmentNotDeployable = "NetworkAttachmentNotDeployable"

	// TooManyFailureDomains is a document whose failure-domain labels, added to
	// the ones its cluster has already mapped, exceed what the mapping holds.
	TooManyFailureDomains = "TooManyFailureDomains"

	// MissingSites is a sync-replication document whose groups do not all declare
	// a site. Every node of a sync cluster needs one, so provisioning holds until
	// they are filled in.
	MissingSites = "MissingSites"
	// SiteMismatch is a node group of a stretched cluster's site whose site is
	// not that cluster's site: its nodes would join the other site's half of the
	// cluster while running on this one.
	SiteMismatch = "SiteMismatch"
	// FailureDomainLayout is a document creating a cluster in failure-domain
	// mode whose domains the control plane refuses to activate: fewer than the
	// stripe's parity plus two, or holding unequal numbers of workers.
	FailureDomainLayout = "FailureDomainLayout"

	// What the document says about erasure coding, which is the one part of a
	// deployment nothing below the operator checks: the control plane validates
	// the scheme on the cluster create and counts devices at activation, never
	// nodes, so a fleet too small for its stripe is admitted everywhere else.
	StripeUnsupported         = "StripeUnsupported"
	StripeBelowMinimumNodes   = "StripeBelowMinimumNodes"
	StripeBelowMinimumWorkers = "StripeBelowMinimumWorkers"
	StripeSingleNodeCount     = "StripeSingleNodeCount"
	StripeBelowMinimumDevices = "StripeBelowMinimumDevices"

	// AwaitingNodes is the expansion waiting for a node it created to come
	// online before it asks for the cluster to be activated.
	AwaitingNodes = "AwaitingNodes"
	// WaitingForPeerSite is a stretched cluster's leader whose own nodes are
	// online but whose other site has not yet brought up the nodes the backend
	// needs before the cluster can activate.
	WaitingForPeerSite = "WaitingForPeerSite"
	// ActivationLeftToLeader is a stretched cluster's follower whose nodes are
	// online; the activation is its leader's, so it asks for none.
	ActivationLeftToLeader = "ActivationLeftToLeader"

	// ActivationRequested is the expansion having asked, which is the last thing
	// a document does.
	ActivationRequested = "ActivationRequested"
	DeviceNotFound      = "DeviceNotFound"
	DeviceClassMismatch = "DeviceClassMismatch"

	// AwaitingApproval is the one that changes how the kind is used. A valid draft
	// nobody has approved looks identical to a controller that has not noticed it,
	// and this event is what distinguishes them. It is emitted on the transition
	// to a validated draft rather than on every reconcile.
	AwaitingApproval = "AwaitingApproval"

	// ControlPlaneNotReady holds an approved document. The expansion creates a
	// StorageCluster, and a control plane that cannot accept one would leave the
	// cluster in a state the document did not describe.
	ControlPlaneNotReady = "ControlPlaneNotReady"

	// The two refusals of §6. A config creates or adds and never reconciles a
	// difference, so a document that names a cluster it did not expect is a
	// failure with a reason rather than a merge somebody has to unpick.
	ClusterExists   = "ClusterExists"
	ClusterNotFound = "ClusterNotFound"

	// The limits a member cluster is held to (design-superclusters.md §5.2,
	// §5.3): at most 32 storage nodes, and a name that is a DNS label. Both
	// hold for a node set whether or not the document makes a supercluster.
	MemberTooLarge    = "MemberTooLarge"
	MemberNameInvalid = "MemberNameInvalid"

	// MemberSizingUnstated refuses a document that adds members to a
	// supercluster without stating their sizing in spec.cluster
	// (design-superclusters.md §5.6).
	MemberSizingUnstated = "MemberSizingUnstated"

	// The topology rules every document is held to (design-superclusters.md
	// §5.7): a member stays in one region, and with failure domains enabled
	// every member of a region holds every domain the region has.
	MemberSpansRegions         = "MemberSpansRegions"
	MemberMissingFailureDomain = "MemberMissingFailureDomain"

	// JournalDeviceRequired reports logical block devices without a dedicated
	// journal device, which the control plane refuses at every node add.
	JournalDeviceRequired = "JournalDeviceRequired"

	// What the expansion produced.
	ClusterCreated = "ClusterCreated"
	NodesCreated   = "NodesCreated"

	// StepDeadlineExceeded distinguishes an expansion still working from one that
	// stopped, which is the distinction status.message cannot express.
	StepDeadlineExceeded = "StepDeadlineExceeded"
)
