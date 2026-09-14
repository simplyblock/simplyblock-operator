// The events the data-protection band emits, in one place because the object an
// event goes on is a decision about the whole band rather than about one
// controller.
//
// The rule is that an event goes where somebody is already looking. An event
// about a policy goes on the StorageBackupPolicy, which is what an administrator
// managing data protection has open. An event about a restore goes on the
// StorageBackupOps, which is the audit record. An event about a backup goes on
// the StorageBackup, except when the object is the thing disappearing: an event
// on a disappearing object is an event nobody reads, so a pruned backup is
// reported on the cluster and on the policy instead.
//
// design-storagebackup.md §11.1 is the specification.

package backup

// The six reasons every Ops kind in the group carries, which is what makes a
// dashboard, an alert, and a runbook writable once against the category rather
// than once per kind (design-crd-model.md §3.3).
const (
	// ReasonOperationQueued reports the phase's second meaning rather than the
	// phase. Pending is where an operation starts, so an operation nobody has
	// reconciled yet and one waiting on a lock it cannot take are the same value
	// in status.phase, and this event is the only thing that separates them.
	ReasonOperationQueued = "OperationQueued"

	ReasonOperationStarted   = "OperationStarted"
	ReasonOperationSucceeded = "OperationSucceeded"
	ReasonOperationFailed    = "OperationFailed"

	// ReasonOperationAborted cannot fire from the last two steps of a restore,
	// because the graph declares no abort edge there. It is declared anyway, so
	// that a reason appearing when an action gains an edge is one every consumer
	// already handles.
	ReasonOperationAborted = "OperationAborted"

	ReasonStepDeadlineExceeded = "StepDeadlineExceeded"
)

// The band's own reasons, which say what happened to a backup rather than what
// happened to an operation.
const (
	// ReasonClaimAttached and ReasonClaimDetached record the policy's membership
	// changing. Detaching does not delete the backups already taken, which is why
	// the two are separate from anything about retention.
	ReasonClaimAttached = "ClaimAttached"
	ReasonClaimDetached = "ClaimDetached"

	// ReasonClaimNotEligible is a claim the selector matches whose volume is not
	// backed by this cluster. It is a warning rather than a failure of the
	// policy: the other claims are still covered.
	ReasonClaimNotEligible = "ClaimNotEligible"

	// ReasonSelectorEmpty exists because of the policy's default. A policy with
	// no selector covers nothing, which is the safe reading and is also
	// indistinguishable from a policy that is working, so this event is what
	// tells an author their policy is inert.
	ReasonSelectorEmpty = "SelectorEmpty"

	ReasonBackupAvailable = "BackupAvailable"

	// ReasonBackupDiscovered is the only way a StorageBackup object comes into
	// existence, so it is also the audit record of the mirror having done so.
	ReasonBackupDiscovered = "BackupDiscovered"

	ReasonBackupFailed        = "BackupFailed"
	ReasonBackupTargetMissing = "BackupTargetMissing"

	// ReasonStoreUnreachable and ReasonBackupGone go on the StorageCluster. The
	// first is about the store rather than about any one backup, and the second
	// is about an object that is being deleted as the event is written.
	ReasonStoreUnreachable = "StoreUnreachable"
	ReasonBackupGone       = "BackupGone"

	// ReasonBackupPruned goes on the policy rather than on the backup, because
	// the backup object is being deleted at that moment.
	ReasonBackupPruned = "BackupPruned"

	// ReasonClaimExists is the refusal that stands between a restore and
	// overwriting a running workload's data.
	ReasonClaimExists = "ClaimExists"

	ReasonPoolNotFound = "PoolNotFound"
)
