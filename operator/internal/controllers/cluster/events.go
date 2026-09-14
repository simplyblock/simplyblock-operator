// The reasons this package emits events under.
//
// They are collected here rather than declared beside the code that raises them
// because a reason is a contract with whoever is reading `kubectl describe`: it
// is what an administrator greps for and what an alert matches on, so it
// outlives the function that happens to raise it today.
//
// Events need a target object and both kinds are their own. An event about the
// cluster's own reconcile goes on the StorageCluster, which is what an
// administrator has open. An event about an operation goes on the
// StorageClusterOps, which outlives the operation as its record.
//
// design-storagecluster.md §10.1 is the specification.

package cluster

const (
	// FDBNotReady says the control plane is not ready to accept a creation.
	FDBNotReady = "FDBNotReady"
	// BackupCredentialsError says the Secret spec.backup names cannot be
	// resolved: missing, unreadable, or lacking the two keys.
	BackupCredentialsError = "BackupCredentialsError"
	// InvalidConfig says a user-supplied field failed validation, which today
	// means a URL that does not resolve to an external address.
	InvalidConfig = "InvalidConfig"
	// ClusterCreationFailed carries the control plane's own refusal, status and
	// body, so the cause is visible in `kubectl describe` without reading the
	// operator's log.
	ClusterCreationFailed = "ClusterCreationFailed"
	// ClusterAdopted says an existing backend cluster was taken over rather
	// than created, which is the difference between a migration and a mistake.
	ClusterAdopted = "ClusterAdopted"

	// TaskCompleted and TaskCanceled are what remains of a control-plane task
	// once it leaves status.tasks. The list is a window on the present and
	// these are the record that something happened in it.
	TaskCompleted = "TaskCompleted"
	TaskCanceled  = "TaskCanceled"

	// The operation reasons, raised on the StorageClusterOps rather than the
	// cluster.
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

	// PeerNodeNotOnline is the load-bearing one. Holding a rolling restart for
	// a peer is correct behavior, and without an event it is indistinguishable
	// from a stalled controller.
	PeerNodeNotOnline = "PeerNodeNotOnline"

	// NodeRestarted says the walk advanced to the next node.
	NodeRestarted = "NodeRestarted"

	// FailureDomainNotReady says an activation is waiting because the cluster's
	// failure domains do not yet hold an equal number of hosts.
	FailureDomainNotReady = "FailureDomainNotReady"
)
