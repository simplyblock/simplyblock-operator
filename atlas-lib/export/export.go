// Package export assembles and tears down a pNFS export on its metadata-server
// host.
//
// An export is a filesystem on one NVMe-oF namespace, mounted at a path and
// published through the host's nfsd. Assembly is four steps; teardown is those
// four reversed. The caller is a reconciler, so each step is skipped when
// already satisfied.
package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// ErrInvalidSpec is a spec no host could assemble.
var ErrInvalidSpec = errors.New("invalid export spec")

// NFSDProcDir is the kernel's nfsd control interface, which exists once the
// module is loaded and the nfsd filesystem is mounted on it.
const NFSDProcDir = "/proc/fs/nfsd"

// FSType is the only filesystem a pNFS SCSI layout can be served from. Not a
// default: no other Linux filesystem implements the export operations nfsd
// needs to hand out block layouts.
const FSType = "xfs"

// pnfs is what makes nfsd offer a layout at all. sync is deliberate: a shared
// filesystem cannot acknowledge writes before they land.
var exportOptions = []string{"rw", "sync", "no_subtree_check", "no_root_squash", "pnfs"}

// Spec is one export to assemble: which device, where it goes, and who may
// mount it.
type Spec struct {
	// VolumeUUID identifies the backing namespace; for a simplyblock volume it
	// is the logical volume's own id. Not a device path: the kernel assigns
	// those in attach order, so they differ across hosts.
	VolumeUUID string

	// ClusterID and PoolID let Attach ask the control plane where the namespace
	// is served from. A Config with no Attach ignores them.
	ClusterID string
	PoolID    string

	// Path is the mount point and the exported directory.
	Path string

	// FSID is what the export is published under. Stable across hosts, so a
	// re-materialized export reproduces the file handles clients hold.
	FSID string

	// Encrypted says the control plane stacks a crypto bdev under this
	// namespace, which makes an unwritten block read as pseudo-random
	// plaintext rather than as zeros. The blank check needs it, or an empty
	// encrypted volume is mistaken for an occupied one and never formatted.
	Encrypted bool

	// Clients is the set allowed to mount, as exports(5) spells it. Empty is
	// refused rather than widened, because the widening is to everyone.
	Clients []string
}

// Validate reports whether the spec can be assembled at all.
func (s Spec) Validate() error {
	switch {
	case s.VolumeUUID == "":
		return fmt.Errorf("export: no volume UUID: %w", ErrInvalidSpec)
	case s.Path == "":
		return fmt.Errorf("export: no path: %w", ErrInvalidSpec)
	case s.FSID == "":
		return fmt.Errorf("export: no fsid: %w", ErrInvalidSpec)
	case len(s.Clients) == 0:
		return fmt.Errorf("export %s: no allowed clients: %w", s.Path, ErrInvalidSpec)
	}
	return nil
}

// dropInName is the exports.d file this export owns. From the FSID, because a
// path contains separators.
func (s Spec) dropInName() string { return "pnfs-" + strings.ReplaceAll(s.FSID, ":", "-") + ".exports" }

// line is the exports(5) entry for this export.
func (s Spec) line() string {
	opts := append(append([]string{}, exportOptions...), "fsid="+s.FSID)
	clients := make([]string, 0, len(s.Clients))
	for _, c := range s.Clients {
		clients = append(clients, c+"("+strings.Join(opts, ",")+")")
	}
	return s.Path + " " + strings.Join(clients, " ") + "\n"
}

// Filesystem is the subset of volstack's FilesystemOps an export needs.
type Filesystem interface {
	Format(ctx context.Context, device, fsType string, options []string) error
	Mount(ctx context.Context, source, target, fsType string, options []string) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(ctx context.Context, path string) (bool, error)
}

// Config is what an Assembler needs to reach the host.
type Config struct {
	// Devices resolves the backing namespace.
	Devices nvme.DeviceResolver

	// Filesystem formats, mounts, and unmounts.
	Filesystem Filesystem

	// Content reads what a device carries. Asked every time: formatting one
	// that already has a filesystem destroys it.
	Content ContentReader

	// Run executes exportfs. Injected so a test does not need one.
	Run blockdev.Runner

	// ExportsDir is the drop-in directory, conventionally /etc/exports.d.
	ExportsDir string

	// Attach makes the namespace present on this host; Detach gives it up.
	// Both optional: nil assembles against a device that is already there.
	//
	// The metadata server needs one because it is an initiator like any client,
	// and no CSI call ever targets it -- kubelet stages on the nodes running
	// the pods. So unless assembly attaches, nothing on that host will.
	//
	// Injected rather than implemented here: connecting a namespace means
	// asking the control plane where it is served from and running an initiator
	// with this host's identity, and the CSI driver already owns that path.
	Attach func(ctx context.Context, spec Spec) error
	Detach func(ctx context.Context, spec Spec) error
}

// Assembler builds and removes exports on the host it runs on.
type Assembler struct {
	cfg Config
}

// New returns an Assembler, or an error when the configuration cannot work.
func New(cfg Config) (*Assembler, error) {
	switch {
	case cfg.Devices == nil:
		return nil, fmt.Errorf("export: no device resolver: %w", ErrInvalidSpec)
	case cfg.Filesystem == nil:
		return nil, fmt.Errorf("export: no filesystem ops: %w", ErrInvalidSpec)
	case cfg.Content == nil:
		return nil, fmt.Errorf("export: no content reader: %w", ErrInvalidSpec)
	case cfg.Run == nil:
		return nil, fmt.Errorf("export: no command runner: %w", ErrInvalidSpec)
	case cfg.ExportsDir == "":
		return nil, fmt.Errorf("export: no exports directory: %w", ErrInvalidSpec)
	}
	return &Assembler{cfg: cfg}, nil
}

// Create assembles the export, skipping whatever is already done.
//
// The order is forced. Publishing last is what makes a half-assembled export
// invisible to clients rather than briefly broken for them.
func (a *Assembler) Create(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	// Attach first: everything below looks for a device that is not there until
	// this has run, and a not-found would name the wrong problem.
	if a.cfg.Attach != nil {
		if err := a.cfg.Attach(ctx, spec); err != nil {
			return fmt.Errorf("export %s: attaching the namespace: %w", spec.Path, err)
		}
	}

	device, err := a.cfg.Devices.ByUUID(ctx, spec.VolumeUUID)
	if err != nil {
		return fmt.Errorf("export %s: finding device uuid=%s: %w", spec.Path, spec.VolumeUUID, err)
	}
	devPath := device.Namespace.DevicePath
	if devPath == "" {
		return fmt.Errorf("export %s: device uuid=%s has no block node: %w",
			spec.Path, spec.VolumeUUID, errs.ErrNotFound)
	}

	// Format only a blank device. One that already holds a filesystem is this
	// export re-entered, or somebody else's; formatting either destroys data.
	blank, err := a.blank(ctx, spec, devPath)
	if err != nil {
		return err
	}
	if blank {
		if err := a.cfg.Filesystem.Format(ctx, devPath, FSType, nil); err != nil {
			return fmt.Errorf("export %s: mkfs.%s on %s: %w", spec.Path, FSType, devPath, err)
		}
	}

	if err := os.MkdirAll(spec.Path, 0o755); err != nil {
		return fmt.Errorf("export %s: creating the mount point: %w", spec.Path, err)
	}
	mounted, err := a.cfg.Filesystem.IsMountPoint(ctx, spec.Path)
	if err != nil {
		return fmt.Errorf("export %s: checking the mount point: %w", spec.Path, err)
	}
	if !mounted {
		if err := a.cfg.Filesystem.Mount(ctx, devPath, spec.Path, FSType, nil); err != nil {
			return fmt.Errorf("export %s: mounting %s: %w", spec.Path, devPath, err)
		}
	}

	// Grow onto whatever the device now is. The control plane can enlarge the
	// logical volume under a live export, and no CSI call ever reaches this
	// host to grow the filesystem after it -- kubelet stages on the nodes
	// running the pods. Assembly is the only thing here, and it is re-entered
	// whenever the record changes.
	//
	// Unconditional: xfs_growfs on a filesystem already filling its device is
	// a no-op, and a conditional would need a size this package cannot learn.
	if err := a.grow(ctx, spec.Path); err != nil {
		return err
	}

	if err := a.writeDropIn(spec); err != nil {
		return err
	}
	return a.reexport(ctx, spec.Path)
}

// Delete tears the export down in reverse, treating an already-absent step as
// done. The caller is a finalizer, so it has to converge.
func (a *Assembler) Delete(ctx context.Context, spec Spec) error {
	if spec.Path == "" || spec.FSID == "" {
		return fmt.Errorf("export: delete needs a path and an fsid: %w", ErrInvalidSpec)
	}

	// Unpublish first, so no new client arrives while the filesystem is being
	// taken away underneath.
	dropIn := filepath.Join(a.cfg.ExportsDir, spec.dropInName())
	if err := os.Remove(dropIn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("export %s: removing %s: %w", spec.Path, dropIn, err)
	}
	if err := a.reexport(ctx, spec.Path); err != nil {
		return err
	}

	mounted, err := a.cfg.Filesystem.IsMountPoint(ctx, spec.Path)
	if err != nil {
		return fmt.Errorf("export %s: checking the mount point: %w", spec.Path, err)
	}
	if mounted {
		if err := a.cfg.Filesystem.Unmount(ctx, spec.Path); err != nil {
			return fmt.Errorf("export %s: unmounting: %w", spec.Path, err)
		}
	}

	// A non-empty directory holds something this package did not put there.
	if err := os.Remove(spec.Path); err != nil &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrExist) {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return fmt.Errorf("export %s: removing the mount point: %w", spec.Path, err)
		}
	}

	// Detach last. Giving up the namespace under a live mount leaves a
	// filesystem over a device that is gone: an EIO every process in it has to
	// be killed to clear.
	if a.cfg.Detach != nil {
		if err := a.cfg.Detach(ctx, spec); err != nil {
			return fmt.Errorf("export %s: detaching the namespace: %w", spec.Path, err)
		}
	}
	return nil
}

// writeDropIn writes the exports entry, replacing whatever was there. Written
// and renamed, because exportfs parses the directory and a torn write publishes
// a malformed export rather than none.
func (a *Assembler) writeDropIn(spec Spec) error {
	if err := os.MkdirAll(a.cfg.ExportsDir, 0o755); err != nil {
		return fmt.Errorf("export %s: creating %s: %w", spec.Path, a.cfg.ExportsDir, err)
	}
	final := filepath.Join(a.cfg.ExportsDir, spec.dropInName())
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(spec.line()), 0o644); err != nil {
		return fmt.Errorf("export %s: writing %s: %w", spec.Path, tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("export %s: renaming %s: %w", spec.Path, tmp, err)
	}
	return nil
}

// ContentReader is the probe that says what a device carries, which is the
// seam the volstack filesystem layer takes too.
type ContentReader interface {
	Read(ctx context.Context, dev blockdev.Device) (blockdev.Reading, error)
}

// blank reports whether the device may be formatted.
//
// On an encrypted volume an unrecognized reading is no evidence of anything:
// the control plane stacks a crypto bdev under the namespace, so a
// never-written block arrives decrypted from zeros as pseudo-random plaintext,
// which reads exactly like somebody else's data. A stack layer is still
// refused, because a physical-volume or RAID label is a signature the probe
// positively recognized, and finding one means the plan is wrong rather than
// that the bytes were undecipherable.
func (a *Assembler) blank(ctx context.Context, spec Spec, devPath string) (bool, error) {
	reading, err := a.cfg.Content.Read(ctx, blockdev.Device{Path: devPath})
	if err != nil {
		return false, fmt.Errorf("export %s: probing %s: %w", spec.Path, devPath, err)
	}
	switch {
	case reading.Content == blockdev.ContentBlank:
		return true, nil
	case reading.Content == blockdev.ContentFilesystem:
		return false, nil
	case spec.Encrypted && reading.Content != blockdev.ContentStackLayer:
		return true, nil
	}
	return false, fmt.Errorf("export %s: refusing to format %s, which carries %s: %s: %w",
		spec.Path, devPath, reading.Content, reading.Detail, ErrInvalidSpec)
}

// grow expands the filesystem to fill its device.
func (a *Assembler) grow(ctx context.Context, path string) error {
	out, code, err := a.cfg.Run(ctx, "xfs_growfs", path)
	if err != nil {
		return fmt.Errorf("export %s: xfs_growfs: %w", path, err)
	}
	if code != 0 {
		return fmt.Errorf("export %s: xfs_growfs exited %d: %s",
			path, code, strings.TrimSpace(string(out)))
	}
	return nil
}

// reexport asks nfsd to re-read the export table.
func (a *Assembler) reexport(ctx context.Context, path string) error {
	out, code, err := a.cfg.Run(ctx, "exportfs", "-ra")
	if err != nil {
		return fmt.Errorf("export %s: exportfs -ra: %w", path, err)
	}
	if code != 0 {
		return fmt.Errorf("export %s: exportfs -ra exited %d: %s",
			path, code, strings.TrimSpace(string(out)))
	}
	return nil
}
