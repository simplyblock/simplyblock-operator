package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

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
