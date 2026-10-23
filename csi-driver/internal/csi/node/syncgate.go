package node

// The stage-time gate of a stretched, sync-replicated cluster
// (design-stretched-sync-replication-ramen.md §9.2, §9.4).
//
// A volume of a sync cluster is served from the site its store is led from, and
// a node attaches it only once the backend reports it led from the node's own
// site. The node's site and whether the cluster is sync come from the driver's
// cluster secret entry, which the site's operator writes; nothing is read from
// the Node object or from a VolumeReplication.
//
// A volume led from the other site is moved by asking this site's operator, over
// the link the node already holds to it, and the stage holds until the backend's
// status names this site. The operator's answer is never taken as permission to
// attach: a move it reports done that the status does not show yet is held one
// more retry, so the node never attaches on a stale status.
//
// Every hold is an error the kubelet retries, so the wait costs a node nothing
// and survives a restart.

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/link/sitev1"
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

	from := st.Site
	client, err := ns.switchover()
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"sync gate: volume %s is led from site %s, and this node cannot ask its operator to move it: %v",
			volumeID, from, err)
	}
	klog.Infof("sync gate: volume %s is led from site %s; asking the operator to move it to %s", volumeID, from, site)
	resp, err := client.Move(ctx, &sitev1.MoveRequest{Volume: volumeID, ToSite: site, AllowForced: true})
	if err != nil {
		return status.Errorf(codes.Unavailable, "sync gate: moving volume %s to site %s: %v", volumeID, site, err)
	}
	switch resp.GetState() {
	case sitev1.MoveResponse_STATE_MOVED:
		after, err := ns.syncStatus(ctx, h)
		if err != nil {
			return status.Errorf(codes.Unavailable, "sync gate: where volume %s is led from: %v", volumeID, err)
		}
		if after.Site != site {
			return status.Errorf(codes.Unavailable,
				"sync gate: volume %s was moved to site %s and the status still names %q; retrying",
				volumeID, site, after.Site)
		}
		klog.Infof("sync gate: volume %s is led from site %s", volumeID, site)
		return nil
	case sitev1.MoveResponse_STATE_REFUSED:
		reason := resp.GetReason()
		if blocking := resp.GetBlockingVolumes(); len(blocking) > 0 {
			reason += fmt.Sprintf(" (volumes still served on site %s: %s)", from, strings.Join(blocking, ", "))
		}
		return status.Errorf(codes.FailedPrecondition,
			"sync gate: the move of volume %s to site %s was refused: %s", volumeID, site, reason)
	default:
		return status.Errorf(codes.Unavailable,
			"sync gate: volume %s is being moved from site %s to %s", volumeID, from, site)
	}
}
