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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func capWithMode(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
	}
}

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
		{"xfs many writers stays block", "xfs", csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false, false},
		{"no fsType stays block", "", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false, false},
		{"reader only is refused", pnfsFSType, csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, false, true},
		{"single writer across nodes is refused", pnfsFSType, csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER, false, true},
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
func TestExpandingAPNFSVolumeIsRefusedInItsOwnWords(t *testing.T) {
	err := refusePNFSExpansion(
		"nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f")
	if err == nil {
		t.Fatal("expanding a pNFS volume was accepted")
	}
	if !strings.Contains(err.Error(), "pNFS") {
		t.Errorf("the refusal %q does not say what kind of volume this is", err)
	}
	if strings.Contains(err.Error(), "invalid volume handle") {
		t.Errorf("the refusal reads as a malformed handle: %q", err)
	}
}

// A block volume still expands.
func TestExpandingABlockVolumeIsNotRefusedHere(t *testing.T) {
	if err := refusePNFSExpansion("f0bb9077:pool-a:bfc56677"); err != nil {
		t.Errorf("a block volume was refused: %v", err)
	}
}

// A pNFS handle names a real volume, and asking about its capabilities must not
// answer NotFound. The handle does not parse as a block one, so without a
// branch the driver reports a volume it provisioned as missing -- which reads
// as data loss to whoever asked.
func TestValidatingAPNFSVolumeDoesNotReportItMissing(t *testing.T) {
	const handle = "nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f"

	// A ReadWriteMany filesystem request is what this volume is for.
	confirmed, err := validatePNFSCapabilities(handle, []*csi.VolumeCapability{{
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

// Asking whether a pNFS volume can be a raw block device is answered no, not
// confirmed: pNFS exports a filesystem, and a caller told yes would go on to
// use it as a device.
func TestValidatingAPNFSVolumeRefusesBlock(t *testing.T) {
	const handle = "nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f"

	confirmed, err := validatePNFSCapabilities(handle, []*csi.VolumeCapability{{
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

// ReadWriteMany on an ordinary filesystem is not this path's business. It is
// what every release before pNFS did, and KubeVirt live migration needs the
// raw-block form of it, so neither is refused here.
func TestRWXWithoutPNFSIsLeftAlone(t *testing.T) {
	for _, c := range []*csi.VolumeCapability{
		mountCapMode("xfs", csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		pnfsBlockCap(),
	} {
		pnfs, err := isPNFSRequest([]*csi.VolumeCapability{c})
		if err != nil {
			t.Errorf("a ReadWriteMany claim was refused: %v", err)
		}
		if pnfs {
			t.Error("a ReadWriteMany claim was routed to pNFS without asking for it")
		}
	}
}
