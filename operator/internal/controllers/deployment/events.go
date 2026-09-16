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

	// AwaitingNodes is the expansion waiting for a node it created to come
	// online before it asks for the cluster to be activated.
	AwaitingNodes = "AwaitingNodes"

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

	// What the expansion produced.
	ClusterCreated = "ClusterCreated"
	NodesCreated   = "NodesCreated"

	// StepDeadlineExceeded distinguishes an expansion still working from one that
	// stopped, which is the distinction status.message cannot express.
	StepDeadlineExceeded = "StepDeadlineExceeded"
)
