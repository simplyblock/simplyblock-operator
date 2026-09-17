// Making and mounting the filesystem an export serves, on the host.
//
// The driver image is built on a newer base than the hosts it runs on, and that
// is not a detail: the container's mkfs.xfs enables on-disk features the host
// kernel does not know, so a filesystem made in the container formats cleanly
// and then cannot be mounted at all: the kernel reports a superblock with
// unknown incompatible features, naming a flag rather than the version skew
// behind it.
//
// So the filesystem is made and mounted with the host's own tools, in the
// host's mount namespace, for the same reason exportfs runs there: this
// filesystem is the host's. Its kernel mounts it, its nfsd serves it, and its
// userspace has to be able to make it.
//
// It also removes the last thing that depended on the export mount propagating
// out of the container, which was true but delicate.

package nfsexport

import (
	"context"
	"fmt"
	"strings"
)

// formatCommand builds the mkfs for a device.
//
// -f overwrites what is already there. That reads dangerous and is the safe
// choice here, because Create only calls Format for a device its blank check
// said carries nothing -- and the one case that reaches this with something on
// it is a filesystem an older attempt made that this kernel cannot mount, which
// is exactly what has to be replaced.
func formatCommand(device, fsType string, options []string) (string, []string) {
	args := append([]string{"-f"}, options...)
	return hostCommand("mkfs."+fsType, append(args, device)...)
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
	out, code, err := exec(ctx, name, args...)
	if err != nil {
		return false, fmt.Errorf("checking whether %s is a mount point: %w", path, err)
	}
	// mountpoint -q exits 0 when it is one and non-zero when it is not, and it
	// says nothing either way. A non-zero exit is the answer, not a failure.
	if code != 0 && len(strings.TrimSpace(string(out))) > 0 {
		return false, nil
	}
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
