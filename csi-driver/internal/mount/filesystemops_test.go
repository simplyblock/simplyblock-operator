// What the volume stack's filesystem layer is handed when it formats, grows, or
// asks whether something is mounted. The commands are scripted, so what is
// asserted is the argv the layer's requests turn into rather than the state of
// a kernel.

package mount

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	k8smount "k8s.io/mount-utils"
)

// A reservation the volume asked for is passed as it was asked for.
func TestFormatPassesARequestedReservation(t *testing.T) {
	fe, calls := scriptedExec([]scriptedResult{{out: ""}})
	ops := NewWith(nil, fe).FilesystemOps()

	if err := ops.Format(context.Background(), "/dev/fake", "ext4", []string{"-m", "5"}); err != nil {
		t.Fatalf("Format: %v", err)
	}

	got := strings.Join((*calls)[0], " ")
	want := "mkfs.ext4 -F -m 5 /dev/fake"
	if got != want {
		t.Fatalf("format command:\n got %s\nwant %s", got, want)
	}
}

// A volume that asked for no reservation is formatted with none of this
// package's choosing, which leaves mke2fs at its own default.
//
// Asking for nothing and asking for zero are different requests, which is what
// the filesystem layer's own FormatParameters says of the field this comes from:
// a class that sets tune2fs_reserved_blocks to "0" gets -m 0, and a class that
// omits it gets what it got before the volume stack existed, when an unset value
// meant tune2fs was never run at all.
func TestFormatLeavesTheReservationAloneWhenTheVolumeAsksForNone(t *testing.T) {
	fe, calls := scriptedExec([]scriptedResult{{out: ""}})
	ops := NewWith(nil, fe).FilesystemOps()

	if err := ops.Format(context.Background(), "/dev/fake", "ext4", nil); err != nil {
		t.Fatalf("Format: %v", err)
	}

	got := strings.Join((*calls)[0], " ")
	want := "mkfs.ext4 -F /dev/fake"
	if got != want {
		t.Fatalf("format command:\n got %s\nwant %s", got, want)
	}
}

// Every other filesystem is given its options as they are, with the device last
// and nothing of the ext family's added.
func TestFormatPassesTheVolumeOptionsThroughForXFS(t *testing.T) {
	fe, calls := scriptedExec([]scriptedResult{{out: ""}})
	ops := NewWith(nil, fe).FilesystemOps()

	if err := ops.Format(context.Background(), "/dev/fake", "xfs", []string{"-d", "su=16k,sw=1"}); err != nil {
		t.Fatalf("Format: %v", err)
	}

	got := strings.Join((*calls)[0], " ")
	want := "mkfs.xfs -d su=16k,sw=1 /dev/fake"
	if got != want {
		t.Fatalf("format command:\n got %s\nwant %s", got, want)
	}
}

// Grow runs the command the filesystem chose, because the tool and what it is
// pointed at are the filesystem's business and not this package's.
func TestGrowRunsTheCommandItIsGiven(t *testing.T) {
	fe, calls := scriptedExec([]scriptedResult{{out: ""}})
	ops := NewWith(nil, fe).FilesystemOps()

	if err := ops.Grow(context.Background(), []string{"xfs_growfs", "/staging"}); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if got := strings.Join((*calls)[0], " "); got != "xfs_growfs /staging" {
		t.Fatalf("grow command: got %s", got)
	}
}

// A staging path that does not exist yet is not mounted, and saying so is what
// lets the layer go on to create it. Reporting the kernel's ENOENT as a failure
// would stop every first stage.
func TestIsMountPointTreatsAMissingPathAsNotMounted(t *testing.T) {
	ops := NewWith(k8smount.NewFakeMounter(nil), nil).FilesystemOps()

	mounted, err := ops.IsMountPoint(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("IsMountPoint: %v", err)
	}
	if mounted {
		t.Fatal("a path that does not exist was reported as mounted")
	}
}

// Mount creates the directory it is asked to mount onto. The layer describes a
// filesystem rather than managing the paths beneath it, so the staging
// directory is this package's to make.
func TestMountCreatesTheStagingDirectory(t *testing.T) {
	fm := k8smount.NewFakeMounter(nil)
	ops := NewWith(fm, nil).FilesystemOps()

	target := filepath.Join(t.TempDir(), "stage")
	if err := ops.Mount(context.Background(), "/dev/fake", target, "ext4", nil); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the staging directory was not created: %v", err)
	}
}

const (
	testStaging = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.simplyblock.io/abc/globalmount/c:p:v"
	testPodBind = "/var/lib/kubelet/pods/1020b550/volumes/kubernetes.io~csi/pvc-45f9/mount"
)

// opsOverMountTable returns operations reading the given mount table.
func opsOverMountTable(t *testing.T, table string) *FilesystemOperations {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := NewWith(nil, nil).FilesystemOps()
	ops.mountinfo = path
	return ops
}

// A pod's bind mount of the staged filesystem carries the staging mount's device
// number, and is what a heal has to wait out before it may unmount and mount the
// filesystem again. A mount of another device is not.
func TestOtherMountsListsTheBindMountsOfTheStagedFilesystem(t *testing.T) {
	ops := opsOverMountTable(t, ""+
		"25 1 253:0 / / rw shared:1 - xfs /dev/mapper/root rw\n"+
		"300 25 253:7 / "+testStaging+" rw shared:90 - ext4 /dev/mapper/sb-v rw\n"+
		"310 25 253:7 / "+testPodBind+" rw shared:90 - ext4 /dev/mapper/sb-v rw\n"+
		"320 25 253:8 / /var/lib/kubelet/pods/other/volumes/x/mount rw - ext4 /dev/mapper/sb-w rw\n")
	others, err := ops.OtherMounts(context.Background(), testStaging)
	if err != nil {
		t.Fatal(err)
	}
	if len(others) != 1 || others[0] != testPodBind {
		t.Fatalf("OtherMounts = %v, want only the pod's bind mount", others)
	}
}

// Once the old pod is unpublished, the staging mount is the filesystem's only
// holder, and so is a path that is not mounted at all.
func TestOtherMountsOfASolelyHeldOrUnmountedPathIsNone(t *testing.T) {
	ops := opsOverMountTable(t, "300 25 253:7 / "+testStaging+" rw - ext4 /dev/mapper/sb-v rw\n")
	for _, path := range []string{testStaging, "/not/mounted"} {
		if others, err := ops.OtherMounts(context.Background(), path); err != nil || len(others) != 0 {
			t.Errorf("OtherMounts(%s) = %v, %v; want none", path, others, err)
		}
	}
}
