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
// This file is the third source, and it is the host itself. A staging path is a
// mount, a mount has a device under it, and the device's sysfs entry says which
// subsystem and namespace it belongs to. That is a reading rather than a guess,
// which is what makes it admissible where a default would not be.

package node

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"

	"github.com/simplyblock/csi-driver/internal/mount"
)

// stagedIdentity reads the namespace behind a staging path off the host.
//
// The resolver is built per call rather than held, because each call re-scans
// anyway and a teardown that runs once per volume is not a path worth caching
// for.
func stagedIdentity(
	mounter *mount.Mounter,
) func(ctx context.Context, stagingTargetPath string) (lvol.Connection, error) {
	return func(ctx context.Context, stagingTargetPath string) (lvol.Connection, error) {
		devicePath, err := mounter.DeviceAtMount(stagingTargetPath)
		if err != nil {
			return lvol.Connection{}, fmt.Errorf(
				"read what is mounted at %s: %w", stagingTargetPath, err)
		}
		if devicePath == "" {
			return lvol.Connection{}, fmt.Errorf(
				"nothing is mounted at %s, so the host knows of no namespace to release",
				stagingTargetPath)
		}

		device, err := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{}).
			ByDevicePath(ctx, devicePath)
		if err != nil {
			return lvol.Connection{}, fmt.Errorf(
				"read the namespace %s belongs to: %w", devicePath, err)
		}

		return lvol.Connection{
			NQN:  device.Subsystem.NQN,
			NSID: uint32(device.Namespace.ID),
			UUID: device.Namespace.UUID,
		}, nil
	}
}
