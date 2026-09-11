// The reasons this package emits events under.
//
// They are collected here rather than declared beside the code that emits them
// because a reason is a contract with whoever is reading `kubectl describe`: it
// is what an administrator greps for and what an alert matches on, so it outlives
// the function that happens to raise it today. Two of them are raised from two
// places, which is the other reason one list beats a constant per call site.
//
// design-storagepool.md §9.1 is the specification.

package pool

const (
	// PoolCreated says the pool now exists in the control plane.
	PoolCreated = "PoolCreated"
	// PoolCreationFailed carries the control plane's own refusal, body and all,
	// so the cause is visible without reading the operator's log.
	PoolCreationFailed = "PoolCreationFailed"
	// ClusterNotReady says the pool is admitted and waiting, which is the
	// ordinary state of a pool applied in the same manifest as its cluster.
	ClusterNotReady = "ClusterNotReady"

	// StorageClassAssigned says a class now names this pool.
	StorageClassAssigned = "StorageClassAssigned"
	// StorageClassCreated says the operator wrote the class for a default pool.
	StorageClassCreated = "StorageClassCreated"
	// StorageClassNameTaken says the name the default pool's class would have
	// had belongs to somebody else's class. A StorageClass is cluster-scoped, so
	// this is reachable without anybody touching this namespace, and the pool is
	// left without a default class rather than credited with one that
	// provisions elsewhere.
	StorageClassNameTaken = "StorageClassNameTaken"
	// QoSParameterConflict says an assigned class states one ceiling under two
	// spellings. The pool controller raises it when it indexes the class, which
	// is usually well before anybody's claim reaches it, and the CSI driver
	// raises the same reason on the claim it is provisioning. Neither is
	// redundant: the two audiences are different, and the class itself,
	// cluster-scoped and shared, is a poor place for either to look.
	QoSParameterConflict = "QoSParameterConflict"

	// AllowedNodeMissing says an entry in spec.allowedNodes resolves to no
	// StorageNode. It is raised once per name rather than every pass, because
	// the authored list is deliberately left as written and repeating the event
	// would say nothing new.
	AllowedNodeMissing = "AllowedNodeMissing"

	// StorageClassStillAssigned and VolumesStillBound are the two that explain a
	// `kubectl delete` which does not finish. They are separate reasons rather
	// than one because the remedies differ: a class is deleted by whoever wrote
	// it, and a bound volume is released by deleting a claim. Reporting "still
	// referenced" without saying which would leave an administrator guessing
	// between the two — and correct behavior here looks exactly like a stuck
	// finalizer, which is the failure mode people reach for --force over.
	StorageClassStillAssigned = "StorageClassStillAssigned"
	VolumesStillBound         = "VolumesStillBound"

	// PoolDeletionFailed says the control plane refused to delete the pool,
	// which is what covers the volumes Kubernetes cannot see.
	PoolDeletionFailed = "PoolDeletionFailed"
	// CapacityExhausted says the pool has reached its capacity limit.
	CapacityExhausted = "CapacityExhausted"

	// The operation reasons, raised on the StoragePoolOps rather than the pool.
	OperationQueued       = "OperationQueued"
	OperationStarted      = "OperationStarted"
	OperationSucceeded    = "OperationSucceeded"
	OperationFailed       = "OperationFailed"
	OperationAborted      = "OperationAborted"
	StepDeadlineExceeded  = "StepDeadlineExceeded"
	OperationTargetFailed = "OperationTargetFailed"
)
