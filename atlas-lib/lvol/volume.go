package lvol

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// VolumeHandle is the stable, cluster-wide identifier of a logical volume —
// the value used as the CSI volume_id. It encodes the three simplyblock UUIDs
// a volume is addressed by, colon-separated:
//
//	<clusterID>:<poolID>:<volumeID>
type VolumeHandle string

// Split decomposes the handle into the cluster, pool, and volume UUIDs it
// encodes. It returns an error unless the handle is exactly three
// colon-separated UUIDs.
func (h VolumeHandle) Split() (clusterID, poolID, volumeID uuid.UUID, err error) {
	parts := strings.Split(string(h), ":")
	if len(parts) != 3 {
		return uuid.Nil, uuid.Nil, uuid.Nil,
			fmt.Errorf("invalid volume handle %q: want clusterID:poolID:volumeID", h)
	}
	if clusterID, err = uuid.Parse(parts[0]); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("volume handle %q: cluster id: %w", h, err)
	}
	if poolID, err = uuid.Parse(parts[1]); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("volume handle %q: pool id: %w", h, err)
	}
	if volumeID, err = uuid.Parse(parts[2]); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("volume handle %q: volume id: %w", h, err)
	}
	return clusterID, poolID, volumeID, nil
}

// Volume is the identity of a simplyblock logical volume, independent of
// where (or whether) it is currently attached to a node.
type Volume struct {
	ID        VolumeHandle
	Name      string
	Pool      string
	SizeBytes uint64
	NQN       string // subsystem NQN this volume is published under
}
