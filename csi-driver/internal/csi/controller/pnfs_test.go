// Tests for the RWX provisioning path: which access modes are taken, which are
// refused, and that the identity a volume is given is derived once and agrees
// with itself.
//
// The refusals matter as much as the acceptance. A read-only or single-writer
// multi-node claim that silently got RWX behavior would hand a user a filesystem
// several nodes can write, which is neither what they asked for nor what their
// application is built for.

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func capWithMode(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
	}
}

func TestRWXIsTakenOnlyForMultiWriter(t *testing.T) {
	cases := []struct {
		name string
		mode csi.VolumeCapability_AccessMode_Mode
		rwx  bool
		errs bool
	}{
		{"single node writer stays block", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false, false},
		{"multi node multi writer is RWX", csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true, false},
		{"multi node reader only is refused", csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, false, true},
		{"multi node single writer is refused", csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rwx, err := isRWX([]*csi.VolumeCapability{capWithMode(tc.mode)})
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
				t.Fatalf("mode %s: %v", tc.mode, err)
			}
			if rwx != tc.rwx {
				t.Errorf("rwx = %v, want %v", rwx, tc.rwx)
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
		if !strings.HasPrefix(p, "/mnt/") {
			t.Errorf("path %q is not under /mnt", p)
		}
		if strings.ContainsAny(strings.TrimPrefix(p, "/mnt/"), "/ :") {
			t.Errorf("path %q carries a character an exports entry will not take", p)
		}
	}
}

// The record's name has to be a legal Kubernetes name, which the handle is not:
// it carries colons.
func TestRecordNameIsDNSSafeAndStable(t *testing.T) {
	name := exportRecordName("0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90", "pool-a",
		"3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13")

	if strings.ContainsAny(name, ":/_") {
		t.Errorf("name %q carries a character a Kubernetes name does not admit", name)
	}
	if len(name) > 253 {
		t.Errorf("name %q is too long", name)
	}
	again := exportRecordName("0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90", "pool-a",
		"3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13")
	if name != again {
		t.Error("the name is not stable across calls, so a retry would create a second record")
	}
	other := exportRecordName("0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90", "pool-b",
		"3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13")
	if name == other {
		t.Error("two pools produced the same record name")
	}
}

// Everything derived is derived once, so the handle, the record name, the mount
// point and the fsid cannot drift apart.
func TestIdentityAgreesWithItself(t *testing.T) {
	const (
		cluster = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"
		pool    = "pool-a"
		export  = "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
	)
	id := deriveIdentity(cluster, pool, export, "team-a", "shared")

	if !strings.HasPrefix(id.Handle, "nfs:") {
		t.Errorf("handle %q is not the pNFS form", id.Handle)
	}
	if !strings.Contains(id.Handle, export) {
		t.Errorf("handle %q does not name the export", id.Handle)
	}
	// The fsid is the export UUID, which is what makes it unique cluster-wide
	// and stable for the export's life without an allocator.
	if id.FSID != export {
		t.Errorf("fsid = %q, want the export UUID", id.FSID)
	}
	if id.RecordName != exportRecordName(cluster, pool, export) {
		t.Error("the record name does not match what the name helper produces")
	}
}

// While the operator is assembling, CreateVolume reports Aborted so the external
// provisioner retries rather than the RPC being held open.
func TestNotReadyIsAbortedAndDegradedIsNot(t *testing.T) {
	err := checkExportReady("nfsexp-abc", ExportRecord{Phase: "Assembling"})
	if status.Code(err) != codes.Aborted {
		t.Errorf("assembling gave %s, want Aborted", status.Code(err))
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
		ExportPath:     "/mnt/team-a-shared-3c81a0f4",
		FSID:           "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
		NGUID:          "71714b79784f4b54756f65624e495374",
	})
	for _, key := range []string{ctxAccessProtocol, ctxExportService, ctxExportPath, ctxFSID, ctxNGUID} {
		if ctx[key] == "" {
			t.Errorf("volume context is missing %s", key)
		}
	}
	if ctx[ctxAccessProtocol] != AccessProtocolNFS {
		t.Errorf("access_protocol = %q, want %q", ctx[ctxAccessProtocol], AccessProtocolNFS)
	}
}

// fakeRegistry records what provisioning and deletion asked of the record.
type fakeRegistry struct {
	record    ExportRecord
	ensured   []string
	deleted   []string
	stillGone bool
}

func (f *fakeRegistry) EnsureExport(_ context.Context, name string, _ ExportSpec) (ExportRecord, error) {
	f.ensured = append(f.ensured, name)
	return f.record, nil
}

func (f *fakeRegistry) DeleteExport(_ context.Context, name string) (bool, error) {
	f.deleted = append(f.deleted, name)
	return f.stillGone, nil
}

// Deleting an RWX claim has to take the export down with it. The record is the
// only description of a mount, an exports entry, and an attached namespace on
// the metadata-server host, so a DeleteVolume that skipped it would leave all
// three running with nothing left naming them.
func TestDeleteRWXVolumeDeletesTheExportRecord(t *testing.T) {
	const (
		cluster = "f0bb9077-78c4-4482-9ccf-a5693ce2df78"
		pool    = "9d016dd4-34d7-42f0-b549-52a5af2f1399"
		volume  = "bfc56677-d602-4017-804b-975f3b929e3f"
	)
	registry := &fakeRegistry{}
	handle := "nfs:" + cluster + ":" + pool + ":" + volume

	// The record is still there, so the call is not finished: the host teardown
	// runs behind the record's finalizer.
	_, err := deleteExportFor(context.Background(), registry, handle)
	if err == nil {
		t.Fatal("deletion reported success while the export was still being torn down")
	}
	want := exportRecordName(cluster, pool, volume)
	if len(registry.deleted) != 1 || registry.deleted[0] != want {
		t.Fatalf("deleted %v, want one delete of %s", registry.deleted, want)
	}
}

// Once the record is gone the host side is torn down, and deletion continues to
// the backing volume. The block handle is reconstructed from the same three
// fields the pNFS handle carries, because it is the same volume.
func TestDeletedExportYieldsTheBackingBlockHandle(t *testing.T) {
	const (
		cluster = "f0bb9077-78c4-4482-9ccf-a5693ce2df78"
		pool    = "9d016dd4-34d7-42f0-b549-52a5af2f1399"
		volume  = "bfc56677-d602-4017-804b-975f3b929e3f"
	)
	registry := &fakeRegistry{stillGone: true}

	backing, err := deleteExportFor(context.Background(),
		registry, "nfs:"+cluster+":"+pool+":"+volume)
	if err != nil {
		t.Fatalf("deleteExportFor: %v", err)
	}
	if want := cluster + ":" + pool + ":" + volume; backing != want {
		t.Errorf("backing handle = %q, want %q", backing, want)
	}
}

// Expanding a ReadWriteMany volume is not implemented, and says so.
//
// It is two steps on two hosts -- grow the logical volume, then xfs_growfs on
// whichever host serves the export -- and the second has no caller yet. Left
// alone the handle simply fails to parse somewhere further in, and the user
// reads "invalid volume handle" about a volume the driver created, which sends
// them looking for corruption rather than for a missing feature.
func TestExpandingAnRWXVolumeIsRefusedInItsOwnWords(t *testing.T) {
	err := refuseRWXExpansion(
		"nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f")
	if err == nil {
		t.Fatal("expanding a ReadWriteMany volume was accepted")
	}
	if !strings.Contains(err.Error(), "ReadWriteMany") {
		t.Errorf("the refusal %q does not say what kind of volume this is", err)
	}
	if strings.Contains(err.Error(), "invalid volume handle") {
		t.Errorf("the refusal reads as a malformed handle: %q", err)
	}
}

// A block volume still expands.
func TestExpandingABlockVolumeIsNotRefusedHere(t *testing.T) {
	if err := refuseRWXExpansion("f0bb9077:pool-a:bfc56677"); err != nil {
		t.Errorf("a block volume was refused: %v", err)
	}
}

// A pNFS handle names a real volume, and asking about its capabilities must not
// answer NotFound. The handle does not parse as a block one, so without a
// branch the driver reports a volume it provisioned as missing -- which reads
// as data loss to whoever asked.
func TestValidatingAnRWXVolumeDoesNotReportItMissing(t *testing.T) {
	const handle = "nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f"

	// A ReadWriteMany filesystem request is what this volume is for.
	confirmed, err := validateRWXCapabilities(handle, []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}})
	if err != nil {
		t.Fatalf("validating a ReadWriteMany volume: %v", err)
	}
	if !confirmed {
		t.Error("a ReadWriteMany volume did not confirm a ReadWriteMany capability")
	}
}

// Asking whether an RWX volume can be a raw block device is answered no, not
// confirmed: pNFS exports a filesystem, and a caller told yes would go on to
// use it as a device.
func TestValidatingAnRWXVolumeRefusesBlock(t *testing.T) {
	const handle = "nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f"

	confirmed, err := validateRWXCapabilities(handle, []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}})
	if err != nil {
		t.Fatalf("validating a block request: %v", err)
	}
	if confirmed {
		t.Error("a pNFS volume confirmed a raw block capability")
	}
}

// blockCap and mountCap build the two shapes a ReadWriteMany request arrives in.
func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}
}

func mountCap(fsType string) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}
}

// ReadWriteMany with volumeMode: Block is ordinary multi-attach: one namespace,
// several initiators, no filesystem. The block path has always served it, and
// KubeVirt live migration needs it. pNFS is not involved, so this must route to
// the block path rather than be refused -- refusing it is a regression against
// every release before pNFS existed.
func TestRWXBlockIsNotAPNFSVolume(t *testing.T) {
	rwx, err := isRWX([]*csi.VolumeCapability{blockCap()})
	if err != nil {
		t.Fatalf("RWX block was refused: %v", err)
	}
	if rwx {
		t.Error("RWX block routed to pNFS; it is plain multi-attach and belongs on the block path")
	}
}

// XFS is the only filesystem a SCSI layout can be served from, so a request for
// anything else cannot be honored. It has to be refused rather than quietly
// formatted as XFS, which is what happens when nothing checks: the user asks
// for ext4, the export is XFS, and nothing says so.
func TestRWXRefusesANonXFSFilesystem(t *testing.T) {
	if _, err := isRWX([]*csi.VolumeCapability{mountCap("ext4")}); err == nil {
		t.Error("RWX with ext4 was accepted; the export is always XFS, so this has to be refused")
	}
	for _, fsType := range []string{"", "xfs"} {
		rwx, err := isRWX([]*csi.VolumeCapability{mountCap(fsType)})
		if err != nil {
			t.Errorf("RWX with fsType %q was refused: %v", fsType, err)
		}
		if !rwx {
			t.Errorf("RWX with fsType %q did not route to pNFS", fsType)
		}
	}
}
