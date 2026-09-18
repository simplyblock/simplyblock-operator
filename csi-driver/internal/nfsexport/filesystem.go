// Making and mounting the filesystem an export serves, on the host.
//
// The driver image is built on a newer base than the hosts it runs on, and that
// is not a detail: the container's mkfs.xfs enables on-disk features the host
// kernel does not know, so a filesystem made there formats cleanly and then
// cannot be mounted, with the kernel naming a feature flag rather than the
// version skew behind it.
//
// So it is made and mounted with the host's own tools, for the same reason
// exportfs runs there: the filesystem is the host's. That also removes the last
// dependency on the export mount propagating out of the container.

package nfsexport

import (
	"context"
	"fmt"
	"strings"
)

// formatCommand builds the mkfs for a device.
//
// Deliberately without -f. Format is only reached for a device the blank check
// said carries nothing, so mkfs refusing because it found a filesystem is not
// an obstacle to override -- it is two independent answers disagreeing about
// whether this device holds data, and the safe reading of that disagreement is
// the one that does not write.
//
// This repository has already lost data to the other reading: an unreadable
// device made blkid exit non-zero, that was read as an empty device, and mkfs ran on a
// live filesystem. The second opinion is worth keeping.
func formatCommand(device, fsType string, options []string) (string, []string) {
	return hostCommand("mkfs."+fsType, append(append([]string{}, options...), device)...)
}

func mountCommand(source, target, fsType string, options []string) (string, []string) {
	args := []string{"-t", fsType}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	return hostCommand("mount", append(args, source, target)...)
}

func unmountCommand(target string) (string, []string) {
	return hostCommand("umount", target)
}

func mountPointCommand(path string) (string, []string) {
	return hostCommand("mountpoint", "-q", path)
}

// hostFilesystem satisfies atlas/export's Filesystem against the host.
type hostFilesystem struct{}

func (hostFilesystem) Format(ctx context.Context, device, fsType string, options []string) error {
	name, args := formatCommand(device, fsType, options)
	return run(ctx, name, args)
}

func (hostFilesystem) Mount(ctx context.Context, source, target, fsType string, options []string) error {
	name, args := mountCommand(source, target, fsType, options)
	return run(ctx, name, args)
}

func (hostFilesystem) Unmount(ctx context.Context, target string) error {
	name, args := unmountCommand(target)
	return run(ctx, name, args)
}

// IsMountPoint asks the host, not this container. The two have different views,
// and a mount visible in one and not the other would make assembly either skip
// a mount that is missing or repeat one that is already there.
func (hostFilesystem) IsMountPoint(ctx context.Context, path string) (bool, error) {
	name, args := mountPointCommand(path)
	_, code, err := exec(ctx, name, args...)
	if err != nil {
		return false, fmt.Errorf("checking whether %s is a mount point: %w", path, err)
	}
	// mountpoint -q exits 0 when the path is a mount point and non-zero when it
	// is not, including when it does not exist, and prints nothing either way.
	// So a non-zero exit is the answer rather than a failure, and "not there"
	// and "there but not mounted" are the same answer to the only question
	// assembly asks: does something need mounting here.
	return code == 0, nil
}

// run executes a built command and turns a non-zero exit into an error carrying
// what the command said, because that is the only place the reason appears.
func run(ctx context.Context, name string, args []string) error {
	out, code, err := exec(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if code != 0 {
		return fmt.Errorf("%s %s exited %d: %s",
			name, strings.Join(args, " "), code, strings.TrimSpace(string(out)))
	}
	return nil
}

// HostMounter is what the node plugin mounts a pNFS export with. It is the
// same host filesystem the metadata server assembles through, narrowed to what
// a client does: mount, unmount, and ask whether a path is already a mount.
//
// A client needs the host for a reason of its own. mount(8) hands an NFS mount
// to /sbin/mount.nfs, a helper from nfs-utils that this image does not carry
// and should not -- a host that may run a ReadWriteMany pod needs nfs-utils
// anyway, and two copies of it would be two things to keep in step. Mounting
// there also puts the mount directly where kubelet looks, rather than relying
// on it propagating out of the container.
type HostMounter interface {
	Mount(ctx context.Context, source, target, fsType string, options []string) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(ctx context.Context, path string) (bool, error)
}

// HostFilesystem returns that mounter.
func HostFilesystem() HostMounter { return hostFilesystem{} }
