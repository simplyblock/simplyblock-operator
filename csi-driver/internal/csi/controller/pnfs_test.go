// Tests for the pNFS provisioning path: which requests take it, which are
// refused, and that the identity a volume is given is derived once and agrees
// with itself.
//
// The routing is on fsType, so most of these are about what does *not* take the
// path. A claim that asked for a block volume and silently got an NFS export
// would be a different product than the one its StorageClass named.

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPNFSIsTakenOnlyForThePNFSFSType(t *testing.T) {
	cases := []struct {
		name   string
		fsType string
		mode   csi.VolumeCapability_AccessMode_Mode
		pnfs   bool
		errs   bool
	}{
		{"pnfs with one writer", pnfsFSType, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true, false},
		{"pnfs with many writers", pnfsFSType, csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true, false},
		{"xfs stays block", "xfs", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false, false},
		{"xfs many writers is refused", "xfs", csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false, true},
		{"no fsType stays block", "", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false, false},
		{"reader only is refused", pnfsFSType, csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, false, true},
		{"single writer across nodes is refused", pnfsFSType,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pnfs, err := isPNFSRequest([]*csi.VolumeCapability{mountCapMode(tc.fsType, tc.mode)})
			if tc.errs {
				if err == nil {
					t.Fatalf("mode %s was accepted", tc.mode)
				}
				if status.Code(err) != codes.InvalidArgument {
					t.Errorf("code = %s, want InvalidArgument", status.Code(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("fsType %q, mode %s: %v", tc.fsType, tc.mode, err)
			}
			if pnfs != tc.pnfs {
				t.Errorf("pnfs = %v, want %v", pnfs, tc.pnfs)
			}
		})
	}
}

// Two claims with the same name in different namespaces must not collide on one
// MDS host, in the mount point or in the exports file.
func TestExportPathSeparatesNamespaces(t *testing.T) {
	a := exportPathFor("team-a", "data", "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13")
	b := exportPathFor("team-b", "data", "7f22b1e5-2c3d-4f88-ab12-6e7d9c0f1a24")
	if a == b {
		t.Fatalf("two namespaces produced the same path: %s", a)
	}
	for _, p := range []string{a, b} {
		if !strings.HasPrefix(p, exportRootDir+"/") {
			t.Errorf("path %q is not under %s", p, exportRootDir)
		}
		// The segment under the root is one component and takes no character
		// an exports(5) entry would read as a separator.
		if strings.ContainsAny(strings.TrimPrefix(p, exportRootDir+"/"), "/ :") {
			t.Errorf("path %q carries a character an exports entry will not take", p)
		}
	}
}

// The record's name has to be a legal Kubernetes name, which the handle is not:
// it carries colons. It is also what a retried CreateVolume and a later
// DeleteVolume each derive independently, so it has to be the same bytes every
// time and different for every volume.
func TestRecordNameIsDNSSafeAndStable(t *testing.T) {
	const (
		one = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
		two = "7f22b1e5-2c3d-4f88-ab12-6e7d9c0f1a24"
	)
	name := exportRecordName(one)

	if strings.ContainsAny(name, ":/_") {
		t.Errorf("name %q carries a character a Kubernetes name does not admit", name)
	}
	if len(name) > 253 {
		t.Errorf("name %q is too long", name)
	}
	if name != exportRecordName(one) {
		t.Error("the name is not stable across calls, so a retry would create a second record")
	}
	if name == exportRecordName(two) {
		t.Error("two volumes produced the same record name")
	}
}

// A pNFS volume keeps its backing volume's handle, and the export record is
// named after that volume.
//
// This is the property the whole design turns on: a snapshot, a clone, an
// expand, and a capability check all address the lvol, so none of them has to
// know an export is in front of it. An identity of its own is what the earlier
// nfs: handle gave, at the cost of five call sites that forgot to branch.
func TestTheRecordIsNamedAfterTheBackingVolume(t *testing.T) {
	const volume = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"

	name := exportRecordName(volume)
	if !strings.Contains(name, volume) {
		t.Errorf("record name %q does not name the volume it serves", name)
	}
	if strings.HasPrefix(name, "nfs:") {
		t.Error("the record name carries a handle prefix")
	}
}

// While the operator is assembling, CreateVolume reports a retryable code so
// the external provisioner comes back rather than the RPC being held open.
func TestNotReadyIsUnavailableAndDegradedIsNot(t *testing.T) {
	err := checkExportReady("nfsexp-abc", ExportRecord{Phase: "Assembling"})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("assembling gave %s, want Unavailable", status.Code(err))
	}
	// Aborted is the CSI spec's guard against a duplicate in-flight call for
	// one volume, which is not what an export still assembling is.
	if status.Code(err) == codes.Aborted {
		t.Error("an export still assembling reports a duplicate in-flight call")
	}

	if err := checkExportReady("nfsexp-abc", ExportRecord{Phase: "Ready"}); err != nil {
		t.Errorf("Ready gave an error: %v", err)
	}

	// Degraded needs a human. Retrying it forever would be noise.
	err = checkExportReady("nfsexp-abc", ExportRecord{Phase: "Degraded", Message: "assembly timed out"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("degraded gave %s, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "assembly timed out") {
		t.Errorf("the degraded error does not say why: %v", err)
	}
}

// The node cannot mount without these, and re-deriving them at stage time is
// how a node and the operator end up disagreeing about where the export is.
func TestVolumeContextCarriesWhatTheNodeNeeds(t *testing.T) {
	ctx := volumeContextFor(ExportRecord{
		ServiceAddress: "10.43.199.218",
		ExportPath:     "/var/lib/simplyblock/exports/team-a-shared-3c81a0f4",
	})
	for _, key := range []string{
		csicommon.CtxAccessProtocol, csicommon.CtxExportService, csicommon.CtxExportPath,
	} {
		if ctx[key] == "" {
			t.Errorf("volume context is missing %s", key)
		}
	}
	if ctx[csicommon.CtxAccessProtocol] != csicommon.AccessProtocolNFS {
		t.Errorf("access_protocol = %q, want %q",
			ctx[csicommon.CtxAccessProtocol], csicommon.AccessProtocolNFS)
	}
}

// fakeRegistry records what provisioning and deletion asked of the record.
type fakeRegistry struct {
	record    ExportRecord
	ensured   []string
	deleted   []string
	stillGone bool
	exists    bool
	sized     []string
	size      int64
}

func (f *fakeRegistry) EnsureExport(_ context.Context, name string, _ ExportSpec) (ExportRecord, error) {
	f.ensured = append(f.ensured, name)
	return f.record, nil
}

func (f *fakeRegistry) DeleteExport(_ context.Context, name string) (bool, error) {
	f.deleted = append(f.deleted, name)
	return f.stillGone, nil
}

func (f *fakeRegistry) SetExportSize(_ context.Context, name string, bytes int64) (bool, error) {
	if !f.exists {
		return false, nil
	}
	f.sized = append(f.sized, name)
	f.size = bytes
	return true, nil
}

// Deleting a claim has to take its export down first. The record is the only
// description of a mount, an exports entry, and an attached namespace on the
// host, so deleting the volume under them is an EIO every process in the
// filesystem must be killed to clear.
func TestDeleteTakesTheExportDownFirst(t *testing.T) {
	const handle = "f0bb9077-78c4-4482-9ccf-a5693ce2df78:" +
		"9d016dd4-34d7-42f0-b549-52a5af2f1399:bfc56677-d602-4017-804b-975f3b929e3f"
	const volume = "bfc56677-d602-4017-804b-975f3b929e3f"

	// The record is still there, so the call is not finished: the host teardown
	// runs behind the record's finalizer.
	registry := &fakeRegistry{}
	if err := deleteExportBefore(context.Background(), registry, handle); err == nil {
		t.Fatal("deletion reported success while the export was still being torn down")
	}
	want := exportRecordName(volume)
	if len(registry.deleted) != 1 || registry.deleted[0] != want {
		t.Fatalf("deleted %v, want one delete of %s", registry.deleted, want)
	}

	// Once it is gone, deletion continues to the volume itself.
	if err := deleteExportBefore(context.Background(),
		&fakeRegistry{stillGone: true}, handle); err != nil {
		t.Errorf("deleteExportBefore: %v", err)
	}
}

// A block volume takes the same path and finds nothing, which is the point:
// nothing has to know in advance which kind of volume it is holding.
func TestDeletingAVolumeWithNoExportIsNotAnError(t *testing.T) {
	const handle = "f0bb9077-78c4-4482-9ccf-a5693ce2df78:" +
		"9d016dd4-34d7-42f0-b549-52a5af2f1399:bfc56677-d602-4017-804b-975f3b929e3f"

	if err := deleteExportBefore(context.Background(),
		&fakeRegistry{stillGone: true}, handle); err != nil {
		t.Errorf("deleting a volume with no export was refused: %v", err)
	}
}

// Expanding a volume an export serves records the new size on the record.
//
// That write is the whole mechanism: it bumps the record's generation, the
// operator re-assembles, and assembly runs xfs_growfs on the host serving the
// export -- the one machine that can, and the one no CSI call ever reaches.
func TestExpandingAnExportedVolumeRecordsTheNewSize(t *testing.T) {
	const handle = "f0bb9077-78c4-4482-9ccf-a5693ce2df78:" +
		"9d016dd4-34d7-42f0-b549-52a5af2f1399:bfc56677-d602-4017-804b-975f3b929e3f"
	const volume = "bfc56677-d602-4017-804b-975f3b929e3f"

	registry := &fakeRegistry{exists: true}
	if err := growExportAfter(context.Background(), registry, handle, 8<<30); err != nil {
		t.Fatalf("growExportAfter: %v", err)
	}

	if want := exportRecordName(volume); len(registry.sized) != 1 || registry.sized[0] != want {
		t.Fatalf("sized %v, want one write to %s", registry.sized, want)
	}
	if registry.size != 8<<30 {
		t.Errorf("recorded %d bytes, want the new capacity", registry.size)
	}
}

// A volume with no export expands as it always did, and the node still grows
// its own filesystem.
func TestExpandingAVolumeWithNoExportLeavesTheNodeToIt(t *testing.T) {
	const handle = "f0bb9077-78c4-4482-9ccf-a5693ce2df78:" +
		"9d016dd4-34d7-42f0-b549-52a5af2f1399:bfc56677-d602-4017-804b-975f3b929e3f"

	if err := growExportAfter(context.Background(), &fakeRegistry{}, handle, 8<<30); err != nil {
		t.Fatalf("growExportAfter on a volume with no export: %v", err)
	}
}

// mountCapMode and pnfsBlockCap build the shapes a request arrives in.
func mountCapMode(fsType string, mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func pnfsBlockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}
}

// A raw-block claim has no filesystem to ask for, so fsType cannot select pNFS
// for one. Reaching here means volumeMode and the StorageClass disagree, and
// the claim is refused rather than quietly given one or the other.
func TestPNFSRefusesARawBlockClaim(t *testing.T) {
	caps := []*csi.VolumeCapability{pnfsBlockCap(), mountCapMode(pnfsFSType,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}
	if _, err := isPNFSRequest(caps); err == nil {
		t.Error("a raw-block claim asking for pNFS was accepted")
	}
}

// ReadWriteMany is served by an export and by nothing else. The driver has to
// advertise MULTI_NODE_MULTI_WRITER for an RWX claim to bind at all, and that
// advertisement is global, so without this refusal an RWX claim whose
// StorageClass asks for ext4 is accepted and provisioned down the block path,
// which is two nodes mounting one ext4 with no coordination between them.
//
// The fsType is the switch, and the access mode does not matter: RWO over an
// export is equally legitimate and takes the same path.
func TestReadWriteManyWithoutPNFSIsRefused(t *testing.T) {
	ext4 := []*csi.VolumeCapability{
		mountCapMode("ext4", csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	}
	if _, err := isPNFSRequest(ext4); err == nil {
		t.Error("a ReadWriteMany ext4 claim was accepted, so two nodes will mount one filesystem")
	}

	// A raw-block ReadWriteMany is multi-attach, which is a different feature
	// with a different failure mode. It is refused here too, for now.
	block := []*csi.VolumeCapability{pnfsBlockCap()}
	if _, err := isPNFSRequest(block); err == nil {
		t.Error("a raw-block ReadWriteMany claim was accepted")
	}
}

// ReadWriteOnce over an export is ordinary: the fsType selects pNFS, and the
// access mode is orthogonal to it.
func TestReadWriteOnceOverPNFSIsAccepted(t *testing.T) {
	caps := []*csi.VolumeCapability{
		mountCapMode(pnfsFSType, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	pnfs, err := isPNFSRequest(caps)
	if err != nil {
		t.Fatalf("a ReadWriteOnce pNFS claim was refused: %v", err)
	}
	if !pnfs {
		t.Error("a ReadWriteOnce claim asking for pnfs did not take the export path")
	}
}

// Creating a pNFS volume from a data source is refused: the clone carries the
// source's filesystem, so assembly would find it non-blank, skip the mkfs, and
// mount it as XFS -- which it may not be.
func TestCloningIntoAPNFSVolumeIsRefused(t *testing.T) {
	err := refusePNFSCloneTarget(true)
	if err == nil {
		t.Fatal("a pNFS claim with a data source was accepted")
	}
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code = %s, want Unimplemented", status.Code(err))
	}
	if refusePNFSCloneTarget(false) != nil {
		t.Error("cloning into a block volume was refused")
	}
}
