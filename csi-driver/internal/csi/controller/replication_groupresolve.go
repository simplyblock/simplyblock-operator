package controller

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/csi-driver/internal/clusters"
)

// groupSide says which group of a moved consistency group a verb acts on, the
// group analogue of chooseReplica's local member and the chain's active end.
type groupSide int

const (
	// groupActiveEnd is the group serving the data now: Info, Resync and
	// Enable (re-protection attaches the live group).
	groupActiveEnd groupSide = iota
	// groupLocalSite is the group on this driver's own cluster: Demote and
	// Disable act on what this site holds, never on the live primary elsewhere
	// (the per-volume rule of 2026-10-02, PR #618).
	groupLocalSite
)

// resolveGroupTarget resolves a VolumeGroupReplication's group handle to the
// group a verb must act on, and a client for that group's cluster.
//
// A VGR keeps its original group handle across a relocate, while the group it
// names is emptied by design -- its demoted members are deleted so a relocate
// back stays possible -- and the data lives in the peer group of the same name
// as clones (2026-10-04, WordPress A -> B: every group verb on the original
// handle reached the empty source group). The control plane resolves the
// handle (ResolveGroup); the two candidates are the named group and the group
// holding live members. groupActiveEnd picks the latter; groupLocalSite picks
// the one on a cluster flagged local in the driver's secret, preferring the
// live one, and falls back to the live one when no cluster is flagged, as
// chooseReplica does.
//
// A control plane without the resolution endpoint, or a resolution that fails,
// leaves the handle as it is: the verbs then behave as before this resolution.
func resolveGroupTarget(
	ctx context.Context, gh lvol.GroupHandle, side groupSide,
) (lvol.GroupHandle, *atlascp.Client, error) {
	client, err := clusters.ReplicationClient(ctx, gh.ClusterID)
	if err != nil {
		return gh, nil, status.Error(codes.Unavailable, err.Error())
	}
	res, err := client.ResolveGroup(ctx, gh)
	if err != nil {
		klog.Warningf("resolve group %s: %v; acting on the handle as named", gh.Handle(), err)
		return gh, client, nil
	}
	target := chooseGroup(gh, res, side, localClusters())
	if target == gh {
		return gh, client, nil
	}
	targetClient, err := clusters.ReplicationClient(ctx, target.ClusterID)
	if err != nil {
		return gh, nil, status.Error(codes.Unavailable, err.Error())
	}
	klog.Infof("group %s resolves to %s", gh.Handle(), target.Handle())
	return target, targetClient, nil
}

// chooseGroup is resolveGroupTarget's pure choice: the named group or the live
// one, by side and the local clusters (nil when none is flagged).
func chooseGroup(
	gh lvol.GroupHandle, res atlascp.GroupResolution, side groupSide, local map[string]bool,
) lvol.GroupHandle {
	if res.Active == nil || *res.Active == gh {
		return gh
	}
	live := *res.Active
	if side == groupActiveEnd || local == nil {
		return live
	}
	if local[live.ClusterID] {
		return live
	}
	if local[gh.ClusterID] {
		return gh
	}
	return live
}

// localClusters is the set of clusters the driver's secret flags local, nil
// when none is flagged (an older operator) or the secret cannot be read.
func localClusters() map[string]bool {
	local, flagged, err := clusters.Local()
	if err != nil || !flagged {
		return nil
	}
	return local
}
