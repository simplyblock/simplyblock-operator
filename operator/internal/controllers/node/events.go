// The reasons this package emits events under.
//
// They are collected here rather than declared beside the code that raises them
// because a reason is a contract with whoever is reading `kubectl describe`: it
// is what an administrator greps for and what an alert matches on, so it outlives
// the function that happens to raise it today.
//
// Events need a target object and this pairing has two candidates that are both
// right for different things. An event about the node's own lifecycle goes on the
// StorageNode, which is what an administrator looking at a worker has open. An
// event about an operation goes on the StorageNodeOps, which outlives the
// operation as its audit record. An operation's events are mirrored onto its
// target node as well, because the node is where someone investigating a stuck
// cluster starts and the operation's name is not something they know yet.
//
// design-storagenode.md §13.1 is the specification.

package node

const (
	// The node's own lifecycle, raised on the StorageNode.
	//
	// The three holding reasons are the load-bearing ones. A provisioning node
	// waiting for a slot, one waiting for a fault group, and one whose worker
	// does not answer are all correct behavior that looks exactly like a stalled
	// controller, and the event is the only thing that distinguishes them.
	ClusterNotReady      = "ClusterNotReady"
	FailureDomainMissing = "FailureDomainMissing"
	HostUnreachable      = "HostUnreachable"
	AwaitingSlot         = "AwaitingSlot"

	// NodeAdopted says an existing backend node was taken over rather than
	// added, which is the difference between a migration and a mistake.
	NodeAdopted = "NodeAdopted"

	// NodeOnline says the node came up and is carrying its share.
	NodeOnline = "NodeOnline"

	// PodSchedulingFailed says the node's storage pod cannot be placed, which is
	// the one Kubernetes-side failure a node's own status cannot show.
	PodSchedulingFailed = "PodSchedulingFailed"

	// The operation reasons, raised on the StorageNodeOps rather than the node.
	//
	// OperationSucceeded is one reason for all seven actions rather than one
	// each: the action is already in spec.action and on a print column, so
	// encoding it in the reason name tells a reader nothing and gives anyone
	// alerting on completion seven reasons to match instead of one.
	OperationQueued    = "OperationQueued"
	OperationStarted   = "OperationStarted"
	OperationSucceeded = "OperationSucceeded"
	OperationFailed    = "OperationFailed"
	OperationAborted   = "OperationAborted"

	// StepDeadlineExceeded distinguishes an operation still working from one
	// that stopped, which is the distinction status.message cannot express.
	StepDeadlineExceeded = "StepDeadlineExceeded"

	// DrainBlocked is one reason for two conditions, with the volume names and
	// the resolution in the message. A pinned volume and an unmanaged one are the
	// same situation from an alerting perspective — a drain that will not proceed
	// until somebody acts — and the difference is what the message says to do.
	DrainBlocked = "DrainBlocked"

	// NoMigrationTarget says a drain has no online peer to move volumes to. It is
	// a stall rather than a failure: the condition is resolved by another node
	// coming back, and failing the operation would only mean starting it again
	// afterward.
	NoMigrationTarget = "NoMigrationTarget"

	// MigrationRetried says one volume's move failed and is being retried
	// against a fresh target.
	MigrationRetried = "MigrationRetried"

	// DrainCompleted says every volume has been migrated off the node.
	DrainCompleted = "DrainCompleted"

	// NodeResumeFailed is the one that cannot be retried away. The unwind of §8.3
	// is best-effort, so a resume that fails leaves a node suspended and out of
	// service, and this event is the only place that is visible.
	NodeResumeFailed = "NodeResumeFailed"

	// MaintenanceQueued says a maintenance window is holding for another worker,
	// which is correct behavior and looks like a stalled controller without it.
	MaintenanceQueued = "MaintenanceQueued"
)
