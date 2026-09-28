// What CreateVolume does when the StorageClass asks for the pnfs fsType.
//
// It creates two things and stops: the backing volume, and the NFSExport
// record saying one needs serving. It never calls a node and never picks a
// host; it waits for the record to say Ready.
//
// The volume keeps the backing volume's handle. A pNFS volume is an lvol with
// an export in front of it, so snapshot, clone, and expand all address the
// lvol without knowing the export is there.

package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const (
	// pnfsFSType is what a StorageClass sets csi.storage.k8s.io/fstype to in
	// order to ask for a pNFS export. It names a request, not an on-disk
	// format: the export itself is export.FSType, and the client mounts NFS.
	//
	// The volume-context keys this path writes are csicommon's, because the
	// node service reads them.
	pnfsFSType = "pnfs"

	// fsTypeParameter is the StorageClass parameter that carries it, named
	// only so a refusal can tell the user where to set it.
	fsTypeParameter = "csi.storage.k8s.io/fstype"
)

// isPNFSRequest reports whether the StorageClass asked for an export. The
// access mode is orthogonal: ReadWriteOnce over one is as legitimate as
// ReadWriteMany.
func isPNFSRequest(caps []*csi.VolumeCapability) (bool, error) {
	pnfs := false
	for _, c := range caps {
		if c.GetMount().GetFsType() == pnfsFSType {
			pnfs = true
		}
	}
	if !pnfs {
		// The driver must advertise MULTI_NODE_MULTI_WRITER for an RWX claim
		// to bind, and that advertisement is global. Without this refusal an
		// RWX claim on any other fsType goes down the block path: two nodes
		// mounting one filesystem with nothing coordinating them.
		for _, c := range caps {
			if c.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
				return false, status.Errorf(codes.InvalidArgument,
					"ReadWriteMany is served by a pNFS export; set %s: %q on the StorageClass",
					fsTypeParameter, pnfsFSType)
			}
		}
		return false, nil
	}
	for _, c := range caps {
		// volumeMode and the StorageClass disagree. Honoring either silently
		// picks a winner the user did not.
		if c.GetBlock() != nil {
			return false, status.Errorf(codes.InvalidArgument,
				"fsType %q asks for a pNFS export, which is a filesystem, "+
					"and volumeMode: Block asks for a raw device", pnfsFSType)
		}
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
			return false, status.Errorf(codes.InvalidArgument,
				"access mode %s is not supported; use ReadWriteOnce or ReadWriteMany",
				c.GetAccessMode().GetMode())
		}
	}
	return true, nil
}

// exportRootDir is where every export is mounted on its host, and must match
// the operator's exportRoot: the operator mounts the directory into the node
// plugin, and a path outside it would be one the host never sees.
const exportRootDir = "/var/lib/simplyblock/exports"

// exportPathFor builds the server-side mount point. It carries the namespace
// and a suffix of the volume id, because a PVC name is unique only within a
// namespace and two "data" claims must not collide on one host.
func exportPathFor(namespace, pvcName, exportUUID string) string {
	suffix := exportUUID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	parts := []string{namespace, pvcName, suffix}
	name := strings.Join(parts, "-")
	return exportRootDir + "/" + sanitizePathSegment(name)
}

// sanitizePathSegment keeps a segment to what a mount point and an exports
// entry both tolerate. Anything else becomes a dash rather than being dropped,
// so two names differing only in punctuation do not collide.
func sanitizePathSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// exportRecordName is the record's Kubernetes name. The handle cannot be it:
// handles carry colons. The UUID alone is unique, so the cluster and pool add
// nothing.
func exportRecordName(lvolID string) string {
	return "nfsexp-" + lvolID
}

// ExportRecord is what the controller reads back. Not the API type: the driver
// has no reason to depend on the operator's module.
type ExportRecord struct {
	Phase          string
	ServiceAddress string
	ExportPath     string
	Message        string
}

// ExportRegistry is how the controller creates and reads export records. An
// interface, so provisioning is testable without a cluster.
type ExportRegistry interface {
	// EnsureExport is create-or-fetch: the provisioner retries CreateVolume,
	// and a retry must not leave a second export.
	EnsureExport(ctx context.Context, name string, spec ExportSpec) (ExportRecord, error)

	// DeleteExport removes the record and reports whether it is gone, which it
	// is not the moment it is deleted: the operator holds a finalizer while it
	// tears the host down.
	DeleteExport(ctx context.Context, name string) (gone bool, err error)

	// SetExportSize records a volume's new capacity on the export serving it,
	// and reports whether there was one. The write bumps the record's
	// generation, which is what makes the operator re-assemble and grow the
	// filesystem on its host.
	SetExportSize(ctx context.Context, name string, bytes int64) (found bool, err error)
}

// ExportSpec is the record's whole spec. The namespace UUID and the fsid are
// not here: both are the volume id VolumeRef already carries.
type ExportSpec struct {
	Namespace  string
	VolumeRef  string
	ExportPath string
	Encrypted  bool
}

// volumeContextFor is what the node needs to mount. Written once rather than
// re-derived at stage time, so the two cannot disagree about where it is.
func volumeContextFor(record ExportRecord) map[string]string {
	return map[string]string{
		csicommon.CtxAccessProtocol: csicommon.AccessProtocolNFS,
		csicommon.CtxExportService:  record.ServiceAddress,
		csicommon.CtxExportPath:     record.ExportPath,
	}
}

// errExportNotReady lets the provisioner retry rather than holding the RPC
// open across an assembly. Unavailable, not Aborted: the spec reserves Aborted
// for a duplicate in-flight call, and nothing here is duplicated.
func errExportNotReady(name, phase, message string) error {
	if message != "" {
		return status.Errorf(codes.Unavailable,
			"export %s is %s: %s", name, phase, message)
	}
	return status.Errorf(codes.Unavailable, "export %s is %s", name, phase)
}

// errExportDegraded is returned when the operator has given up. A degraded
// export needs a human, so this is not an Aborted to retry forever.
func errExportDegraded(name, message string) error {
	return status.Errorf(codes.FailedPrecondition,
		"export %s is degraded and will not assemble without intervention: %s", name, message)
}

// checkExportReady turns a record's phase into what CreateVolume should do.
func checkExportReady(name string, record ExportRecord) error {
	switch record.Phase {
	case "Ready":
		return nil
	case "Degraded":
		return errExportDegraded(name, record.Message)
	default:
		return errExportNotReady(name, phaseOrPending(record.Phase), record.Message)
	}
}

func phaseOrPending(phase string) string {
	if phase == "" {
		return "Pending"
	}
	return phase
}

// ensureExportRecord creates or fetches the record for a pNFS volume.
func (cs *Server) ensureExportRecord(
	ctx context.Context,
	registry ExportRegistry,
	spec ExportSpec,
	name string,
) (ExportRecord, error) {
	if registry == nil {
		// A backing volume with nothing to serve it is worse than a refused
		// claim.
		return ExportRecord{}, status.Error(codes.FailedPrecondition,
			"a pNFS volume needs the NFSExport registry, which is not configured")
	}
	record, err := registry.EnsureExport(ctx, name, spec)
	if err != nil {
		return ExportRecord{}, fmt.Errorf("recording export %s: %w", name, err)
	}
	return record, nil
}

// createPNFSVolume records the export and waits for the operator to serve it.
// The backing volume is created first: an export with nothing behind it
// describes nothing.
func (cs *Server) createPNFSVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	csiVolume *csi.Volume,
) (*csi.CreateVolumeResponse, error) {
	params := req.GetParameters()
	namespace := params[csicommon.CSIStorageNamespaceKey]
	pvcName := params[csicommon.CSIStorageNameKey]
	if namespace == "" || pvcName == "" {
		// From the provisioner's --extra-create-metadata. Without them two
		// same-named claims collide on one host.
		return nil, status.Error(codes.InvalidArgument,
			"a pNFS volume needs the PVC name and namespace; enable --extra-create-metadata on the provisioner")
	}

	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(csiVolume.GetVolumeId()))
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"the backing volume handle %q is not well formed", csiVolume.GetVolumeId())
	}

	// The export is keyed by the backing volume, and the claim keeps that
	// volume's own handle: a pNFS volume is an lvol with an export in front of
	// it, not a second kind of volume with an identity of its own.
	name := exportRecordName(handle.VolumeID)
	record, err := cs.ensureExportRecord(ctx, cs.exports, ExportSpec{
		Namespace:  namespace,
		VolumeRef:  csiVolume.GetVolumeId(),
		ExportPath: exportPathFor(namespace, pvcName, handle.VolumeID),
		// The host needs it to tell an empty encrypted volume, which reads as
		// pseudo-random plaintext, from one carrying somebody else's data.
		Encrypted: csiVolume.GetVolumeContext()[csicommon.ParamEncryption] == "true",
	}, name)
	if err != nil {
		return nil, err
	}
	if err := checkExportReady(name, record); err != nil {
		return nil, err
	}

	volume := &csi.Volume{
		VolumeId:      csiVolume.GetVolumeId(),
		CapacityBytes: csiVolume.GetCapacityBytes(),
		VolumeContext: volumeContextFor(record),
	}
	if csiVolume.GetAccessibleTopology() != nil {
		volume.AccessibleTopology = csiVolume.GetAccessibleTopology()
	}
	return &csi.CreateVolumeResponse{Volume: volume}, nil
}
