package node

// The stage-time gate of a stretched, sync-replicated cluster
// (design-stretched-sync-replication-ramen.md §9.2, §9.4).
//
// A volume of a sync cluster is served from the site its store is led from, and
// a node attaches it only once the backend reports it led from the node's own
// site. The node's site and whether the cluster is sync come from the driver's
// cluster secret entry, which the site's operator writes. Nothing is read from
// the Node object or from a VolumeReplication.
//
// The node never moves a volume. The site's operator moves it when Ramen makes
// the volume's VolumeReplicationGroup primary on this site, before Ramen points
// the placement here (internal/controllers/vrgfollow in the operator). A stage
// that finds the volume led from the other site holds until the backend's status
// names this site, so the node never attaches on a stale status.
//
// Every hold is an error the kubelet retries, so the wait costs a node nothing
// and survives a restart.

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"
)

// ensureLedHere returns once volumeID is led from this node's site, or the
// status the stage holds with.
func (ns *Server) ensureLedHere(ctx context.Context, volumeID string) error {
	h := lvol.VolumeHandle(volumeID)
	cluster, _, _, err := h.Split()
	if err != nil {
		// A handle this gate cannot read is not a sync volume's: the rest of
		// the stage reports it.
		return nil
	}
	if ns.syncSite == nil {
		return nil
	}
	site, sync, err := ns.syncSite(cluster.String())
	if err != nil {
		return status.Errorf(codes.Unavailable, "sync gate: the cluster secret: %v", err)
	}
	if !sync {
		return nil
	}
	if site == "" {
		return status.Errorf(codes.FailedPrecondition,
			"sync gate: the cluster secret marks cluster %s sync-replicated but names no site for this node", cluster)
	}

	st, err := ns.syncStatus(ctx, h)
	if err != nil {
		return status.Errorf(codes.Unavailable, "sync gate: where volume %s is led from: %v", volumeID, err)
	}
	switch st.Site {
	case site:
		return nil
	case "":
		return status.Errorf(codes.Unavailable,
			"sync gate: volume %s is led from no site yet; waiting for its leader", volumeID)
	}

	// The move is the operator's, made when Ramen makes the volume's VRG primary
	// on this site, which Ramen does before it points the placement here. A stage
	// that gets here first, or a pod scheduled here outside Ramen, waits for it.
	return status.Errorf(codes.Unavailable,
		"sync gate: volume %s is led from site %s, not this node's site %s; "+
			"waiting for it to be moved here (the operator moves it when Ramen makes its VRG primary on this site)",
		volumeID, st.Site, site)
}
