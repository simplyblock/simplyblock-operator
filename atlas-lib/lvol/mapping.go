package lvol

import (
	"context"

	"github.com/simplyblock/atlas/nvme"
)

// Mapper resolves a logical volume to the local NVMe device that backs it,
// bridging control-plane identity (VolumeHandle) and node-local reality
// (an /dev/nvmeXnY namespace).
type Mapper interface {
	// Resolve returns the local NVMe device for id, or an error wrapping
	// errs.ErrNotFound if no matching namespace is attached on this node.
	Resolve(ctx context.Context, id VolumeHandle) (nvme.Device, error)
}
