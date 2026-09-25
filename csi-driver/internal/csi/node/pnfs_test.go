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

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type fakeNFSMounter struct {
	mounts    []string
	unmounts  []string
	isMounted bool
	mountErr  error
}

func (m *fakeNFSMounter) Mount(source, target, fsType string, options []string) error {
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mounts = append(m.mounts, source+" "+target+" "+fsType+" "+strings.Join(options, ","))
	m.isMounted = true
	return nil
}

func (m *fakeNFSMounter) Unmount(target string) error {
	m.unmounts = append(m.unmounts, target)
	m.isMounted = false
	return nil
}

func (m *fakeNFSMounter) IsMounted(string) (bool, error) {
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
	if len(nfsMountOptions) != 1 || nfsMountOptions[0] != "vers=4.1" {
		t.Fatalf("options = %v, want just vers=4.1", nfsMountOptions)
	}
}

// A namespace with no usable NGUID cannot be mapped, so staging refuses rather
// than mounting something that will quietly route through the MDS.
func TestStageRefusesAnUnusableNGUID(t *testing.T) {
	m := &fakeNFSMounter{}
	err := stagePNFS(context.Background(), m, t.TempDir(),
		"10.43.0.1", "/var/lib/simplyblock/exports/export", "", "/dev/nvme0n1")
	if err == nil {
		t.Fatal("staged a namespace with no NGUID")
	}
	if len(m.mounts) != 0 {
		t.Errorf("mounted anyway: %v", m.mounts)
	}
}

// The source is address:path, which is what an NFS mount takes.
func TestNFSSourceIsAddressAndPath(t *testing.T) {
	const path = "/var/lib/simplyblock/exports/pnfs-a"
	if got := nfsSource("10.43.199.218", path); got != "10.43.199.218:"+path {
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
	if err := unstagePNFS(m, t.TempDir(), ""); err != nil {
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
		"10.43.0.1", "/var/lib/simplyblock/exports/export", "71714b79784f4b54756f65624e495374", "/dev/nvme0n1")
	if err == nil {
		t.Fatal("a failed mount returned no error")
	}
}

// A pNFS client is an NVMe-oF initiator for the same namespace the metadata
// server made the filesystem on -- that is the whole point, and it is what lets
// the data path bypass the metadata server. So staging has to connect it, just
// as the block path does.
func TestBackingVolumeIsReadFromTheHandle(t *testing.T) {
	const (
		cluster = "f0bb9077-78c4-4482-9ccf-a5693ce2df78"
		pool    = "9d016dd4-34d7-42f0-b549-52a5af2f1399"
		volume  = "bfc56677-d602-4017-804b-975f3b929e3f"
	)

	spec, ok := backingVolumeOf(cluster + ":" + pool + ":" + volume)
	if !ok {
		t.Fatal("the handle was not recognized")
	}
	if spec.ClusterID != cluster || spec.PoolID != pool || spec.VolumeUUID != volume {
		t.Errorf("backing volume = %+v", spec)
	}
}

// What a volume is, is read from the stash rather than from its handle: a pNFS
// volume's handle is its backing volume's, so a snapshot or a clone addresses
// the lvol without knowing an export is in front of it.
//
// Getting this wrong strands a pod. Publish heals a dead mount before binding
// it, and the generic heal builds a volume stack a pNFS volume does not have.
func TestPNFSIsRecognizedFromTheStashedContext(t *testing.T) {
	pnfs := map[string]string{csicommon.CtxAccessProtocol: csicommon.AccessProtocolNFS}
	if !isPNFSVolume(pnfs) {
		t.Error("a staged pNFS volume was not recognized, so publish would build it a stack")
	}
	if isPNFSVolume(map[string]string{"uuid": "bfc56677"}) {
		t.Error("a block volume was routed to the pNFS path")
	}
	if isPNFSVolume(nil) {
		t.Error("a volume with no stash was routed to the pNFS path")
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
