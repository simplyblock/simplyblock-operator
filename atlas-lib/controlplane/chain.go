// The walk from a volume handle to the volume serving its data. A handle is
// immutable once a PersistentVolume carries it, and a fail-over or a move
// leaves it naming a volume that may be gone: the control plane keeps the
// relationship record after the source is deleted, and following the records
// from source to target, hop by hop, arrives at the active volume.
//
// The CSI driver follows the chain for every call on such a handle, and the
// operator asserts after a move that the handle resolves to the target. Both
// walk it here, so the two cannot disagree on where a volume is.

package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// Relationship states the control plane reports.
const (
	RelationshipReplicating    = "replicating"
	RelationshipCutoverPending = "cutover_pending"
	RelationshipCutoverDone    = "cutover_done"
	RelationshipFailedOver     = "failed_over"
)

// RelationshipReader reads a volume's replication relationship. *Client is
// one; a caller that reaches the control plane through an interface of its
// own, to test against a fake, walks the chain with that.
type RelationshipReader interface {
	GetVolumeReplicationRelationship(ctx context.Context, h lvol.VolumeHandle) (Relationship, error)
}

// ClientFor returns the client of a cluster, built from whatever credentials
// the caller holds for it.
type ClientFor[C RelationshipReader] func(ctx context.Context, clusterID string) (C, error)

// ChainHop is one volume of a replication chain, with the client of its
// cluster.
type ChainHop[C RelationshipReader] struct {
	Handle lvol.Handle
	Client C
}

// ErrChainDoesNotConverge is a chain whose records loop, or that is longer
// than any chain a volume accumulates.
var ErrChainDoesNotConverge = errors.New("replication chain does not converge")

// maxChainHops bounds the walk. One hop per past fail-over or move, never
// compacted, so the bound is a guard against a looping record rather than an
// estimate of a chain's length.
const maxChainHops = 256

// WalkChain walks the replication chain behind start: start itself, then
// every source-to-target hop until the active volume. known is whether start
// has a relationship at all; a volume without one is its own chain.
func WalkChain[C RelationshipReader](
	ctx context.Context, start lvol.Handle, client C, clientFor ClientFor[C],
) ([]ChainHop[C], bool, error) {
	known := false
	hops := []ChainHop[C]{{Handle: start, Client: client}}
	visited := map[lvol.VolumeHandle]bool{}
	h := start
	for {
		if visited[h.Handle()] || len(hops) > maxChainHops {
			return nil, false, fmt.Errorf("%w: %s, at %s", ErrChainDoesNotConverge, start.Handle(), h.Handle())
		}
		visited[h.Handle()] = true
		rel, err := client.GetVolumeReplicationRelationship(ctx, h.Handle())
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				return hops, known, nil
			}
			return nil, false, err
		}
		known = true
		if !rel.IsSource {
			return hops, known, nil
		}
		target := lvol.Handle{ClusterID: rel.TargetClusterID, PoolRef: rel.TargetPoolID, VolumeID: rel.TargetLvolID}
		targetClient, err := clientFor(ctx, target.ClusterID)
		if err != nil {
			return nil, false, fmt.Errorf("replication chain of %s: %w", start.Handle(), err)
		}
		h, client = target, targetClient
		hops = append(hops, ChainHop[C]{Handle: h, Client: client})
		if rel.ActiveLvolID == "" || rel.ActiveLvolID == rel.TargetLvolID {
			return hops, known, nil
		}
	}
}
