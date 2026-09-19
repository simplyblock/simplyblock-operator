// The volume stack's filesystem operations, performed with this node's mount
// utilities.
//
// The stack's filesystem layer takes formatting and mounting as an interface,
// because atlas has no business depending on a Kubernetes mount library. This
// is the side of that seam the CSI driver fills in, and it lives here rather
// than in the node service because every one of these is a question about the
// node: what mkfs is called, whether a path is mounted, and how a mount that
// will not come down is taken down anyway.
//
// It is deliberately thin. Nothing here decides whether a device may be
// formatted. That decision is the layer's, and it is the whole point of the
// layer that it is made in one place.

package mount

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/simplyblock/atlas/volstack/layers"
)

// FilesystemOperations is the stack's FilesystemOps over one node's mounter.
type FilesystemOperations struct {
	m *Mounter
}

// FilesystemOps returns the operations the stack's filesystem layer is built
// with.
func (m *Mounter) FilesystemOps() *FilesystemOperations {
	return &FilesystemOperations{m: m}
}

// Compile-time proof that this is the seam the layer asks for, so a change to
// the contract fails here rather than at the one call site that wires it.
var _ layers.FilesystemOps = (*FilesystemOperations)(nil)

// ErrDeadMount reports a mount whose backing device is gone: total path loss
// removed the device and left the mount answering ENOTCONN, ESTALE, or EIO, or
// answering from cache while writing nowhere.
var ErrDeadMount = errors.New("the mount is dead, because the device behind it is gone")

// extFamily are the filesystems mke2fs creates, which are the ones that take
// -F and a reservation.
var extFamily = map[string]bool{"ext2": true, "ext3": true, "ext4": true}

// defaultReservedBlocks is the reservation an ext filesystem is created with
// when the volume asked for none. It is zero because a CSI volume is a whole
// device handed to one workload, and holding 5% of it back for privileged
// processes on the node serves nobody.
const defaultReservedBlocks = "-m0"

// Format writes a new filesystem on device.
//
// The layer calls this only for a device its content reading found blank, so
// nothing here re-decides that: a format arriving here is one that has already
// been established as safe.
//
// For the ext family the reservation default is passed *before* the caller's
// options, because mke2fs takes the last -m it is given and a volume that asked
// for a reservation has to get it. Everything else is given its options as they
// are, with the device last, which is where every mkfs expects it.
func (o *FilesystemOperations) Format(_ context.Context, device, fsType string, options []string) error {
	args := make([]string, 0, len(options)+3)
	if extFamily[fsType] {
		args = append(args, "-F", defaultReservedBlocks)
	}
	args = append(args, options...)
	args = append(args, device)

	tool := "mkfs." + fsType
	output, err := o.m.execer.Command(tool, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (%s)", tool, args, err, output)
	}
	return nil
}

// Mount attaches source at target, creating the directory the mount lands on.
//
// The directory is this package's to make: the layer describes a filesystem and
// owns nothing about the paths beneath it, and a staging path that does not
// exist yet is the ordinary case on a volume's first stage.
func (o *FilesystemOperations) Mount(_ context.Context, source, target, fsType string, options []string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create the mount point %s: %w", target, err)
	}
	return o.m.Mount(source, target, fsType, options)
}

// Unmount detaches whatever is mounted at target.
func (o *FilesystemOperations) Unmount(_ context.Context, target string) error {
	return o.m.Unmount(target)
}

// ForceUnmount detaches a mount that will not come down the ordinary way, which
// is what total path loss leaves behind.
func (o *FilesystemOperations) ForceUnmount(_ context.Context, target string) error {
	return o.m.ForceUnmount(target)
}

// IsMountPoint reports whether anything is mounted at path.
//
// A path that does not exist is not mounted, which is the answer that lets a
// first stage carry on. A mount whose backing device is gone is reported as an
// error rather than as either answer, because it is neither: the layer treats
// that as a mount to be cleared and, on the heal path, as a mount to be
// remade. Saying "mounted" would have the layer leave a dead mount in place,
// and saying "not mounted" would have it mount a second filesystem on top of
// one that is still there.
func (o *FilesystemOperations) IsMountPoint(_ context.Context, path string) (bool, error) {
	mounted, err := o.m.mounter.IsMountPoint(path)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check whether %s is a mount point: %w", path, err)
	}
	if mounted && o.m.IsDead(path) {
		return false, fmt.Errorf("%s: %w", path, ErrDeadMount)
	}
	return mounted, nil
}

// Grow runs the resize command the filesystem chose. Which tool it is and what
// it is pointed at are the filesystem's business: ext resizes a device and XFS
// resizes a mount.
func (o *FilesystemOperations) Grow(_ context.Context, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("no resize command to run")
	}
	output, err := o.m.execer.Command(command[0], command[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w (%s)", command, err, output)
	}
	return nil
}
