// Naming a volume this driver did not stage.
//
// A volume staged before the volume stack existed has no stack record, and the
// teardown of one is reconstructed instead: the shape is what that version
// could build, and the namespace comes from the stashed volume context. Both of
// those are in stage.go, and between them they cover every volume the previous
// node service finished staging.
//
// What they do not cover is the one it did not finish. The old service wrote
// volume-context.json as the last step of NodeStageVolume, after the connect
// and after the mount, so a driver that died in that window left a volume that
// is up on the host and names itself nowhere. The teardown then has nothing to
// act on and refuses, correctly: releasing a namespace nothing identifies
// detaches whichever one is found first, and on a node serving several volumes
// that is somebody else's.
//
// This file is the third source, and it is the host itself. A staging path
// resolves to a device number, and the namespace carrying that number says
// which subsystem and namespace id it is. That is a reading rather than a
// guess, which is what makes it admissible where a default would not be.
//
// The number and not the path, because the path a volume was mounted from is
// not the path sysfs records. The previous node service mounted what its
// initiator handed back, which is a by-id symlink
// (/dev/disk/by-id/nvme-SPDK_Controller1_…), while sysfs knows the namespace as
// /dev/nvme0n1: comparing those two strings finds nothing, and finding nothing
// here means refusing exactly the volumes this path exists for.

package node

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
)

// stagedIdentity reads the namespace behind a staging path off the host.
//
// The resolver is built per call rather than held, because each call re-scans
// anyway and a teardown that runs once per volume is not a path worth caching
// for.
func stagedIdentity(
	ctx context.Context, stagingTargetPath string,
) (lvol.Connection, error) {
	number, err := nvme.DeviceNumberAt(stagingTargetPath)
	if err != nil {
		return lvol.Connection{}, fmt.Errorf(
			"read what is staged at %s: %w", stagingTargetPath, err)
	}

	device, err := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{}).
		ByDeviceNumber(ctx, number)
	if err != nil {
		return lvol.Connection{}, fmt.Errorf(
			"read the namespace device %s belongs to: %w", number, err)
	}

	return lvol.Connection{
		NQN:  device.Subsystem.NQN,
		NSID: uint32(device.Namespace.ID),
		UUID: device.Namespace.UUID,
	}, nil
}
