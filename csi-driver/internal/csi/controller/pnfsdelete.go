// Tearing an export down before the volume it serves.
//
// The order is forced: the record is the only description of a mount, an
// exports entry, and an attached namespace on the host, so deleting the volume
// first is an EIO every process in the filesystem must be killed to clear.
//
// Waiting is a returned error rather than a loop, because the provisioner is
// already the retry loop.

package controller

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"
)

// lvolIDOf reads the logical volume out of a handle, which is the key an
// export record is named by.
func lvolIDOf(volumeHandle string) (string, bool) {
	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(volumeHandle))
	if !ok {
		return "", false
	}
	return handle.VolumeID, true
}

// deleteExportBefore removes the export serving this volume, if one does.
//
// A volume with no export is not an error: every block volume takes this path
// too, which is the point -- nothing has to know which kind it is holding.
func deleteExportBefore(ctx context.Context, registry ExportRegistry, volumeHandle string) error {
	if registry == nil {
		return nil
	}
	lvolID, ok := lvolIDOf(volumeHandle)
	if !ok {
		return nil
	}

	name := exportRecordName(lvolID)
	gone, err := registry.DeleteExport(ctx, name)
	if err != nil {
		return status.Errorf(codes.Internal, "deleting export %s: %v", name, err)
	}
	if !gone {
		// Still finalizing on the host. Unavailable rather than Aborted:
		// nothing is wrong and no call is duplicated, the volume just cannot
		// be deleted yet.
		return status.Errorf(codes.Unavailable,
			"export %s is still being torn down on its host", name)
	}
	return nil
}

// growExportAfter records the volume's new size on the export serving it, if
// one does.
//
// Recording is the whole mechanism: the size bumps the record's generation, the
// operator re-assembles, and assembly runs xfs_growfs on the host. Nothing here
// can reach that host, and no CSI call ever targets it.
//
// It also reports whether the node has anything left to do. For a pNFS volume
// it does not: the client has an NFS mount, not the filesystem.
func growExportAfter(
	ctx context.Context, registry ExportRegistry, volumeHandle string, bytes int64,
) (nodeExpansion bool, err error) {
	if registry == nil {
		return true, nil
	}
	lvolID, ok := lvolIDOf(volumeHandle)
	if !ok {
		return true, nil
	}

	grown, err := registry.SetExportSize(ctx, exportRecordName(lvolID), bytes)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"recording the new size of volume %s on its export: %v", volumeHandle, err)
	}
	return !grown, nil
}
