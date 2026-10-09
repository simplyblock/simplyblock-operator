// The snapshot and backup-request calls: snapshot a volume, list a volume's
// snapshots, and ask for one to be backed up. They are the request side of an
// on-demand backup, and backups.go reads what the store holds.

package controlplane

import (
	"context"
	"fmt"
	"net/http"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/internal/cpapi"
)

// Snapshot is a point-in-time copy of one volume.
type Snapshot struct {
	ID     string
	Name   string
	Status string
}

// CreateSnapshot snapshots one volume and returns the snapshot's identifier. It
// takes no inline backup, because the control plane reports a failed one only in
// its log, so the caller asks for it with CreateBackup. A name already in use
// answers errs.ErrAlreadyExists, which is how a retried creation shows up.
func (c *Client) CreateSnapshot(ctx context.Context, clusterID, poolID, volumeID, name string) (string, error) {
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
	return createdID(what, resp.HTTPResponse, resp.Body)
}

// VolumeSnapshots lists the snapshots of one volume.
func (c *Client) VolumeSnapshots(ctx context.Context, clusterID, poolID, volumeID string) ([]Snapshot, error) {
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
	dtos, err := decodeBody[[]cpapi.SnapshotDTO](what, resp.StatusCode(), resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, Snapshot{ID: d.Id.String(), Name: d.Name, Status: d.Status})
	}
	return out, nil
}

// CreateBackup asks the control plane to back a snapshot up. It returns once the
// request is accepted, and the copy's progress is read through ListBackups.
func (c *Client) CreateBackup(ctx context.Context, clusterID, snapshotID string) error {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return err
	}
	if _, err := parseUUID("snapshot id", snapshotID); err != nil {
		return err
	}

	what := "back up snapshot " + snapshotID
	resp, err := c.api.ClustersBackupsCreateApiV2ClustersClusterIdBackupsPostWithResponse(
		ctx, cluster, nil, cpapi.UnderscoreBackupSnapshotParams{SnapshotId: snapshotID})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return respError(what, code, resp.Body)
	}
	return nil
}
