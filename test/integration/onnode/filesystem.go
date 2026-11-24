//go:build linux

// Formatting and mounting on the node, which the filesystem layer takes as an
// interface because atlas has no business depending on a mount library.
//
// The CSI driver fills this seam with the Kubernetes mount utilities. Filling it
// here with the same tools the driver's image carries keeps the difference to
// the library and away from the syscalls: it is a real mkfs writing a real
// filesystem, and a real mount syscall over a real fabric device.

package onnode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// shellFilesystem is layers.FilesystemOps over the node's own tools.
type shellFilesystem struct{}

// Format writes a new filesystem. The layer calls it only for a device its
// content reading found blank, which is the guarantee under test rather than
// one this implementation adds.
func (shellFilesystem) Format(ctx context.Context, device, fsType string, options []string) error {
	args := append(append([]string{}, options...), device)
	return runTool(ctx, "mkfs."+fsType, args...)
}

// Mount attaches the filesystem at target, creating the target first because a
// staging path does not exist until something makes it.
func (shellFilesystem) Mount(ctx context.Context, source, target, fsType string, options []string) error {
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	args := []string{"-t", fsType}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, source, target)
	return runTool(ctx, "mount", args...)
}

// Unmount detaches the filesystem, and a path that is not mounted is not an
// error: a teardown may resume against a stack that is already partly down.
func (shellFilesystem) Unmount(ctx context.Context, target string) error {
	err := runTool(ctx, "umount", target)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not mounted") {
		return nil
	}
	return err
}

// ForceUnmount detaches a mount that will not come down the ordinary way, which
// is what total path loss leaves behind: the device is gone, the mount answers
// ENOTCONN or EIO, and a plain umount hangs or refuses. The lazy detach is what
// unblocks the path itself, and the force is what abandons the pending I/O.
func (shellFilesystem) ForceUnmount(ctx context.Context, target string) error {
	if err := runTool(ctx, "umount", "-f", "-l", target); err != nil {
		return fmt.Errorf("force unmount %s: %w", target, err)
	}
	return nil
}

// Grow runs the resize tool the filesystem chose, pointed at whatever that
// filesystem resizes. Nothing here decides which tool it is: the layer asked its
// filesystem, and this runs what it was handed.
func (shellFilesystem) Grow(ctx context.Context, command []string) error {
	if len(command) == 0 {
		return errors.New("no resize command to run")
	}
	return runTool(ctx, command[0], command[1:]...)
}

// IsMountPoint reports whether anything is mounted at path, by comparing the
// device path sits on with its parent's.
//
// It stats rather than reading the mount table on purpose. A mount whose
// backing device is gone is still listed in the table while a stat on it
// answers ENOTCONN or EIO, and reporting that as an error is what makes the
// layer heal it rather than believe it is serving.
func (shellFilesystem) IsMountPoint(_ context.Context, path string) (bool, error) {
	here, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	up, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, fmt.Errorf("stat the parent of %s: %w", path, err)
	}

	hereSys, ok := here.Sys().(*syscall.Stat_t)
	upSys, upOK := up.Sys().(*syscall.Stat_t)
	if !ok || !upOK {
		return false, fmt.Errorf("no stat data for %s", path)
	}
	return hereSys.Dev != upSys.Dev, nil
}

// OtherMounts lists the mount points other than path that hold the filesystem
// mounted at path, read from this process's mount table.
func (shellFilesystem) OtherMounts(_ context.Context, path string) ([]string, error) {
	table, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read the mount table: %w", err)
	}
	return otherMountsIn(string(table), path), nil
}

// otherMountsIn finds path in a mountinfo table and returns every other mount
// point carrying the same device number, which a bind mount of it does.
// Fields are id, parent, major:minor, root, mount point, with the mount point
// octal-escaped.
func otherMountsIn(table, path string) []string {
	type mount struct{ device, point string }
	var mounts []mount
	device := ""
	for line := range strings.Lines(table) {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		m := mount{device: fields[2], point: unescapeMountPoint(fields[4])}
		mounts = append(mounts, m)
		if m.point == path {
			device = m.device
		}
	}
	if device == "" {
		return nil
	}
	var others []string
	for _, m := range mounts {
		if m.device == device && m.point != path {
			others = append(others, m.point)
		}
	}
	return others
}

// unescapeMountPoint undoes the kernel's octal escaping of space, tab,
// newline, and backslash in a mountinfo field.
func unescapeMountPoint(field string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(field)
}

// runTool executes one of the node's storage tools and folds its output into
// the error, because the layers above match on what a tool said.
func runTool(ctx context.Context, name string, args ...string) error {
	_, err := runToolOutput(ctx, name, args...)
	return err
}

// runToolOutput is runTool for a tool that is being asked something rather than
// told to do something.
func runToolOutput(ctx context.Context, name string, args ...string) (string, error) {
	//nolint:gosec // a fixed set of storage tools, with structured arguments
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
