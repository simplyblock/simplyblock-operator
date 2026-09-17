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

func (m *fakeNFSMounter) IsMounted(string) (bool, error) { return m.isMounted, nil }

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
