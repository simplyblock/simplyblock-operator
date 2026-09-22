// The one question this tool asks of something that is not Kubernetes.
//
// §16.4's handles name their pool by name where they were provisioned before
// the v2 API migration, and the UUID that name stands for is held by the
// control plane and nowhere else: no PersistentVolume records it, and the
// StoragePool custom resource carries the name a user chose rather than the
// identifier the backend assigned.
//
// It is an interface rather than a client because of where it is called from.
// Every other rule in this framework reads the cluster and nothing else, which
// is what lets a test substitute a fake client for the whole of it; a rule
// reaching for a control-plane client directly would be a rule that cannot be
// tested without one.

package upgrade

import "context"

// PoolResolver turns a pool's name into its UUID, within one cluster.
//
// The cluster is a parameter rather than a property of the implementation
// because a handle names its own: one installation holds several clusters, and
// two of them may each have a pool called production.
type PoolResolver interface {
	// PoolUUID returns the UUID of the named pool.
	//
	// It wraps errs.ErrNotFound for a name no pool answers to, which is a
	// finding rather than a failure: a handle naming a pool that no longer
	// exists is something a person has to look at, and §16.4 says the migration
	// reports it and does not proceed.
	PoolUUID(ctx context.Context, clusterUUID, poolName string) (string, error)
}
