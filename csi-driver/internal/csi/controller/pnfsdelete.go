// Tearing an RWX volume down, which is the RWX provisioning path in reverse.
//
// The order is forced and it is the whole content of this file. The record is
// the only description of a mount, an exports entry, and an attached namespace
// on the metadata-server host; deleting the backing volume first would pull the
// namespace out from under a live filesystem, which is an EIO every process in
// it has to be killed to clear. So the record goes first, the operator's
// finalizer tears the host down, and only once the record is actually gone does
// the backing volume follow.
//
// Waiting for that is a returned error rather than a loop here: the external
// provisioner is already the retry loop, and DeleteVolume is required to be
// idempotent, so a second call resumes wherever the first one stopped.

package controller

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"
)

// deleteExportFor removes the export record for a pNFS handle and reports the
// backing volume's own handle once it is gone.
//
// The backing handle is rebuilt from the three fields the pNFS handle already
// carries rather than recorded anywhere, because it is the same volume: the
// export UUID is the logical volume's id, which is what provisioning derived
// the pNFS handle from in the first place.
func deleteExportFor(
	ctx context.Context,
	registry ExportRegistry,
	volumeHandle string,
) (string, error) {
	handle, ok := lvol.ParseNFSHandle(lvol.VolumeHandle(volumeHandle))
	if !ok {
		return "", status.Errorf(codes.InvalidArgument,
			"%q is not a pNFS volume handle", volumeHandle)
	}
	if registry == nil {
		// Without a registry the record cannot be deleted, and deleting the
		// backing volume anyway would strand the export. Refusing leaves both
		// in place, which is recoverable; the other order is not.
		return "", status.Error(codes.FailedPrecondition,
			"ReadWriteMany needs the NFSExport registry, which is not configured")
	}

	name := exportRecordName(handle.ClusterID, handle.PoolRef, handle.ExportUUID)
	gone, err := registry.DeleteExport(ctx, name)
	if err != nil {
		return "", status.Errorf(codes.Internal, "deleting export %s: %v", name, err)
	}
	if !gone {
		// Still finalizing on the host. Aborted rather than an error, so the
		// provisioner retries instead of giving up on the claim.
		return "", status.Errorf(codes.Aborted,
			"export %s is still being torn down on its host", name)
	}

	return lvol.Handle{
		ClusterID: handle.ClusterID,
		PoolRef:   handle.PoolRef,
		VolumeID:  handle.ExportUUID,
	}.String(), nil
}
