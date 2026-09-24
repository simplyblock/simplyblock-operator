package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

func uuidPtrString(u *openapi_types.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// ReplicationStatus is the typed steady-state replication status of one
// volume, for the volume's whole replicated life -- unlike a cutover-record
// relationship read, this is never a 404 for a volume that exists.
//
// The pointer fields mirror the API's own optionality: a volume that has
// never replicated reports every timing/lag field nil, which is a valid
// answer, not an error, and is a different thing from a genuine zero.
type ReplicationStatus struct {
	Role  string
	State string

	LastReplicatedAt *time.Time
	LagSeconds       *int
	LagBudgetSeconds *int

	OutstandingCount int
	OutstandingBytes int

	FailingCount    int
	MaxRetryReached bool

	LastCycleBytes   *int
	LastCycleSeconds *int

	Resyncing bool
}

func replicationStatusFromDTO(d cpapi.ReplicationStatusDTO) ReplicationStatus {
	return ReplicationStatus{
		Role:             string(d.Role),
		State:            string(d.State),
		LastReplicatedAt: d.LastReplicatedAt,
		LagSeconds:       d.LagSeconds,
		LagBudgetSeconds: d.LagBudgetSeconds,
		OutstandingCount: intFrom(d.OutstandingCount),
		OutstandingBytes: intFrom(d.OutstandingBytes),
		FailingCount:     intFrom(d.FailingCount),
		MaxRetryReached:  boolFrom(d.MaxRetryReached),
		LastCycleBytes:   d.LastCycleBytes,
		LastCycleSeconds: d.LastCycleSeconds,
		Resyncing:        boolFrom(d.Resyncing),
	}
}

func intFrom(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func boolFrom(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// EnableVolumeReplication attaches the volume to the named replication
// policy, starting replication. Attaching a volume already following that
// same policy is success (the backend's own idempotency); attaching one that
// follows a different policy is refused (control-plane 412), because a
// silent re-attach would force a full re-sync.
func (c *Client) EnableVolumeReplication(ctx context.Context, h lvol.VolumeHandle, policyID string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	policy, err := parseUUID("replication policy id", policyID)
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesUpdateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdPutWithResponse(
		ctx, cluster, pool, volume, cpapi.UpdatableLVolParams{ReplicationPolicyId: &policy})
	if err != nil {
		return fmt.Errorf("enable replication on volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("enable replication on volume "+string(h), code, resp.Body)
	}
	return nil
}

// DisableVolumeReplication detaches the volume from whatever replication
// policy it follows, stopping replication. Detaching a volume that follows no
// policy is success (the backend's own idempotency); a 409 (a cutover in
// flight) is a retryable refusal.
func (c *Client) DisableVolumeReplication(ctx context.Context, h lvol.VolumeHandle) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	// ReplicationPolicyId is `omitempty` on the generated request struct, so
	// building it with a nil pointer would drop the key entirely rather than
	// send it as an explicit null -- and the backend distinguishes "the key
	// was absent" (leave the policy alone) from "the key was null" (detach)
	// by which keys the request body carries, not by the decoded value. The
	// raw-body variant is the only way to say "null" here.
	resp, err := c.api.ClustersStoragePoolsVolumesUpdateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdPutWithBodyWithResponse(
		ctx, cluster, pool, volume, "application/json", strings.NewReader(`{"replication_policy_id":null}`))
	if err != nil {
		return fmt.Errorf("disable replication on volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("disable replication on volume "+string(h), code, resp.Body)
	}
	return nil
}

// GetVolumeReplicationInfo returns the volume's typed steady-state
// replication status.
func (c *Client) GetVolumeReplicationInfo(ctx context.Context, h lvol.VolumeHandle) (ReplicationStatus, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return ReplicationStatus{}, err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationStatusApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationStatusGetWithResponse(
		ctx, cluster, pool, volume)
	if err != nil {
		return ReplicationStatus{}, fmt.Errorf("replication status of volume %s: %w", h, err)
	}
	d, err := payload("replication status of volume "+string(h), resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return ReplicationStatus{}, err
	}
	return replicationStatusFromDTO(*d), nil
}

// PromoteVolume brings the volume up as primary on this cluster.
//
// force=true is the unplanned path: it ignores demote state entirely,
// because its whole premise is that the peer may never have been reachable
// to demote. force=false is the planned path, gated on a completed demote
// (P0-3): a 409 (demote still converging, retryable) or 412 (no demote was
// ever requested) surfaces as a *StatusError the caller classifies -- the
// 409/412 split matters because the vendored csi-addons controller
// auto-escalates ANY FAILED_PRECONDITION from a force=false promote to
// force=true inline, with no wait-and-retry grace period of its own.
func (c *Client) PromoteVolume(ctx context.Context, h lvol.VolumeHandle, force bool) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	planned := !force
	params := &cpapi.ClustersStoragePoolsVolumesReplicationFailoverApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationFailoverPostParams{
		Planned: &planned,
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationFailoverApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationFailoverPostWithResponse(
		ctx, cluster, pool, volume, params)
	if err != nil {
		return fmt.Errorf("promote volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("promote volume "+string(h), code, resp.Body)
	}
	return nil
}

// DemoteVolume fences the source and confirms the last write replicated
// (P0-3) -- the lossless half of a planned swap. Synchronous and
// re-drivable, not queued: it returns done=false while the final snapshot is
// still converging, and the caller (the driver's DemoteVolume RPC) is
// expected to call this again rather than block, matching the backend's own
// call-repeatedly contract.
func (c *Client) DemoteVolume(ctx context.Context, h lvol.VolumeHandle) (bool, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return false, err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationDemoteApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationDemotePostWithResponse(
		ctx, cluster, pool, volume)
	if err != nil {
		return false, fmt.Errorf("demote volume %s: %w", h, err)
	}
	switch code := resp.StatusCode(); code {
	case http.StatusNoContent:
		return true, nil
	case http.StatusAccepted:
		return false, nil
	default:
		return false, respError("demote volume "+string(h), code, resp.Body)
	}
}

// ResyncVolume reconciles a diverged copy back onto the current primary's
// history. It configures the reverse direction only -- it never cuts over,
// matching the design's own "it never merges": cutover is PromoteVolume's job
// on a separate, later call. sourceClusterID selects the source explicitly
// when it isn't the cluster's configured default; "" leaves it unset.
func (c *Client) ResyncVolume(ctx context.Context, h lvol.VolumeHandle, sourceClusterID string) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	var body cpapi.FailbackParams
	if sourceClusterID != "" {
		id, err := parseUUID("source cluster id", sourceClusterID)
		if err != nil {
			return err
		}
		body.SourceClusterId = &id
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationFailbackApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationFailbackPostWithResponse(
		ctx, cluster, pool, volume, body)
	if err != nil {
		return fmt.Errorf("resync volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNoContent {
		return respError("resync volume "+string(h), code, resp.Body)
	}
	return nil
}

// Relationship is the replication pairing h belongs to: which volume
// replicates to which, and which side h itself names (IsSource). TargetClusterID/
// TargetPoolID/TargetLvolID always name the same, fixed target (replica) side
// of the pairing regardless of whether h names the source or the target --
// querying by either volume's own id returns the identical target_* answer.
type Relationship struct {
	IsSource bool

	SourceClusterID string
	SourceLvolID    string

	TargetClusterID string
	TargetPoolID    string
	TargetLvolID    string
}

// GetVolumeReplicationRelationship resolves h to its replication pairing.
// This is what a caller handed a volume identity inherited from the OTHER
// side of a pairing (e.g. a destination PVC whose PV was restored carrying
// the source's own volumeHandle) uses to find the volume it should actually
// operate on locally: TargetClusterID/TargetPoolID/TargetLvolID name that
// volume regardless of which side h itself named. Returns an error
// unwrapping to errs.ErrNotFound when h has no replication relationship at
// all yet (e.g. a volume never enabled for replication) -- callers treat that
// as "use h unchanged," not a failure.
// GetVolumeReplicationRelationship reads the relationship through the
// cluster-scoped endpoint, not the pool-scoped one: the pool-scoped route
// requires the queried volume to still exist (sbcli's FastAPI Volume
// dependency 404s before the handler body runs), but a relationship must stay
// resolvable by SOURCE id after the source volume itself is deleted -- e.g. a
// demoted volume whose fail-over already completed and was reaped by
// lvol_monitor's deferred-removal hold, confirmed live 2026-09-24 (relocate
// M-02's round trip: resolveToLocalReplica needs exactly this to redirect
// DemoteVolume/DisableVolumeReplication on the SECOND hop of a relocate).
func (c *Client) GetVolumeReplicationRelationship(ctx context.Context, h lvol.VolumeHandle) (Relationship, error) {
	cluster, _, volume, err := h.Split()
	if err != nil {
		return Relationship{}, err
	}
	resp, err := c.api.ClustersReplicationRelationshipsDetailApiV2ClustersClusterIdReplicationRelationshipsLvolIdGetWithResponse(
		ctx, cluster, volume)
	if err != nil {
		return Relationship{}, fmt.Errorf("replication relationship of volume %s: %w", h, err)
	}
	d, err := payload("replication relationship of volume "+string(h), resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return Relationship{}, err
	}
	return Relationship{
		IsSource:        d.IsSource,
		SourceClusterID: uuidPtrString(d.SourceClusterId),
		SourceLvolID:    uuidPtrString(d.SourceLvolId),
		TargetClusterID: uuidPtrString(d.TargetClusterId),
		TargetPoolID:    uuidPtrString(d.TargetPoolId),
		TargetLvolID:    uuidPtrString(d.TargetLvolId),
	}, nil
}
