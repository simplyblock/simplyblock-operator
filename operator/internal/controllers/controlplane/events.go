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
	// It is emitted on transition rather than on every probe, which bounds one
	// outage to one event rather than one every thirty seconds.
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
	// InstallationFailed is an installation step that cannot finish: the index
	// backfill Job has used up its attempts. The install stops rather than
	// retrying on an interval, and this is the event that says so.
	InstallationFailed = "InstallationFailed"

	// ClustersStillPresent holds a deletion. It is a hold rather than a failure:
	// removing the clusters resolves it, and nothing else can.
	ClustersStillPresent = "ClustersStillPresent"

	// EndpointUnreachable is a remote control plane that could not be
	// resolved or reached.
	EndpointUnreachable = "EndpointUnreachable"

	// CredentialsError is a remote control plane whose Secret is missing or
	// carries no token. It is separate from EndpointUnreachable because the two
	// have different fixes.
	CredentialsError = "CredentialsError"

	// PrerequisiteMissing is an install held on something the cluster has to
	// provide and does not, such as the FoundationDB CRDs.
	PrerequisiteMissing = "PrerequisiteMissing"

	// PrerequisiteOutdated is an add-on the operator installed from an older
	// manifest whose upgrade it leaves to a person.
	PrerequisiteOutdated = "PrerequisiteOutdated"

	// DuplicateControlPlane is a second ControlPlane in a Kubernetes cluster that
	// already has one. It installs nothing, because the objects it would apply
	// are the same fixed-name cluster-scoped objects the first one owns.
	DuplicateControlPlane = "DuplicateControlPlane"
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
)

// Reasons a HostMaintenance window emits (design-fdb-host-maintenance.md §11).
// They land on the ControlPlaneOps, which outlives the window as its audit
// record, except HostMaintenanceRaised, which lands on the ControlPlane because
// it is the object that exists before the window does.
const (
	// HostMaintenanceRaised is a cordoned worker that hosts FoundationDB pods
	// and got a window for it.
	HostMaintenanceRaised = "HostMaintenanceRaised"
	// EvictionHeld is the budget up over the worker's FoundationDB pods.
	EvictionHeld = "EvictionHeld"
	// DrainObserved is a drain of the worker seen, and the message names the
	// signal that saw it. Its timestamps across the windows of a pool are the
	// order the pool was drained in.
	DrainObserved = "DrainObserved"
	// MoveQueued is a window whose drain arrived while another window's move
	// holds the control plane. It waits with its budget up, for the seconds the
	// other move takes.
	MoveQueued = "MoveQueued"
	// MaintenanceModeSet and MaintenanceModeNotSet are the two outcomes of
	// asking FoundationDB to leave the worker's zone alone while it is away.
	MaintenanceModeSet    = "MaintenanceModeSet"
	MaintenanceModeNotSet = "MaintenanceModeNotSet"
	// MaintenanceZoneTakenOver is a later window setting maintenance mode for
	// its zone, which ended this window's: FoundationDB holds one zone.
	MaintenanceZoneTakenOver = "MaintenanceZoneTakenOver"
	// CoordinatorsMoved and CoordinatorsNotMoved are the two outcomes of the
	// move. The first names the set and whether two coordinators share a host,
	// which is a cluster too small to do otherwise. The second names the counts
	// or the failed command.
	CoordinatorsMoved    = "CoordinatorsMoved"
	CoordinatorsNotMoved = "CoordinatorsNotMoved"
	// ProcessesHandedOff and ProcessesNotHandedOff are the two outcomes of the
	// exclusion that moves the worker's log and stateless roles while the
	// processes are alive (design-fdb-host-maintenance.md §5.12). The second
	// is the eviction proceeding with the roles in place, which costs the
	// recovery the hand-off exists to spare.
	ProcessesHandedOff    = "ProcessesHandedOff"
	ProcessesNotHandedOff = "ProcessesNotHandedOff"
	// ProcessesIncluded and ProcessesLeftExcluded are the two outcomes of the
	// include that lifts that exclusion once the processes are back. The
	// second carries the command to run by hand, because a process left
	// excluded runs no role.
	ProcessesIncluded     = "ProcessesIncluded"
	ProcessesLeftExcluded = "ProcessesLeftExcluded"
	// CoordinatorsReverted is the cluster file still naming the host after a
	// completed move, which is the FoundationDB operator behaving differently
	// from what the design verified. It is the event to look for after an
	// upgrade of that operator.
	CoordinatorsReverted = "CoordinatorsReverted"
	// EvictionReleased is the budget gone, and the drain proceeding.
	EvictionReleased = "EvictionReleased"
	// AwaitingHostReturn is the window waiting for the worker's process groups
	// to report back, wherever they come back.
	AwaitingHostReturn = "AwaitingHostReturn"
	// MaintenanceModeCleared and MaintenanceModeLeft are the two outcomes of
	// clearing the zone. A zone left expires on its own.
	MaintenanceModeCleared = "MaintenanceModeCleared"
	MaintenanceModeLeft    = "MaintenanceModeLeft"
	// MaintenanceMarkersLeft is a terminal teardown that could not remove the
	// budget or a label, which is the one event a worker that will not drain
	// is explained by.
	MaintenanceMarkersLeft = "MaintenanceMarkersLeft"
)
