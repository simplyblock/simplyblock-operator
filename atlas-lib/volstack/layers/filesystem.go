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

	// ForceUnmount detaches a mount that will not come down the ordinary way,
	// which is what total path loss leaves behind: the backing device is gone,
	// the mount answers ENOTCONN or EIO, and a plain unmount hangs or refuses.
	// It is the layer's force path, and a layer without one strands the stack it
	// sits on.
	ForceUnmount(ctx context.Context, target string) error

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
	// FsType is the filesystem this volume is. It decides what a blank device is
	// formatted as, and it is also the only filesystem the layer will mount: a
	// device carrying another is refused, because neither reformatting it nor
	// serving what is on it is safe.
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

// Filesystem formats a blank device, mounts one already carrying the filesystem
// the volume is, and refuses every other device.
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
func (f *Filesystem) Observe(ctx context.Context, below volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	state, _, own, err := f.observe(ctx, below)
	return state, own, err
}

// observe is Observe plus the reading it decided on. Keeping them in one call is
// what makes a stage read the device once rather than twice, and on a degraded
// device a probe is the expensive thing in the whole path.
func (f *Filesystem) observe(
	ctx context.Context, below volstack.Artifact,
) (volstack.State, blockdev.Reading, volstack.Artifact, error) {
	mounted, err := f.cfg.Ops.IsMountPoint(ctx, f.cfg.StagingPath)
	if err != nil {
		return volstack.StateAbsent, blockdev.Reading{}, volstack.Artifact{},
			fmt.Errorf("filesystem: check %s: %w", f.cfg.StagingPath, err)
	}
	mountedArtifact := volstack.Artifact{Devices: below.Devices, Path: f.cfg.StagingPath}
	if mounted {
		return volstack.StateReady, blockdev.Reading{}, mountedArtifact, nil
	}

	reading, err := f.read(ctx, below)
	if err != nil {
		return volstack.StateAbsent, blockdev.Reading{}, volstack.Artifact{}, err
	}
	switch reading.Content {
	case blockdev.ContentBlank:
		// Nothing of this layer exists yet, so it exposes nothing.
		return volstack.StateAbsent, reading, volstack.Artifact{}, nil
	case blockdev.ContentFilesystem:
		if err := f.agrees(reading); err != nil {
			return volstack.StateAbsent, reading, volstack.Artifact{}, err
		}
		// The filesystem is there and is not mounted. It exposes no path until
		// it is, which is what the layer above waits for.
		return volstack.StateInactive, reading, volstack.Artifact{Devices: below.Devices}, nil
	case blockdev.ContentStackLayer, blockdev.ContentForeign, blockdev.ContentUnknown:
		return volstack.StateAbsent, reading, volstack.Artifact{}, fmt.Errorf(
			"filesystem: refusing to stage %s, which carries %s: %s",
			deviceOf(below), reading.Content, reading.Detail)
	default:
		return volstack.StateAbsent, reading, volstack.Artifact{}, fmt.Errorf(
			"filesystem: refusing to stage %s on an unrecognized reading", deviceOf(below))
	}
}

// Ensure formats a blank device, mounts one already carrying this volume's
// filesystem, and does nothing at all to one already mounted.
func (f *Filesystem) Ensure(ctx context.Context, below volstack.Artifact) (volstack.Artifact, error) {
	dev, ok := below.Device()
	if !ok {
		return volstack.Artifact{}, errors.New(
			"filesystem: the layer below exposes no single device to put a filesystem on")
	}

	// One observation: the device is read once per Ensure rather than once to
	// decide the state and again to act on it. The reading itself is not needed
	// past that, because the filesystem to act on is the one the plan named and
	// observe has already refused every device carrying another.
	state, _, own, err := f.observe(ctx, below)
	if err != nil {
		return volstack.Artifact{}, err
	}
	if state == volstack.StateReady {
		return own, nil
	}

	// The filesystem is the one the plan asked for, because observe has already
	// refused every device carrying another. Nothing here has to reconcile a
	// disagreement, which is the point: the only two ways to reconcile one are to
	// reformat, which destroys the volume, and to serve the other filesystem,
	// which hides the misconfiguration until something else acts on it.
	fsType := f.cfg.FsType
	if state == volstack.StateAbsent {
		if err := f.cfg.Ops.Format(ctx, dev.Path, fsType, f.formatOptions(below)); err != nil {
			return volstack.Artifact{}, fmt.Errorf("filesystem: format %s as %s: %w", dev.Path, fsType, err)
		}
	}

	if err := f.cfg.Ops.Mount(ctx, dev.Path, f.cfg.StagingPath, fsType, f.mountFlags()); err != nil {
		return volstack.Artifact{}, fmt.Errorf("filesystem: mount %s at %s as %s: %w",
			dev.Path, f.cfg.StagingPath, fsType, err)
	}
	return volstack.Artifact{Devices: below.Devices, Path: f.cfg.StagingPath}, nil
}

// Release unmounts and keeps the filesystem. It is the only verb an unstage
// calls, and it is a no-op on a path that is not mounted, because a teardown may
// resume against a stack that is already partly down.
func (f *Filesystem) Release(ctx context.Context, _ volstack.Artifact) error {
	return f.clear(ctx)
}

// clear detaches whatever is mounted at the staging path, and detaches it the
// hard way when it will not come down the ordinary one.
//
// The hard way is not an exceptional path. Total path loss removes the device
// while the mount above it is still there, the mount then answers ENOTCONN or
// EIO, and a plain unmount refuses or hangs. A layer with no force path strands
// the stack it sits on, which is why the design gives every layer one.
//
// A path that is not mounted is not an error: a teardown may resume against a
// stack that is already partly down.
func (f *Filesystem) clear(ctx context.Context) error {
	mounted, err := f.cfg.Ops.IsMountPoint(ctx, f.cfg.StagingPath)
	if err != nil {
		// A mount point that cannot be interrogated is how a dead mount
		// presents, so this is a reason to detach it rather than to stop.
		mounted = true
	}
	if !mounted {
		return nil
	}

	if err := f.cfg.Ops.Unmount(ctx, f.cfg.StagingPath); err == nil {
		return nil
	}
	if err := f.cfg.Ops.ForceUnmount(ctx, f.cfg.StagingPath); err != nil {
		return fmt.Errorf("filesystem: unmount %s, including its force path: %w", f.cfg.StagingPath, err)
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
	if err := f.agrees(reading); err != nil {
		return err
	}

	// Clear whatever is at the path before mounting onto it. A heal runs against
	// a mount that Healthy just reported unserviceable, and the dead mount total
	// path loss leaves behind is still a mount: mounting over it stacks a second
	// one on the same path, and a teardown that unmounts once then removes the
	// path recursively walks into the filesystem left underneath.
	if err := f.clear(ctx); err != nil {
		return err
	}

	if err := f.cfg.Ops.Mount(ctx, dev.Path, f.cfg.StagingPath, f.cfg.FsType, f.mountFlags()); err != nil {
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
func (f *Filesystem) Params() any {
	return FilesystemParams{FsType: f.cfg.FsType}
}

// agrees reports whether the filesystem on the device is the one the plan asked
// for, and refuses when it is not.
//
// A volume formatted as one filesystem and asked for as another is somebody
// having changed what a class says about a volume that already exists, and there
// is no safe way to reconcile that here. Reformatting destroys the volume, which
// is the failure this whole design exists to prevent. Mounting the one that is
// there works, and leaves a volume serving something nobody declared, with the
// disagreement recorded in a log line and in nothing else, until whatever notices
// next decides to make the device match the plan again.
//
// So it stops, while the data is intact and while the misconfiguration is still
// the thing in front of whoever is looking.
//
// An empty FsType expresses no opinion, which is what a plan that never named a
// filesystem has, and there is then nothing to disagree with.
func (f *Filesystem) agrees(reading blockdev.Reading) error {
	if f.cfg.FsType == "" || reading.Type == f.cfg.FsType {
		return nil
	}
	return fmt.Errorf(
		"filesystem: the volume carries %s and this plan asks for %s, and neither reformatting it "+
			"nor mounting it as %s is safe; the plan and the volume have to be reconciled first",
		reading.Type, f.cfg.FsType, reading.Type)
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

// formatOptions are the volume's own, plus whatever the filesystem being created
// asks for on the geometry below it.
func (f *Filesystem) formatOptions(below volstack.Artifact) []string {
	return f.strategy().FormatOptions(append([]string{}, f.cfg.FormatOptions...), below.Geometry)
}

// mountFlags are the volume's own, plus whatever the filesystem requires in
// order to mount at all.
func (f *Filesystem) mountFlags() []string {
	return f.strategy().MountFlags(append([]string{}, f.cfg.MountFlags...))
}

// strategy is the per-filesystem half of this layer, for the filesystem the plan
// asked for. That is also the only one the layer acts on, since a device
// carrying another is refused rather than reconciled.
func (f *Filesystem) strategy() FilesystemLayerStrategy {
	return FilesystemStrategyFor(f.cfg.FsType)
}

// deviceOf names the device below for an error message, without asserting there
// is exactly one.
func deviceOf(below volstack.Artifact) string {
	if dev, ok := below.Device(); ok {
		return dev.Path
	}
	return "the device below"
}
