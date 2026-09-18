// The RWX provisioning path: what CreateVolume does differently when the claim
// asks for MULTI_NODE_MULTI_WRITER.
//
// The controller creates two things and stops: the backing volume, and the
// NFSExport record saying one exists and needs serving. Everything after that
// is the operator's, which is what keeps provisioning out of the failover path.
//
// So it never calls a node and never picks a host. It waits for the record to
// say Ready and reports Aborted until then, the existing convention for
// asynchronous work behind a CSI call.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// AccessProtocolNFS is the VolumeContext key that tells the node plugin this is
// a pNFS volume rather than a block one. The node branches on it at stage time.
const AccessProtocolNFS = "nfs"

// VolumeContext keys the node needs to mount a pNFS volume. They are written by
// this path and read by NodeStageVolume, and nothing else produces them.
const (
	ctxAccessProtocol = "access_protocol"
	ctxExportService  = "export_service"
	ctxExportPath     = "export_path"
	ctxFSID           = "fsid"
	ctxNGUID          = "nguid"
)

// exportNameHashLength is how much of the handle digest the record's name
// carries. A Kubernetes name may be 253 characters, so the limit is not the
// constraint: 20 hex characters is 80 bits, which makes a collision across a
// cluster's worth of volumes not worth designing for, while keeping the name
// short enough to read in a kubectl listing.
const exportNameHashLength = 20

// isRWX reports whether the request asks for a shared filesystem, and refuses
// the multi-node modes this design does not implement.
//
// The other MULTI_NODE_* modes are rejected rather than routed. A read-only or
// single-writer multi-node claim silently getting RWX behavior would hand a
// user a filesystem several nodes can write, which is not what they asked for
// and not what their application expects.
func isRWX(caps []*csi.VolumeCapability) (bool, error) {
	rwx := false
	for _, c := range caps {
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			rwx = true
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
			return false, status.Errorf(codes.InvalidArgument,
				"access mode %s is not supported; use ReadWriteOnce or ReadWriteMany",
				c.GetAccessMode().GetMode())
		}
		// A block volume cannot be shared through a filesystem export, and
		// silently giving one a filesystem would be worse than refusing.
		if rwx && c.GetBlock() != nil {
			return false, status.Error(codes.InvalidArgument,
				"ReadWriteMany is filesystem-mode only; pNFS exports a filesystem, not a raw device")
		}
	}
	return rwx, nil
}

// exportPathFor builds the server-side mount point.
//
// It carries the namespace and a suffix of the volume id because a PVC name is
// unique only within a namespace: two claims called "data" in different
// namespaces would otherwise collide on one MDS host, in the mount point and in
// the exports file at once.
func exportPathFor(namespace, pvcName, exportUUID string) string {
	suffix := exportUUID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	parts := []string{namespace, pvcName, suffix}
	name := strings.Join(parts, "-")
	return "/mnt/" + sanitizePathSegment(name)
}

// sanitizePathSegment keeps a path segment to what a mount point and an exports
// entry both tolerate. Anything else becomes a dash rather than being dropped,
// so two names that differ only in punctuation do not collide.
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

// exportRecordName is the Kubernetes name of the record for a handle.
//
// The handle itself cannot be the name: it contains colons, which a Kubernetes
// object name does not admit. It is a digest rather than a sanitized handle so
// that two handles differing only in a character the sanitizer would flatten
// cannot land on one name.
func exportRecordName(clusterID, poolRef, exportUUID string) string {
	sum := sha256.Sum256([]byte(clusterID + "/" + poolRef + "/" + exportUUID))
	return "nfsexp-" + hex.EncodeToString(sum[:])[:exportNameHashLength]
}

// ExportRecord is the subset of an NFSExport the controller reads back. It is
// not the API type: the CSI driver has no reason to depend on the operator's
// module, and what it needs is three fields and a phase.
type ExportRecord struct {
	Phase          string
	ServiceAddress string
	ExportPath     string
	FSID           string
	NGUID          string
	Message        string
}

// ExportRegistry is how the controller creates and reads export records. It is
// an interface so the provisioning path is testable without a cluster, and so
// the CSI driver depends on the shape of the record rather than on the
// operator's types.
type ExportRegistry interface {
	// EnsureExport creates the record if it is absent and returns it either
	// way. It is create-or-fetch rather than create because the external
	// provisioner retries CreateVolume, and a retry must not leave a second
	// export behind.
	EnsureExport(ctx context.Context, name string, spec ExportSpec) (ExportRecord, error)

	// DeleteExport removes the record and reports whether it is gone. It is
	// not gone the moment it is deleted: the operator holds a finalizer while
	// it unmounts, unpublishes, and detaches on the host, and the backing
	// volume must not be deleted until that has finished.
	//
	// The record is addressed by name alone, without a namespace, because
	// DeleteVolume is given a volume handle and nothing else -- not the claim
	// it belonged to, and not the namespace it was in. The name is a digest of
	// the volume's identity and so is unique across the cluster.
	DeleteExport(ctx context.Context, name string) (gone bool, err error)
}

// ExportSpec is what the controller asks for when it creates a record. Every
// field is immutable on the record, so all of them are known here: the record
// cannot be filled in later.
type ExportSpec struct {
	Namespace  string
	VolumeRef  string
	ExportPath string
	FSID       string
	LVolID     string
	NGUID      string
}

// pnfsIdentity is everything about an RWX volume that is derived rather than
// observed, computed in one place so the handle, the record's name, the mount
// point, and the fsid cannot disagree with each other.
type pnfsIdentity struct {
	ExportUUID string
	Handle     string
	RecordName string
	ExportPath string
	FSID       string
}

// deriveIdentity computes the identity of a new RWX volume.
//
// The export UUID is the volume's own id, not the backing volume's: striping
// would give an RWX volume several backing volumes, and a handle built on one of
// them would change identity the moment that happened.
//
// The fsid is that same UUID. exports(5) accepts one, so the value is unique
// cluster-wide and stable by construction, with no allocator to run.
func deriveIdentity(clusterID, poolRef, exportUUID, namespace, pvcName string) pnfsIdentity {
	return pnfsIdentity{
		ExportUUID: exportUUID,
		Handle:     string(lvol.NewNFSVolumeHandle(clusterID, poolRef, exportUUID)),
		RecordName: exportRecordName(clusterID, poolRef, exportUUID),
		ExportPath: exportPathFor(namespace, pvcName, exportUUID),
		FSID:       exportUUID,
	}
}

// volumeContextFor is what the node plugin needs to mount the export. It is
// written once here rather than re-derived at stage time, so that a node and
// the operator cannot disagree about where the export is.
func volumeContextFor(record ExportRecord) map[string]string {
	return map[string]string{
		ctxAccessProtocol: AccessProtocolNFS,
		ctxExportService:  record.ServiceAddress,
		ctxExportPath:     record.ExportPath,
		ctxFSID:           record.FSID,
		ctxNGUID:          record.NGUID,
	}
}

// errExportNotReady is returned while the operator is still assembling. It is
// Aborted rather than an error so the external provisioner retries the call
// instead of the RPC being held open across an assembly.
func errExportNotReady(name, phase, message string) error {
	if message != "" {
		return status.Errorf(codes.Aborted,
			"export %s is %s: %s", name, phase, message)
	}
	return status.Errorf(codes.Aborted, "export %s is %s", name, phase)
}

// errExportDegraded is returned when the operator has given up. Retrying it
// forever would be noise: a degraded export needs a human, and saying so is
// more useful than an Aborted the provisioner will keep re-sending.
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

// ensureExportRecord creates or fetches the record for an RWX volume.
func (cs *Server) ensureExportRecord(
	ctx context.Context,
	registry ExportRegistry,
	identity pnfsIdentity,
	namespace, lvolID, nguid string,
) (ExportRecord, error) {
	if registry == nil {
		// Without a registry the controller cannot record that an export is
		// needed, and a backing volume with nothing to serve it is worse than a
		// refused claim.
		return ExportRecord{}, status.Error(codes.FailedPrecondition,
			"ReadWriteMany needs the NFSExport registry, which is not configured")
	}
	record, err := registry.EnsureExport(ctx, identity.RecordName, ExportSpec{
		Namespace:  namespace,
		VolumeRef:  identity.Handle,
		ExportPath: identity.ExportPath,
		FSID:       identity.FSID,
		LVolID:     lvolID,
		NGUID:      nguid,
	})
	if err != nil {
		return ExportRecord{}, fmt.Errorf("recording export %s: %w", identity.RecordName, err)
	}
	return record, nil
}

// createRWXVolume finishes provisioning an RWX volume once its backing volume
// exists: it records the export and waits for the operator to serve it.
//
// The backing volume is created first and deliberately. An export with no volume
// behind it is a record describing nothing, while a volume with no export yet is
// exactly the state the record is about to describe, and the record's name is
// derived rather than generated so a retry addresses the same object.
func (cs *Server) createRWXVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	csiVolume *csi.Volume,
	clusterID string,
) (*csi.CreateVolumeResponse, error) {
	params := req.GetParameters()
	namespace := params[csicommon.CSIStorageNamespaceKey]
	pvcName := params[csicommon.CSIStorageNameKey]
	if namespace == "" || pvcName == "" {
		// Both arrive from the external provisioner's --extra-create-metadata.
		// Without them two claims of the same name in different namespaces
		// would collide on one MDS host, so this is refused rather than guessed.
		return nil, status.Error(codes.InvalidArgument,
			"ReadWriteMany needs the PVC name and namespace; enable --extra-create-metadata on the provisioner")
	}

	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(csiVolume.GetVolumeId()))
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"the backing volume handle %q is not well formed", csiVolume.GetVolumeId())
	}

	identity := deriveIdentity(clusterID, handle.PoolRef, handle.VolumeID, namespace, pvcName)

	// The record carries the logical volume's id, which is what the MDS host
	// resolves the local device by: for a simplyblock volume the namespace UUID
	// is the volume's own id. The NGUID is deliberately not set here -- it is
	// assigned by the target and read back from the device, so only a host with
	// the namespace attached can know it, and the client reads it from sysfs
	// when it builds the device alias.
	record, err := cs.ensureExportRecord(ctx, cs.exports, identity, namespace, handle.VolumeID, "")
	if err != nil {
		return nil, err
	}
	if err := checkExportReady(identity.RecordName, record); err != nil {
		return nil, err
	}

	volume := &csi.Volume{
		VolumeId:      identity.Handle,
		CapacityBytes: csiVolume.GetCapacityBytes(),
		VolumeContext: volumeContextFor(record),
	}
	if csiVolume.GetAccessibleTopology() != nil {
		volume.AccessibleTopology = csiVolume.GetAccessibleTopology()
	}
	return &csi.CreateVolumeResponse{Volume: volume}, nil
}
