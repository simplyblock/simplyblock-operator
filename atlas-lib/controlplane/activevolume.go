// The walk from a volume handle to the volume that serves its data today.
//
// It lives beside Relationship because it is nothing but a sequence of
// relationship reads, and because both consumers need it: a PersistentVolume
// keeps the handle it was provisioned with for its whole life, while every
// completed cutover or fail-over hands the data to a new volume, possibly on
// another cluster. The lookup is passed in rather than taken from a Client so a
// caller whose clusters sit behind different control planes can route each hop
// to the right one, and a caller with its own transport can use it as is.

package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// RelationshipLookup reads the replication relationship of h. It returns an
// error unwrapping to errs.ErrNotFound when h has none.
type RelationshipLookup func(ctx context.Context, h lvol.Handle) (Relationship, error)

// maxActiveVolumeHops bounds the walk. A chain grows by one hop per move and
// is never compacted, so this is a cycle guard, not a length estimate.
const maxActiveVolumeHops = 256

// ActiveVolume follows completed hand-offs from start to the volume serving
// its data now.
//
// A hop is taken while the volume is the SOURCE of its newest relationship and
// that relationship names another volume as active: the data has moved to the
// relationship's target, and the walk continues from there with the target's
// own cluster, pool, and volume, which are consistent with each other. It
// stops at a volume with no relationship, at the target end of one, or at a
// source that still serves its data (a replication that has not cut over).
//
// The hop does not stop when ActiveLvolID equals the target: a control plane
// that only knows the first hop of a chain reports exactly that, and the
// target's own relationship, read at the next step, carries the walk on. An
// empty ActiveLvolID (a backend predating the field) takes no hop at all.
func ActiveVolume(ctx context.Context, start lvol.Handle, lookup RelationshipLookup) (lvol.Handle, error) {
	h := start
	visited := map[lvol.VolumeHandle]bool{}
	for range maxActiveVolumeHops {
		if visited[h.Handle()] {
			return lvol.Handle{}, fmt.Errorf("replication chain of %s loops at %s", start, h)
		}
		visited[h.Handle()] = true

		rel, err := lookup(ctx, h)
		if errors.Is(err, errs.ErrNotFound) {
			return h, nil
		}
		if err != nil {
			return lvol.Handle{}, fmt.Errorf("replication relationship of %s: %w", h, err)
		}
		if !rel.IsSource || rel.ActiveLvolID == "" || rel.ActiveLvolID == h.VolumeID {
			return h, nil
		}
		if rel.TargetClusterID == "" || rel.TargetPoolID == "" || rel.TargetLvolID == "" {
			return lvol.Handle{}, fmt.Errorf("replication relationship of %s names no complete target "+
				"(cluster %q, pool %q, volume %q)", h, rel.TargetClusterID, rel.TargetPoolID, rel.TargetLvolID)
		}
		h = lvol.Handle{ClusterID: rel.TargetClusterID, PoolRef: rel.TargetPoolID, VolumeID: rel.TargetLvolID}
	}
	return lvol.Handle{}, fmt.Errorf("replication chain of %s did not end within %d hops", start, maxActiveVolumeHops)
}
