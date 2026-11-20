// The snapshot calls: list a volume's snapshots. Asking for a backup is not one
// of them, because the control plane takes and removes the snapshot a backup is
// made of (CreateVolumeBackup in backups.go).

package controlplane

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// Snapshot is a point-in-time copy of one volume.
type Snapshot struct {
	ID     string
	Name   string
	Status string
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
