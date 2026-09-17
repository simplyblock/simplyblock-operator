// The filesystem an export serves is the host's, so it is made and mounted with
// the host's tools.
//
// This is not tidiness. The driver image is built on a newer base than the
// hosts it runs on, so its mkfs.xfs enables on-disk features the host kernel
// does not know: a filesystem made in the container formats cleanly and then
// cannot be mounted: the kernel reports a superblock with unknown incompatible features. The
// same reasoning already sends exportfs to the host, and it generalizes --
// anything producing state the host kernel must later read belongs in the
// host's namespace.

package nfsexport

import (
	"strings"
	"testing"
)

func TestFormatUsesTheHostsMkfs(t *testing.T) {
	name, args := formatCommand("/dev/nvme0n1", "xfs", nil)

	if name != "nsenter" {
		t.Fatalf("mkfs runs as %q, want the host's", name)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-- mkfs.xfs") {
		t.Errorf("args = %v, want the host's mkfs.xfs", args)
	}
	if !strings.HasSuffix(joined, "/dev/nvme0n1") {
		t.Errorf("args = %v, want the device last", args)
	}
	// No -f. Format is only reached for a device the blank check called empty,
	// so mkfs finding a filesystem is two answers disagreeing about whether
	// this device holds data, and the safe reading does not write. This
	// repository has already lost data to the other reading.
	if strings.Contains(joined, " -f") {
		t.Errorf("args = %v, want mkfs to refuse rather than overwrite", args)
	}
}

func TestFormatPassesExtraOptions(t *testing.T) {
	_, args := formatCommand("/dev/nvme0n1", "xfs", []string{"-L", "shared"})
	if !strings.Contains(strings.Join(args, " "), "-L shared") {
		t.Errorf("args = %v, want the caller's options", args)
	}
}

func TestMountAndUnmountUseTheHost(t *testing.T) {
	name, args := mountCommand("/dev/nvme0n1", "/mnt/share", "xfs", nil)
	if name != "nsenter" || !strings.Contains(strings.Join(args, " "), "-- mount -t xfs") {
		t.Errorf("mount = %s %v, want the host's mount", name, args)
	}

	name, args = unmountCommand("/mnt/share")
	if name != "nsenter" || !strings.Contains(strings.Join(args, " "), "-- umount /mnt/share") {
		t.Errorf("umount = %s %v, want the host's umount", name, args)
	}
}

// Whether the path is a mount point is asked of the host too. The container has
// its own view, and a mount that propagated in one direction and not the other
// would make assembly either skip a mount that is missing or repeat one that is
// already there.
func TestMountPointIsCheckedOnTheHost(t *testing.T) {
	name, args := mountPointCommand("/mnt/share")
	if name != "nsenter" || !strings.Contains(strings.Join(args, " "), "-- mountpoint -q /mnt/share") {
		t.Errorf("check = %s %v, want the host's mountpoint", name, args)
	}
}

// A client mounts NFS, and mount(8) hands that to /sbin/mount.nfs -- a helper
// from nfs-utils, which this image does not carry and should not. The host has
// it, because a host that may run a ReadWriteMany pod needs nfs-utils anyway.
//
// So the client's mount goes to the host for the same reason the server's does,
// and lands directly where kubelet looks rather than relying on propagation out
// of the container.
func TestNFSMountUsesTheHostsHelper(t *testing.T) {
	fs := HostFilesystem()
	name, args := mountCommand(
		"192.168.10.83:/mnt/team-a-shared", "/var/lib/kubelet/staging", "nfs", []string{"vers=4.1"})

	if fs == nil {
		t.Fatal("no host filesystem is exposed for the node plugin to mount with")
	}
	joined := strings.Join(args, " ")
	if name != "nsenter" {
		t.Fatalf("the NFS mount runs as %q, want the host's", name)
	}
	if !strings.Contains(joined, "-- mount -t nfs -o vers=4.1") {
		t.Errorf("args = %v, want the host's mount with the version option", args)
	}
	if !strings.HasSuffix(joined, "192.168.10.83:/mnt/team-a-shared /var/lib/kubelet/staging") {
		t.Errorf("args = %v, want source then target last", args)
	}
}
