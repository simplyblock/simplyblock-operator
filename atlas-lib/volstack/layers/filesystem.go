// The filesystem layer: the one that formats, and therefore the one that owes
// the most care about when it must not.
//
// StateAbsent is the only state that permits a mkfs, and what establishes it is
// a positive reading that the device holds nothing. A tool reporting that it
// recognized nothing is not that, which is what reformatted a data-bearing
// volume on 2026-09-03.

package layers

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// FilesystemOps is the mounting and formatting this layer needs.
//
// It is an interface because atlas has no business depending on a Kubernetes
// mount library, and because the consumer that does already has one: the CSI
// driver passes its own, and a test passes one that records rather than acts.
type FilesystemOps interface {
	// Format writes a new filesystem. It is called only for a device the
	// content reading found blank.
	Format(ctx context.Context, device, fsType string, options []string) error

	// Mount attaches the filesystem at target.
	Mount(ctx context.Context, source, target, fsType string, options []string) error

	// Unmount detaches it, and is a no-op when nothing is mounted there.
	Unmount(ctx context.Context, target string) error

	// IsMountPoint reports whether anything is mounted at path.
	IsMountPoint(ctx context.Context, path string) (bool, error)
}

// ContentReader is the reading a format decision rests on, which
// blockdev.Prober implements.
type ContentReader interface {
	Read(ctx context.Context, dev blockdev.Device) (blockdev.Reading, error)
}

// FilesystemConfig is what a filesystem layer is built with.
type FilesystemConfig struct {
	// FsType is the filesystem to create on a blank device. What is already on
	// a device is mounted as what it is, whatever this says.
	FsType string

	// StagingPath is where the filesystem is mounted.
	StagingPath string

	// MountFlags are the flags the volume asked for, before the ones this layer
	// derives from the filesystem itself.
	MountFlags []string

	// FormatOptions are passed to Format ahead of anything derived from the
	// geometry below.
	FormatOptions []string

	Ops     FilesystemOps
	Content ContentReader
}

// Filesystem formats a blank device and mounts what is on any other.
type Filesystem struct {
	cfg FilesystemConfig
}

// NewFilesystem returns the filesystem layer for one volume.
func NewFilesystem(cfg FilesystemConfig) *Filesystem { return &Filesystem{cfg: cfg} }

// Name is what the record calls this layer.
func (f *Filesystem) Name() string { return "filesystem" }

// Observe reads the device below and reports what may be done to it.
//
// Every reading that is neither a blank device nor a filesystem is an error
// rather than a state. A physical-volume label where a filesystem was expected
// means the plan is wrong, and content this driver did not write means the
// device is somebody's: neither is a thing to converge, and formatting through
// the doubt is what this layer exists to refuse.
func (f *Filesystem) Observe(ctx context.Context, below volstack.Artifact) (volstack.State, error) {
	mounted, err := f.cfg.Ops.IsMountPoint(ctx, f.cfg.StagingPath)
	if err != nil {
		return volstack.StateAbsent, fmt.Errorf("filesystem: check %s: %w", f.cfg.StagingPath, err)
	}
	if mounted {
		return volstack.StateReady, nil
	}

	reading, err := f.read(ctx, below)
	if err != nil {
		return volstack.StateAbsent, err
	}
	switch reading.Content {
	case blockdev.ContentBlank:
		return volstack.StateAbsent, nil
	case blockdev.ContentFilesystem:
		return volstack.StateInactive, nil
	case blockdev.ContentStackLayer, blockdev.ContentForeign, blockdev.ContentUnknown:
		return volstack.StateAbsent, fmt.Errorf(
			"filesystem: refusing to stage %s, which carries %s: %s",
			deviceOf(below), reading.Content, reading.Detail)
	default:
		return volstack.StateAbsent, fmt.Errorf(
			"filesystem: refusing to stage %s on an unrecognized reading", deviceOf(below))
	}
}

// Ensure formats a blank device and mounts whatever is there, and does nothing
// at all to one already mounted.
func (f *Filesystem) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	dev, ok := below.Device()
	if !ok {
		return volstack.Artifact{}, errors.New(
			"filesystem: the layer below exposes no single device to put a filesystem on")
	}

	state, err := f.Observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}
	if state == volstack.StateReady {
		return volstack.Artifact{Devices: below.Devices, Path: f.cfg.StagingPath}, nil
	}

	reading, err := f.read(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}

	// The filesystem to mount is the one that is there. It and the one the
	// volume asked for disagree exactly when it matters, for a volume formatted
	// before its StorageClass changed, and mounting ext4 as XFS fails.
	fsType := reading.Type
	if state == volstack.StateAbsent {
		fsType = f.cfg.FsType
		if err := f.cfg.Ops.Format(ctx, dev.Path, fsType, f.formatOptions(below)); err != nil {
			return volstack.Artifact{}, fmt.Errorf("filesystem: format %s as %s: %w", dev.Path, fsType, err)
		}
	}

	if err := f.cfg.Ops.Mount(ctx, dev.Path, f.cfg.StagingPath, fsType, mountFlags(fsType, f.cfg.MountFlags)); err != nil {
		return volstack.Artifact{}, fmt.Errorf("filesystem: mount %s at %s as %s: %w",
			dev.Path, f.cfg.StagingPath, fsType, err)
	}
	return volstack.Artifact{Devices: below.Devices, Path: f.cfg.StagingPath}, nil
}

// Release unmounts and keeps the filesystem. It is the only verb an unstage
// calls, and it is a no-op on a path that is not mounted, because a teardown may
// resume against a stack that is already partly down.
func (f *Filesystem) Release(ctx context.Context, _ volstack.Artifact) error {
	mounted, err := f.cfg.Ops.IsMountPoint(ctx, f.cfg.StagingPath)
	if err != nil {
		return fmt.Errorf("filesystem: check %s: %w", f.cfg.StagingPath, err)
	}
	if !mounted {
		return nil
	}
	if err := f.cfg.Ops.Unmount(ctx, f.cfg.StagingPath); err != nil {
		return fmt.Errorf("filesystem: unmount %s: %w", f.cfg.StagingPath, err)
	}
	return nil
}

// Destroy does nothing. Removing a filesystem means removing the volume it is
// on, which is the control plane's, and a node reaching for that on a teardown
// is the defect the separation of Release and Destroy exists to prevent.
func (f *Filesystem) Destroy(context.Context, volstack.Artifact) error { return nil }

// Healthy reports whether the mount is still serving.
//
// A dead mount is what total path loss leaves behind, and some filesystems do
// not shut down when their backing device is removed, so a mount that answers
// from cache looks healthy while writing nowhere. The check therefore asks the
// mount whether it is a mount at all and lets the error class speak: an
// ENOTCONN, ESTALE, or EIO-class answer is a mount that is gone.
func (f *Filesystem) Healthy(ctx context.Context, _ volstack.Artifact) (bool, error) {
	mounted, err := f.cfg.Ops.IsMountPoint(ctx, f.cfg.StagingPath)
	if err != nil {
		// A mount point that cannot be interrogated is not a healthy one, and
		// saying so is what makes a heal run rather than an error propagate.
		return false, nil
	}
	return mounted, nil
}

// Heal remounts and never reformats: the data exists, which is the whole
// difference between this and a bring-up.
func (f *Filesystem) Heal(ctx context.Context, below, _ volstack.Artifact) error {
	dev, ok := below.Device()
	if !ok {
		return errors.New("filesystem: the layer below exposes no device to remount")
	}
	reading, err := f.read(ctx, below)
	if err != nil {
		return err
	}
	if reading.Content != blockdev.ContentFilesystem {
		return fmt.Errorf(
			"filesystem: refusing to remount %s, which carries %s rather than a filesystem: %s",
			dev.Path, reading.Content, reading.Detail)
	}
	if err := f.cfg.Ops.Mount(ctx, dev.Path, f.cfg.StagingPath, reading.Type,
		mountFlags(reading.Type, f.cfg.MountFlags)); err != nil {
		return fmt.Errorf("filesystem: remount %s at %s: %w", dev.Path, f.cfg.StagingPath, err)
	}
	return nil
}

// FilesystemParams is what the record carries for this layer.
type FilesystemParams struct {
	FsType string `json:"fsType"`
}

// Params is what a later process needs to rebuild this layer. The filesystem
// recorded is the one the volume asked for, and a teardown needs no more than
// that: what is actually on the device is read from the device.
func (f *Filesystem) Params() (any, error) {
	return FilesystemParams{FsType: f.cfg.FsType}, nil
}

// read takes the content reading of the device below.
func (f *Filesystem) read(ctx context.Context, below volstack.Artifact) (blockdev.Reading, error) {
	dev, ok := below.Device()
	if !ok {
		return blockdev.Reading{}, errors.New(
			"filesystem: the layer below exposes no single device to read")
	}
	reading, err := f.cfg.Content.Read(ctx, dev)
	if err != nil {
		return blockdev.Reading{}, fmt.Errorf(
			"filesystem: cannot read what %s carries, so it is not a device this may format: %w",
			dev.Path, err)
	}
	return reading, nil
}

// formatOptions are the caller's, plus stripe alignment when the layer below
// reports geometry to align to.
//
// A virtualized device reports none, and the hints computed for the backend
// underneath it describe nothing once its blocks are relocated, so passing them
// there is misleading rather than merely useless.
func (f *Filesystem) formatOptions(below volstack.Artifact) []string {
	opts := append([]string{}, f.cfg.FormatOptions...)
	if !below.Geometry.Known() || f.cfg.FsType != "xfs" {
		return opts
	}
	return append(opts,
		"-d", fmt.Sprintf("su=%d,sw=%d", below.Geometry.ChunkBytes, below.Geometry.Stripes),
		"-l", fmt.Sprintf("su=%d", below.Geometry.ChunkBytes))
}

// mountFlags are the volume's, plus the ones the filesystem itself requires.
//
// XFS refuses to mount two filesystems carrying the same UUID, which a volume
// and its clone or restored snapshot do, so nouuid is what lets both be mounted
// on one node.
func mountFlags(fsType string, flags []string) []string {
	out := append([]string{}, flags...)
	if fsType == "xfs" {
		out = append(out, "nouuid")
	}
	return out
}

// deviceOf names the device below for an error message, without asserting there
// is exactly one.
func deviceOf(below volstack.Artifact) string {
	if dev, ok := below.Device(); ok {
		return dev.Path
	}
	return "the device below"
}
