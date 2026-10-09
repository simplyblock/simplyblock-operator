// The snapshot calls of the control-plane client: create one of a volume, find
// the ones a volume has, and delete one. They are the first half of an
// on-demand backup, which snapshots a volume and then asks for that snapshot to
// be backed up, and they live apart from backups.go because a snapshot exists
// and is useful without any backup of it.

package controlplane

import (
	"context"
	"fmt"
	"net/http"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/cpapi"
)

// Snapshot is a point-in-time copy of one volume, held by the storage cluster.
type Snapshot struct {
	ID     string
	Name   string
	Status string

	// GroupID names the consistency group whose frozen cut produced the
	// snapshot, and is empty for a snapshot of one volume taken alone.
	GroupID string
}

// CreateSnapshot snapshots one volume and returns the snapshot's identifier.
//
// The snapshot is taken without a backup of its own. The control plane can
// back a snapshot up in the same call, but it reports a failed backup only in
// its log and answers as though the call succeeded, which leaves the caller
// holding a snapshot and no backup. Requesting the backup separately with
// CreateBackup is what makes that failure visible.
//
// A name already in use answers errs.ErrAlreadyExists, which is how a retried
// creation shows up.
func (c *Client) CreateSnapshot(
	ctx context.Context, clusterID, poolID, volumeID, name string,
) (string, error) {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return "", err
	}
	volume, err := parseUUID("volume id", volumeID)
	if err != nil {
		return "", err
	}

	what := fmt.Sprintf("snapshot volume %s as %s", volumeID, name)
	resp, err := c.api.ClustersStoragePoolsVolumesSnapshotsCreateApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdSnapshotsPostWithResponse(
		ctx, cluster, pool, volume, cpapi.UnderscoreSnapshotParams{Name: name})
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if resp.StatusCode() == http.StatusConflict {
		return "", fmt.Errorf("%s: %w", what, errs.ErrAlreadyExists)
	}
	if resp.HTTPResponse == nil {
		return "", fmt.Errorf("%s: the response carried no headers: %w", what, errs.ErrInvalidResponse)
	}
	return createdID(what, resp.HTTPResponse, resp.Body)
}

// VolumeSnapshots lists the snapshots of one volume.
func (c *Client) VolumeSnapshots(
	ctx context.Context, clusterID, poolID, volumeID string,
) ([]Snapshot, error) {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return nil, err
	}
	volume, err := parseUUID("volume id", volumeID)
	if err != nil {
		return nil, err
	}

	what := "list the snapshots of volume " + volumeID
	resp, err := c.api.ClustersStoragePoolsVolumesSnapshotsListApiV2ClustersClusterIdStoragePoolsPoolIdVolumesVolumeIdSnapshotsGetWithResponse(
		ctx, cluster, pool, volume)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	snapshots, err := decodeBody[[]cpapi.SnapshotDTO](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}

	out := make([]Snapshot, 0, len(snapshots))
	for _, d := range snapshots {
		out = append(out, Snapshot{
			ID:      d.Id.String(),
			Name:    d.Name,
			Status:  d.Status,
			GroupID: d.GroupId,
		})
	}
	return out, nil
}

// DeleteSnapshot deletes one snapshot. A snapshot that is already gone is the
// state being asked for, so it is not an error.
func (c *Client) DeleteSnapshot(ctx context.Context, clusterID, poolID, snapshotID string) error {
	cluster, pool, err := parseIDs(clusterID, poolID)
	if err != nil {
		return err
	}
	snapshot, err := parseUUID("snapshot id", snapshotID)
	if err != nil {
		return err
	}

	what := "delete snapshot " + snapshotID
	resp, err := c.api.ClustersStoragePoolsSnapshotsDeleteApiV2ClustersClusterIdStoragePoolsPoolIdSnapshotsSnapshotIdDeleteWithResponse(
		ctx, cluster, pool, snapshot)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return okOrGone(what, resp.StatusCode(), resp.Body)
}
