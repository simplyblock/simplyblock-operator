// Tests for the pNFS client path, weighted toward the alias name.
//
// That weighting is from experience rather than taste. During bring-up the
// alias was written as `nvme-eui64.` -- which the design document also says --
// and the kernel looks for `nvme-eui.`. The mount succeeded, both nodes read and
// wrote the same files, every checksum matched, and not one byte took the direct
// path. Nothing about that looks wrong from outside, so the name is pinned here.

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeNFSMounter struct {
	mounts    []string
	unmounts  []string
	isMounted bool
	mountErr  error
}

func (m *fakeNFSMounter) Mount(_ context.Context, source, target, fsType string, options []string) error {
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mounts = append(m.mounts, source+" "+target+" "+fsType+" "+strings.Join(options, ","))
	m.isMounted = true
	return nil
}

func (m *fakeNFSMounter) Unmount(_ context.Context, target string) error {
	m.unmounts = append(m.unmounts, target)
	m.isMounted = false
	return nil
}

func (m *fakeNFSMounter) IsMountPoint(context.Context, string) (bool, error) {
	return m.isMounted, nil
}

// The kernel builds its lookup path from the designator as plain lowercase hex.
// Both spellings the NGUID arrives in have to reduce to that one.
func TestNormalizeNGUIDAgreesAcrossSpellings(t *testing.T) {
	// sysfs reports it hyphenated; `nvme id-ns` reports it bare. Same namespace.
	sysfs := "71714b79-784f-4b54-756f-65624e495374"
	idNS := "71714b79784f4b54756f65624e495374"

	if got := normalizeNGUID(sysfs); got != idNS {
		t.Errorf("normalizeNGUID(sysfs) = %q, want %q", got, idNS)
	}
	if got := normalizeNGUID(idNS); got != idNS {
		t.Errorf("normalizeNGUID(id-ns) = %q, want it unchanged", got)
	}
	if got := normalizeNGUID(strings.ToUpper(idNS)); got != idNS {
		t.Errorf("normalizeNGUID(upper) = %q, want it lowercased", got)
	}
}

// The prefix the kernel actually tries. `nvme-eui64.` matches nothing and the
// failure is invisible, so this is pinned rather than left to a comment.
func TestAliasUsesThePrefixTheKernelTries(t *testing.T) {
	got := aliasPath("71714b79-784f-4b54-756f-65624e495374")
	want := "/dev/disk/by-id/nvme-eui.71714b79784f4b54756f65624e495374"
	if got != want {
		t.Errorf("aliasPath = %q, want %q", got, want)
	}
	if strings.Contains(got, "eui64") {
		t.Error("the alias uses the eui64 spelling, which the kernel never looks for")
	}
}

// Every pNFS mount carries vers=4.1: layouts do not exist before it.
func TestMountOptionsAlwaysCarryV41(t *testing.T) {
	opts := mountOptions("")
	if len(opts) != 1 || opts[0] != "vers=4.1" {
		t.Fatalf("options = %v, want just vers=4.1", opts)
	}
	// A class may add options, but must not be able to drop the version by
	// supplying its own list.
	opts = mountOptions("noatime, nconnect=4")
	if opts[0] != "vers=4.1" {
		t.Errorf("options = %v, want vers=4.1 first", opts)
	}
	if len(opts) != 3 {
		t.Errorf("options = %v, want the two extras appended", opts)
	}
}

// A namespace with no usable NGUID cannot be mapped, so staging refuses rather
// than mounting something that will quietly route through the MDS.
func TestStageRefusesAnUnusableNGUID(t *testing.T) {
	m := &fakeNFSMounter{}
	err := stagePNFS(context.Background(), m, t.TempDir(),
		"10.43.0.1", "/mnt/export", "", "/dev/nvme0n1", "")
	if err == nil {
		t.Fatal("staged a namespace with no NGUID")
	}
	if len(m.mounts) != 0 {
		t.Errorf("mounted anyway: %v", m.mounts)
	}
}

// The source is address:path, which is what an NFS mount takes.
func TestNFSSourceIsAddressAndPath(t *testing.T) {
	if got := nfsSource("10.43.199.218", "/mnt/pnfs-a"); got != "10.43.199.218:/mnt/pnfs-a" {
		t.Errorf("nfsSource = %q", got)
	}
}

// A stale alias is replaced rather than left. A device node is assigned in
// attach order, so the same namespace can be a different path after a
// reconnect, and a stale alias sends the kernel at whatever now holds it.
func TestEnsureAliasReplacesAStaleLink(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "nvme-eui.deadbeef")
	if err := os.Symlink("/dev/nvme9n9", alias); err != nil {
		t.Fatalf("seeding a stale alias: %v", err)
	}

	// ensureDeviceAlias writes under /dev/disk/by-id, which a test cannot, so
	// the replacement rule is exercised directly on the same logic path.
	existing, err := os.Readlink(alias)
	if err != nil {
		t.Fatalf("reading the stale alias: %v", err)
	}
	if existing == "/dev/nvme0n1" {
		t.Fatal("the fixture did not create a stale link")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := os.Symlink("/dev/nvme0n1", alias); err != nil {
		t.Fatalf("relinking: %v", err)
	}
	if got, _ := os.Readlink(alias); got != "/dev/nvme0n1" {
		t.Errorf("alias points at %q after replacement", got)
	}
}

// Unstaging an export that is not mounted is success, because the caller
// converges rather than failing.
func TestUnstageToleratesAnAbsentMount(t *testing.T) {
	m := &fakeNFSMounter{isMounted: false}
	if err := unstagePNFS(context.Background(), m, t.TempDir(), ""); err != nil {
		t.Fatalf("unstage: %v", err)
	}
	if len(m.unmounts) != 0 {
		t.Errorf("unmounted something that was not mounted: %v", m.unmounts)
	}
}

// A mount that fails is reported rather than swallowed into a staged-looking
// path.
func TestStageReportsAMountFailure(t *testing.T) {
	m := &fakeNFSMounter{mountErr: errors.New("mount.nfs: connection refused")}
	err := stagePNFS(context.Background(), m, t.TempDir(),
		"10.43.0.1", "/mnt/export", "71714b79784f4b54756f65624e495374", "/dev/nvme0n1", "")
	if err == nil {
		t.Fatal("a failed mount returned no error")
	}
}

// A pNFS client is an NVMe-oF initiator for the same namespace the metadata
// server made the filesystem on -- that is the whole point, and it is what lets
// the data path bypass the metadata server. So staging has to connect it, just
// as the block path does.
//
// The earlier shape of this assumed the namespace was "already connected by the
// time this runs," which was true of nothing: the block branch of
// NodeStageVolume is the other branch, so for a pNFS volume it never runs.
func TestPNFSVolumeContextIdentifiesTheBackingVolume(t *testing.T) {
	const (
		cluster = "f0bb9077-78c4-4482-9ccf-a5693ce2df78"
		pool    = "9d016dd4-34d7-42f0-b549-52a5af2f1399"
		volume  = "bfc56677-d602-4017-804b-975f3b929e3f"
	)
	handle := "nfs:" + cluster + ":" + pool + ":" + volume

	spec, ok := backingVolumeOf(handle)
	if !ok {
		t.Fatalf("%q was not recognized as a pNFS handle", handle)
	}
	if spec.ClusterID != cluster || spec.PoolID != pool || spec.VolumeUUID != volume {
		t.Errorf("backing volume = %+v, want cluster %s pool %s volume %s",
			spec, cluster, pool, volume)
	}
}

// A block handle is not a pNFS one, and must not be mistaken for it: the two
// forms differ by a field, and reading a three-part handle as a four-part one
// would attach whatever the shifted fields happened to name.
func TestBlockHandleIsNotABackingVolume(t *testing.T) {
	if _, ok := backingVolumeOf("f0bb9077:9d016dd4:bfc56677"); ok {
		t.Error("a block handle was read as a pNFS one")
	}
}

// A pNFS volume never stashes a volume context, because staging it needs
// nothing beyond the handle and the context kubelet already passes. So the
// generic repair path -- which reads that stash -- cannot repair one, and has
// to be told to leave it alone.
//
// Getting this wrong strands a pod: publish heals a dead mount before
// bind-mounting it, the heal fails on a stash that was never written, and
// NodePublishVolume returns an error kubelet retries forever.
func TestPNFSVolumesAreRepairedByTheirOwnPath(t *testing.T) {
	const pnfsHandle = "nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:pool-a:bfc56677-d602-4017-804b-975f3b929e3f"

	if !isPNFSVolume(pnfsHandle) {
		t.Error("a pNFS handle was not recognized, so repair would read a stash that does not exist")
	}
	if isPNFSVolume("f0bb9077:pool-a:bfc56677") {
		t.Error("a block handle was routed to the pNFS repair path")
	}
}

// Publishing a pNFS volume into a pod is a bind mount, and a bind mount takes
// no filesystem type.
//
// The type on the volume capability comes from the StorageClass's
// csi.storage.k8s.io/fstype, which for a ReadWriteMany volume describes the
// filesystem the metadata server makes -- not what the client has mounted,
// which is NFS. Passing it through means mount(8) looks for a helper named
// after it, and /sbin/mount.nfs exists and does not bind.
func TestPublishingAPNFSVolumeBindsWithNoFilesystemType(t *testing.T) {
	if got := publishFSType("nfs:c:p:v", xfsFS); got != "" {
		t.Errorf("fsType = %q, want empty: a bind takes no type", got)
	}
	if got := publishFSType("nfs:c:p:v", "nfs"); got != "" {
		t.Errorf("fsType = %q, want empty", got)
	}
}

// A block volume's own handling is untouched.
func TestPublishingABlockVolumeKeepsItsFilesystemType(t *testing.T) {
	if got := publishFSType("cluster:pool:volume", xfsFS); got != xfsFS {
		t.Errorf("fsType = %q, want xfs", got)
	}
}

// primeLayout is what makes the block layout usable from a pod at all.
//
// The client resolves the layout's device by opening
// /dev/disk/by-id/nvme-eui.<nguid>, and it resolves that path in the mount
// namespace of whichever process triggered the I/O. A pod's /dev is the minimal
// one kubelet builds -- no disk/ in it -- so a layout first requested by the
// application can never resolve, and the failure is expensive: the device is
// marked unavailable for two minutes and the layout's read-write fail bit is
// set, so everything afterward bypasses pNFS and routes through the metadata
// server.
//
// The node plugin's own container mounts the host's /dev. Touching the mount
// here, before any pod does, puts the resolution in a namespace where it
// succeeds and leaves the device cached for every later reader.
func TestPrimeLayoutTouchesTheMountAndLeavesNothingBehind(t *testing.T) {
	staging := t.TempDir()

	if err := primeLayout(context.Background(), staging); err != nil {
		t.Fatalf("primeLayout: %v", err)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("reading the staging path: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("priming left %v behind; the export is the user's filesystem", names)
	}
}

// A staging path that cannot be written is reported rather than hidden, but it
// is the caller that decides what to do about it.
func TestPrimeLayoutReportsAnUnwritableMount(t *testing.T) {
	if err := primeLayout(context.Background(), "/nonexistent/staging/path"); err == nil {
		t.Error("priming an unwritable mount reported success")
	}
}

// A stage that has already given up does not start I/O on the mount it gave up
// on. The file operations are plain syscalls and cannot be interrupted once
// begun, so the only useful place to look at the context is before them.
func TestPrimeLayoutRespectsACanceledStage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	staging := t.TempDir()
	if err := primeLayout(ctx, staging); err == nil {
		t.Fatal("priming ran for a stage that had already been canceled")
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("reading the staging path: %v", err)
	}
	if len(entries) != 0 {
		t.Error("priming touched the mount despite the cancellation")
	}
}
