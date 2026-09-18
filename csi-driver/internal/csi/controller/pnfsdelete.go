// Tearing a pNFS volume down, which is the pNFS provisioning path in reverse.
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

	"github.com/container-storage-interface/spec/lib/go/csi"
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
			"a pNFS volume needs the NFSExport registry, which is not configured")
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

// refusePNFSExpansion turns away an expand of a pNFS volume, in its own words.
//
// Growing one is two steps on two hosts: grow the logical volume, and then run
// xfs_growfs on whichever host currently serves the export. The second has no
// caller -- the operator drives the host over the link and nothing asks it to
// resize -- so the feature is absent rather than broken.
//
// Saying so matters because the alternative is not an error about expansion at
// all: the pNFS handle does not parse as a block one, and the user reads
// "invalid volume handle" about a volume this driver created, which reads like
// corruption and sends them looking in the wrong place.
func refusePNFSExpansion(volumeHandle string) error {
	if !lvol.VolumeHandle(volumeHandle).IsNFS() {
		return nil
	}
	return status.Error(codes.Unimplemented,
		"a pNFS volume cannot be expanded yet: growing one means growing the volume "+
			"and then the filesystem on the host serving the export, and the second half is not built")
}

// validatePNFSCapabilities answers ValidateVolumeCapabilities for a pNFS volume.
//
// It cannot go through the control plane the way a block volume does: the
// handle names an export rather than a logical volume, so the lookup would come
// back not-found and the driver would report a volume it provisioned as
// missing, which reads as data loss to whoever asked.
//
// What can be answered locally is everything that matters. A pNFS volume is a
// filesystem, served to one writer or to many, so the question is whether the
// caller is asking for a filesystem and for a mode an export can serve.
func validatePNFSCapabilities(volumeHandle string, caps []*csi.VolumeCapability) (bool, error) {
	if _, ok := lvol.ParseNFSHandle(lvol.VolumeHandle(volumeHandle)); !ok {
		return false, status.Errorf(codes.NotFound, "volume %q not found", volumeHandle)
	}
	for _, c := range caps {
		if c.GetBlock() != nil {
			// Not a failure: the caller asked whether an export can be a raw
			// device, and the answer is that it cannot.
			return false, nil
		}
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		default:
			return false, nil
		}
	}
	return true, nil
}
