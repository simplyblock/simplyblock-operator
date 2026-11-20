// Moving a volume to another cluster on the same control plane: replication
// in migration mode into the target, a commit that cuts over and deletes the
// source, and the release that lets the cutover proceed once every host can
// reach the target. A supercluster's operator moves volumes between its
// members this way; the calls are the control plane's, so they live with the
// rest of its client.

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/simplyblock/atlas/internal/cpapi"
	"github.com/simplyblock/atlas/lvol"
)

// MoveTarget is where a volume moves.
type MoveTarget struct {
	// ClusterID is the destination cluster.
	ClusterID string

	// PoolID is the pool on the destination the volume lands in. Empty lets
	// the control plane choose.
	PoolID string

	// IntervalMinutes is how often the source takes the snapshots replication
	// ships until the commit. The control plane takes none at 0, and only the
	// snapshots a user takes would replicate.
	IntervalMinutes int
}

// StartVolumeMove starts replicating the volume into the target in migration
// mode. A destination the control plane cannot use (its own cluster, a pool
// of another cluster, an encrypted volume) is refused with a 400 carrying the
// reason.
func (c *Client) StartVolumeMove(ctx context.Context, h lvol.VolumeHandle, target MoveTarget) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	targetCluster, err := parseUUID("target cluster id", target.ClusterID)
	if err != nil {
		return err
	}
	mode := cpapi.ReplicationStartParamsModeMigration
	interval := target.IntervalMinutes
	body := cpapi.ReplicationStartParams{
		Mode:                 &mode,
		ReplicationClusterId: &targetCluster,
		IntervalMin:          &interval,
	}
	if target.PoolID != "" {
		targetPool, err := parseUUID("target pool id", target.PoolID)
		if err != nil {
			return err
		}
		body.ReplicationPoolId = &targetPool
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationStartApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationStartPostWithResponse(
		ctx, cluster, pool, volume, body)
	if err != nil {
		return fmt.Errorf("start moving volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusNoContent && code != http.StatusOK {
		return respError("start moving volume "+string(h), code, resp.Body)
	}
	return nil
}

// StopVolumeReplication stops the volume's replication, which abandons a move
// that has not been committed.
func (c *Client) StopVolumeReplication(ctx context.Context, h lvol.VolumeHandle) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationStopApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationStopPostWithResponse(
		ctx, cluster, pool, volume)
	if err != nil {
		return fmt.Errorf("stop replicating volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusNoContent && code != http.StatusOK {
		return respError("stop replicating volume "+string(h), code, resp.Body)
	}
	return nil
}

// CommitVolumeMove queues the cutover into the target and the deletion of the
// source after it, and returns the control plane's task carrying both. A
// commit the control plane already holds, queued, running, or done, is
// answered 409 naming the same task, so a caller that cannot tell whether its
// first commit arrived resends it and learns the task either way.
func (c *Client) CommitVolumeMove(ctx context.Context, h lvol.VolumeHandle) (string, error) {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return "", err
	}
	deleteSource := true
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationCommitApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationCommitPostWithResponse(
		ctx, cluster, pool, volume, cpapi.CommitParams{DeleteSource: &deleteSource})
	if err != nil {
		return "", fmt.Errorf("commit the move of volume %s: %w", h, err)
	}
	what := "commit the move of volume " + string(h)
	switch code := resp.StatusCode(); code {
	case http.StatusAccepted:
		return createdID(what, resp.HTTPResponse, resp.Body)
	case http.StatusConflict:
		var held struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(resp.Body, &held) == nil && held.TaskID != "" {
			return held.TaskID, nil
		}
		if loc := resp.HTTPResponse.Header.Get("Location"); strings.Contains(loc, "/tasks/") {
			return createdID(what, &http.Response{StatusCode: http.StatusOK, Header: resp.HTTPResponse.Header}, nil)
		}
		return "", respError(what, code, resp.Body)
	default:
		return "", respError(what, code, resp.Body)
	}
}

// ProceedVolumeCutover releases a cutover waiting for the hosts: the control
// plane holds a committed move in cutover_pending until this call, or until
// its own timeout, so the hosts can connect to the target first. Before the
// cutover is pending there is nothing to release, and the call fails with an
// error unwrapping to errs.ErrNotFound.
func (c *Client) ProceedVolumeCutover(ctx context.Context, h lvol.VolumeHandle) error {
	cluster, pool, volume, err := h.Split()
	if err != nil {
		return err
	}
	resp, err := c.api.ClustersStoragePoolsVolumesReplicationCutoverProceedApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdReplicationCutoverProceedPostWithResponse(
		ctx, cluster, pool, volume)
	if err != nil {
		return fmt.Errorf("release the cutover of volume %s: %w", h, err)
	}
	if code := resp.StatusCode(); code != http.StatusNoContent && code != http.StatusOK {
		return respError("release the cutover of volume "+string(h), code, resp.Body)
	}
	return nil
}
