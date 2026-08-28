package vdo

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/lvm"
)

type Volume struct {
	volumeGroupName   string
	poolName          string
	logicalVolumeName string
}

// NewVolume identifies an existing VDO volume by the volume group, pool, and
// logical volume names CreateLogicalVolume gave it, so a caller outside this
// package can construct one to pass to UpdateVolume. Fields stay unexported:
// nothing outside this package reads them back out.
func NewVolume(volumeGroupName, poolName, logicalVolumeName string) *Volume {
	return &Volume{
		volumeGroupName:   volumeGroupName,
		poolName:          poolName,
		logicalVolumeName: logicalVolumeName,
	}
}

type volumeHandler struct {
}

func (v *volumeHandler) Name() string {
	return "vdo"
}

func (v *volumeHandler) Handles(def lvm.LogicalVolumeDefinition) bool {
	return def.Compression || def.Deduplication
}

func (v *volumeHandler) CreateVolumeArgs(
	def lvm.LogicalVolumeDefinition,
) []string {
	if def.Compression || def.Deduplication {
		return []string{
			"--type", "vdo",
			"--config", "activation{checks=0}",
			"--compression", yn(def.Compression),
			"--deduplication", yn(def.Deduplication),
		}
	}
	return nil
}

func init() {
	lvm.RegisterVolumeProvisioning(&volumeHandler{})
}

func UpdateVolume(
	ctx context.Context, manager *lvm.Manager, volume *Volume, compression bool, deduplication bool,
) error {
	_, err := manager.Run(ctx,
		"lvchange",
		"--compression", yn(compression),
		"--deduplication", yn(deduplication),
		volume.volumeGroupName+"/"+volume.poolName,
	)
	if err != nil {
		return fmt.Errorf("set VDO features on %s/%s: %w", volume.volumeGroupName, volume.poolName, err)
	}
	return nil
}

func yn(b bool) string {
	if b {
		return "y"
	}
	return "n"
}
