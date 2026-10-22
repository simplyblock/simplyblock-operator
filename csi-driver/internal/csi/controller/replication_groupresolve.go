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
// group analog of chooseReplica's local member and the chain's active end.
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

// groupFailbackCluster is the cluster a group Resync tells the control plane
// to replicate TO: the cluster opposite the group that holds the data.
//
// sbcli's group fail-back reads source_cluster_id as the cluster to re-aim the
// members' replication AT. The class parameter on a site names, by the deploy
// scripts' and the docs' convention, that site's peer. Which cluster is right
// depends on where the data is, never on which site the driver runs on:
//
//   - The group resolved to a group on another site: the members to configure
//     are the live clones over THERE, and the cluster to replicate to is the
//     named group's site. That holds whichever site resyncs. After an unplanned
//     fail-over the recovered site resyncs (#34, run 20261016-1108-ebs: passed
//     through verbatim, the parameter told the live clone's cluster to
//     replicate to itself and the relocate back failed on a copy that was
//     never shipped). On a relocate back the site holding the data resyncs
//     after its demote; picking the driver's own site there named the live
//     group's own cluster, and only the control plane's guard corrected it
//     (run 20261020-1300-ebs, test 2).
//   - The group resolved to itself, or to another group on the same site (a
//     plain relocate: Ramen demoted this site and resyncs it before promoting
//     the peer): the members are HERE, just demoted, and the cluster to
//     replicate to is the peer -- the parameter, exactly as before #34. Naming
//     this site here handed every member its own cluster and the control plane
//     refused the fail-back, so no relocate completed (run 20261019-1541-ebs,
//     tests 1 and 3).
func groupFailbackCluster(named, active lvol.GroupHandle, sourceClusterID string) string {
	if active.ClusterID == named.ClusterID {
		return sourceClusterID
	}
	return named.ClusterID
}

// liveGroupOnPeer reports whether the group a VGR handle resolved to sits on
// another site than this driver's. Enable has nothing to do then: this site's
// policy targets the peer, so attaching it to the peer's live group tells that
// group to replicate to itself, which the control plane refuses (#1502), and
// the promote that follows attaches the new primary's own policy (sbcli's
// attach_reverse_replication and _resume_replication_after_failback). Before
// the refusal the attach went through for the moment until the promote
// replaced it; after it, every Enable failed and csi-addons never promoted, so
// no relocate back completed (run 20261020-1300-ebs, test 2).
//
// The local flag in the driver's secret says which site this is. Without one
// (an older operator), the class's sourceClusterID names the peer by the deploy
// convention. With neither, the group is taken to be this site's.
func liveGroupOnPeer(live lvol.GroupHandle, local map[string]bool, peerClusterID string) bool {
	if local != nil {
		return !local[live.ClusterID]
	}
	return peerClusterID != "" && live.ClusterID == peerClusterID
}
