// The events a volume operation emits.
//
// They land on the PersistentVolumeOps, which is the audit record. That has one
// consequence worth writing down: an Event takes its namespace from the object
// it is about, and client-go substitutes `default` when that is empty, which
// for a cluster-scoped kind is always. So these sit in a namespace nothing else
// about this product uses. `kubectl describe pvops` finds them; a
// namespace-scoped `kubectl get events` in the operator's own namespace finds
// none of them.
//
// design-persistentvolumeops.md §8.1 is the specification.

package volume

// The six reasons every Ops kind in the group carries, which is what makes a
// dashboard, an alert, and a runbook writable once against the category rather
// than once per kind (design-crd-model.md §3.3).
const (
	// ReasonOperationQueued reports the phase's second meaning rather than the
	// phase. Pending is where an operation starts, so an operation nobody has
	// reconciled yet and one waiting on a lock it cannot take are the same
	// value in status.phase, and this event is the only thing that separates
	// them.
	ReasonOperationQueued = "OperationQueued"

	ReasonOperationStarted   = "OperationStarted"
	ReasonOperationSucceeded = "OperationSucceeded"
	ReasonOperationFailed    = "OperationFailed"
	ReasonOperationAborted   = "OperationAborted"

	ReasonStepDeadlineExceeded = "StepDeadlineExceeded"
)

// The kind's own reasons, which say what happened to the migration rather than
// what happened to the operation.
const (
	// ReasonClusterUnresolvable is a volume that cannot be addressed at all:
	// deleted, replaced, or never provisioned by this driver. Admission refuses
	// that at create, so reaching it means the volume became unaddressable
	// afterward.
	ReasonClusterUnresolvable = "ClusterUnresolvable"

	// ReasonTargetNodeNotReady and ReasonTargetNodeIsSource are facts about
	// now rather than about the request, which is why neither is an admission
	// rejection: the node a drain fans fifty migrations out to may well be
	// online by the time the fifteenth of them acquires its lock.
	ReasonTargetNodeNotReady = "TargetNodeNotReady"
	ReasonTargetNodeIsSource = "TargetNodeIsSource"

	ReasonMigrationCreated = "MigrationCreated"
	ReasonMigrationStarted = "MigrationStarted"

	// ReasonWaitingForConsumer is a pod that references one of the subsystem's
	// claims and has not started. It is waited for rather than skipped: a pod
	// that stages against the source mid-migration is stranded at cutover
	// exactly like an established one.
	ReasonWaitingForConsumer = "WaitingForConsumer"

	ReasonValidationStarted = "ValidationStarted"

	// ReasonValidationSkipped is a subsystem no host consumes, which has no
	// paths to check anywhere.
	ReasonValidationSkipped = "ValidationSkipped"

	// ReasonReleasingPaths is the cleanup an abandoned migration owes every
	// host that took part in it.
	ReasonReleasingPaths = "ReleasingPaths"

	// ReasonCleanupBlocked is a delete held open because the cleanup has not
	// finished. Leaving a visibly stuck object is the intended outcome: a path
	// connected with nothing tracking it blocks every later migration of the
	// volume, and has.
	ReasonCleanupBlocked = "CleanupBlocked"
)
