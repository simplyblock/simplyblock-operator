// The csi-addons Replication service: EnableVolumeReplication,
// DisableVolumeReplication, GetVolumeReplicationInfo (design §5.1), and the
// Phase 2 lifecycle verbs PromoteVolume, DemoteVolume, and ResyncVolume
// (design §5.2). Each verb is a thin adapter onto the atlas-lib control-plane
// client's replication calls, resolved through the same
// {clusterID}:{poolID}:{lvolID} handle every other RPC uses.
package controller

import (
	"context"
	"errors"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// replicationPolicyParam is the VolumeReplicationClass parameter key naming
// the backend policy to attach. It is spelled as an id rather than the
// design's own `replicationPolicy` (a name, §7.1), because resolving a name
// to an id needs a policy-list-and-match call this phase does not yet wrap
// in atlas-lib. A future change adds that resolution and accepts either.
const replicationPolicyParam = "replicationPolicyID"

// sourceClusterIDParam is the VolumeReplicationClass parameter naming the
// cluster to resync from, when it isn't the one the backend already has on
// record for this volume's relationship. Optional: sbcli's replication_failback
// resolves it from the existing relationship when omitted (the common case,
// design §5.2's "Recovered source").
const sourceClusterIDParam = "sourceClusterID"

// volumeIDCarrier is every Replication request type: each exposes the legacy
// flat VolumeId field and the ReplicationSource oneof.
type volumeIDCarrier interface {
	GetVolumeId() string
	GetReplicationSource() *replication.ReplicationSource
}

// volumeIDFrom resolves a request's volume ID. The real upstream sidecar
// (kubernetes-csi-addons v0.15.0, internal/sidecar/service.ReplicationServer)
// proxies every Replication RPC through ReplicationSource and never sets the
// legacy flat VolumeId, so that field is checked first; the flat field is
// kept as a fallback for any caller that still sends it.
func volumeIDFrom(req volumeIDCarrier) string {
	if v := req.GetReplicationSource().GetVolume().GetVolumeId(); v != "" {
		return v
	}
	return req.GetVolumeId()
}

// resolveToLocalReplica rewrites h to the volume that actually replicates
// data on this side of an existing pairing, when h names the OTHER (foreign)
// side of it instead. Ramen's S3-restore recreates a destination PV carrying
// the ORIGINAL source's own volumeHandle verbatim (confirmed live
// 2026-09-23, relocate M-02), and every Replication RPC parses its target
// straight from the handle it's given -- without this resolution, "promote"
// or "enable replication" would operate on the foreign, original volume
// instead of the local replica that has actually been receiving replicated
// data. The returned handle and client change together, since the target
// side may live on a different cluster with its own secret.json entry.
//
// Returns h and client unchanged when h has no replication relationship yet
// (errs.ErrNotFound -- the ordinary case for a volume never enabled for
// replication, e.g. M-01's first-ever protect) or when h already names the
// target side.
func resolveToLocalReplica(
	ctx context.Context, h *lvol.Handle, client *atlascp.Client,
) (*lvol.Handle, *atlascp.Client, error) {
	rel, err := client.GetVolumeReplicationRelationship(ctx, h.Handle())
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return h, client, nil
		}
		return nil, nil, err
	}
	if !rel.IsSource {
		return h, client, nil
	}
	target := &lvol.Handle{ClusterID: rel.TargetClusterID, PoolRef: rel.TargetPoolID, VolumeID: rel.TargetLvolID}
	targetClient, err := clusters.ReplicationClient(ctx, target.ClusterID)
	if err != nil {
		return nil, nil, err
	}
	return target, targetClient, nil
}

// EnableVolumeReplication attaches the volume to the policy named by the
// VolumeReplicationClass. Attaching to the policy the volume already follows
// is success (the backend's own idempotency, P0-2).
//
// The design (§5.1, §10) wants a different-policy attach refused with
// FAILED_PRECONDITION, because a silent re-attach forces a full re-sync. That
// refusal is NOT implemented here: it needs the volume's CURRENTLY attached
// policy id to compare against, and neither the P0-1 status read nor any
// other backend endpoint exposes it today (attach_policy in sbcli's
// replication_policy_controller.py detaches and re-attaches on a policy
// change without refusing). Until the backend adds that field, a
// different-policy attach silently re-syncs, exactly as it does through
// every other existing caller of this same endpoint.
func (cs *Server) EnableVolumeReplication(
	ctx context.Context,
	req *replication.EnableVolumeReplicationRequest,
) (*replication.EnableVolumeReplicationResponse, error) {
	policyID := req.GetParameters()[replicationPolicyParam]
	if policyID == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"VolumeReplicationClass parameter %q is required", replicationPolicyParam)
	}
	h, err := csicommon.ParseVolumeHandle(volumeIDFrom(req))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	h, client, err = resolveToLocalReplica(ctx, h, client)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := client.EnableVolumeReplication(ctx, h.Handle(), policyID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			// This backend's replication is one-way: the destination never
			// carries a persistent, independently-provisioned LVol of its
			// own -- the writable clone only comes into existence when
			// PromoteVolume clones the last replicated snapshot. csi-addons
			// always calls Enable before Promote, unconditionally, for
			// whichever side is becoming Primary, so on a first-ever
			// relocate (nothing for resolveToLocalReplica to redirect
			// through either, since no relationship exists until a promote
			// has actually happened) Enable is legitimately handed a handle
			// that names nothing yet. There is nothing to attach a policy
			// to, and nothing wrong either -- PromoteVolume is what actually
			// creates and validates the volume, and is what surfaces a real
			// error if there truly is nothing to clone from.
			return &replication.EnableVolumeReplicationResponse{}, nil
		}
		return nil, classifyEnableVolumeReplicationError(err)
	}
	return &replication.EnableVolumeReplicationResponse{}, nil
}

// DisableVolumeReplication detaches the volume from whatever policy it
// follows. Detaching an already-detached volume is success; a cutover in
// flight (409) is a retryable ABORTED, since Ramen re-drives every reconcile.
func (cs *Server) DisableVolumeReplication(
	ctx context.Context,
	req *replication.DisableVolumeReplicationRequest,
) (*replication.DisableVolumeReplicationResponse, error) {
	h, err := csicommon.ParseVolumeHandle(volumeIDFrom(req))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	h, client, err = resolveToLocalReplica(ctx, h, client)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := client.DisableVolumeReplication(ctx, h.Handle()); err != nil {
		return nil, classifyDisableVolumeReplicationError(err)
	}
	return &replication.DisableVolumeReplicationResponse{}, nil
}

// GetVolumeReplicationInfo returns the volume's replicated-life status.
//
// The spec's response carries only LastSyncTime in this version
// (github.com/csi-addons/spec v0.2.0); lastSyncBytes/lastSyncDuration are not
// yet part of the wire contract this driver builds against, so they cannot
// be set even though the backend status read already computes them (design
// §6.1). Ramen's condition derivation (§6.2) does not depend on them.
func (cs *Server) GetVolumeReplicationInfo(
	ctx context.Context,
	req *replication.GetVolumeReplicationInfoRequest,
) (*replication.GetVolumeReplicationInfoResponse, error) {
	h, err := csicommon.ParseVolumeHandle(volumeIDFrom(req))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	info, err := client.GetVolumeReplicationInfo(ctx, h.Handle())
	if err != nil {
		return nil, classifyGetVolumeReplicationInfoError(err)
	}
	resp := &replication.GetVolumeReplicationInfoResponse{}
	if info.LastReplicatedAt != nil {
		resp.LastSyncTime = timestamppb.New(*info.LastReplicatedAt)
	}
	return resp, nil
}

// PromoteVolume brings the volume up as primary on this cluster (design
// §5.2). Force=true is the unplanned path: it clones the last fully
// replicated generation and ignores demote state entirely, because its whole
// premise is that the peer may never have been reachable to demote.
// Force=false is the planned path, refused with ABORTED (retryable) while a
// demote is still converging and FAILED_PRECONDITION when no demote was ever
// requested -- the split matters because the vendored csi-addons controller
// auto-escalates ANY FAILED_PRECONDITION from a force=false promote to
// force=true inline, with no wait-and-retry grace period of its own.
func (cs *Server) PromoteVolume(
	ctx context.Context,
	req *replication.PromoteVolumeRequest,
) (*replication.PromoteVolumeResponse, error) {
	h, err := csicommon.ParseVolumeHandle(volumeIDFrom(req))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	h, client, err = resolveToLocalReplica(ctx, h, client)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := client.PromoteVolume(ctx, h.Handle(), req.GetForce()); err != nil {
		return nil, classifyPromoteVolumeError(err)
	}
	return &replication.PromoteVolumeResponse{}, nil
}

// DemoteVolume fences the source and confirms the last write replicated
// (P0-3) -- the lossless half of a planned swap. Synchronous and
// non-blocking: it never waits out the backend's own convergence loop.
// While still converging it returns ABORTED (retryable), matching the actual
// upstream reconciler's requeue-until-ready behavior for a Secondary
// transition that has not yet settled, rather than holding the RPC open.
func (cs *Server) DemoteVolume(
	ctx context.Context,
	req *replication.DemoteVolumeRequest,
) (*replication.DemoteVolumeResponse, error) {
	h, err := csicommon.ParseVolumeHandle(volumeIDFrom(req))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	h, client, err = resolveToLocalReplica(ctx, h, client)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	done, err := client.DemoteVolume(ctx, h.Handle())
	if err != nil {
		return nil, classifyDemoteVolumeError(err)
	}
	if !done {
		return nil, status.Error(codes.Aborted, "demote is still converging")
	}
	return &replication.DemoteVolumeResponse{}, nil
}

// ResyncVolume reconciles a diverged copy back onto the current primary's
// history. It configures the reverse direction only and reports readiness
// off the ordinary lag read -- it never cuts over, matching the design's own
// "it never merges" (§5.2): cutover is PromoteVolume's job, on a separate,
// later call.
func (cs *Server) ResyncVolume(
	ctx context.Context,
	req *replication.ResyncVolumeRequest,
) (*replication.ResyncVolumeResponse, error) {
	h, err := csicommon.ParseVolumeHandle(volumeIDFrom(req))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	client, err := clusters.ReplicationClient(ctx, h.ClusterID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	sourceClusterID := req.GetParameters()[sourceClusterIDParam]
	if err := client.ResyncVolume(ctx, h.Handle(), sourceClusterID); err != nil {
		return nil, classifyResyncVolumeError(err)
	}
	info, err := client.GetVolumeReplicationInfo(ctx, h.Handle())
	if err != nil {
		return nil, classifyGetVolumeReplicationInfoError(err)
	}
	ready := info.LagSeconds == nil || info.LagBudgetSeconds == nil || *info.LagSeconds <= *info.LagBudgetSeconds
	return &replication.ResyncVolumeResponse{Ready: ready}, nil
}
