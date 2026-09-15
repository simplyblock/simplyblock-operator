// The event reasons this package emits.
//
// Events land on the object an administrator has open. For the entity that is
// the ControlPlane, which is a singleton, and for an operation it is the
// ControlPlaneOps, which outlives the operation as its audit record.
//
// design-controlplane.md §9.1 is the specification.

package controlplane

// Reasons emitted on the ControlPlane.
const (
	// ControlPlaneNotReady is the load-bearing one, because every controller in
	// the operator holds when it fires and none of them says why. A cluster that
	// will not create, a node that will not add, and a volume that will not
	// provision are one event on one object.
	//
	// It is emitted on transition rather than on every probe. A thirty-second
	// probe that emitted on every failure would produce two thousand events a
	// day from one outage.
	ControlPlaneNotReady = "ControlPlaneNotReady"

	// ControlPlaneDegraded fires while every request is still being served, so
	// it reaches an administrator before an outage rather than during one. It
	// names the component and its two counts, because one reason covering eight
	// workloads sends a reader to status.components anyway.
	ControlPlaneDegraded = "ControlPlaneDegraded"

	// ControlPlaneReady is the recovery, and it is what tells a reader that an
	// earlier ControlPlaneNotReady is over.
	ControlPlaneReady = "ControlPlaneReady"

	// AwaitingDependency is an installation step waiting on something outside
	// the operator: FoundationDB reaching quorum, or the management API starting.
	AwaitingDependency = "AwaitingDependency"

	// StepDeadlineExceeded is an installation step that outlived its budget,
	// which is how a wait by design is separated from a wait caused by a bug.
	StepDeadlineExceeded = "StepDeadlineExceeded"

	// ClustersStillPresent holds a deletion. It is a hold rather than a failure:
	// removing the clusters resolves it, and nothing else can.
	ClustersStillPresent = "ClustersStillPresent"

	// EndpointUnreachable is an external control plane that could not be
	// resolved or reached.
	EndpointUnreachable = "EndpointUnreachable"

	// CredentialsError is an external control plane whose Secret is missing or
	// carries no token. It is separate from EndpointUnreachable because the two
	// have different fixes.
	CredentialsError = "CredentialsError"

	// PrerequisiteMissing is an install held on something the cluster has to
	// provide and does not, such as the FoundationDB CRDs.
	PrerequisiteMissing = "PrerequisiteMissing"
)

// Reasons emitted on a ControlPlaneOps.
const (
	// BackupRequested is a Backup run that created a FoundationDBBackup.
	BackupRequested = "BackupRequested"

	// BackupTriggered is a Backup run that found one already configured and
	// asked it for a snapshot instead of applying a second beside it.
	BackupTriggered = "BackupTriggered"

	// OperationQueued is an operation waiting for another to release the lock.
	OperationQueued = "OperationQueued"

	// OperationsInFlight is an operation holding for other operations in the
	// namespace to finish. It does not cancel them, because an operation
	// canceled to make a restart convenient is a worse outcome than a restart
	// that waited.
	OperationsInFlight = "OperationsInFlight"

	// OperationStarted is an operation that acquired the lock.
	OperationStarted = "OperationStarted"

	// OperationSucceeded and OperationFailed are the two terminal outcomes an
	// operation that ran reaches.
	OperationSucceeded = "OperationSucceeded"
	OperationFailed    = "OperationFailed"

	// OperationAborted is an operation stopped on request whose unwind finished.
	OperationAborted = "OperationAborted"

	// VersionMismatch is an upgrade whose Verifying step found the control plane
	// reporting a version other than the one asked for, which is the difference
	// between an upgrade that completed and a rollout that failed back.
	VersionMismatch = "VersionMismatch"
)
