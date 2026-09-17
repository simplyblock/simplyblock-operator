// Package export assembles and tears down a pNFS export on the host that acts
// as its metadata server.
//
// An export is a filesystem on one NVMe-oF namespace, mounted at a path, and
// published through the host's nfsd. Assembling one is four steps and tearing it
// down is those four in reverse, and both have to be safe to re-run: the caller
// is a reconciler, so every step is skipped when it is already satisfied rather
// than repeated.
//
// It lives in atlas rather than in the CSI driver because none of it is
// Kubernetes-shaped. Finding a device by its NGUID, deciding whether a device is
// blank, mounting it, and writing an exports drop-in are node-level operations,
// and the operator reaches them over a link rather than owning them.
//
// What this package does not do is attach the namespace. The device is expected
// to be present, because connecting it is the CSI driver's existing NVMe-oF
// path, with its own reconnect and monitoring, and a second connect
// implementation is the thing that path exists to avoid.
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

// ErrInvalidSpec is a spec that cannot be assembled whatever the host does. It
// is local rather than in errs because it is about this package's own input:
// errs carries the sentinels that cross a boundary, and this one does not.
var ErrInvalidSpec = errors.New("invalid export spec")

// FSType is the only filesystem a pNFS SCSI layout can be served from. XFS is
// not a default here, it is the whole mechanism: no other Linux filesystem
// implements the export operations nfsd needs to hand out block layouts.
const FSType = "xfs"

// exportOptions are the options every export carries.
//
// `pnfs` is what makes nfsd offer a layout at all. `sync` is deliberate rather
// than inherited: an export whose writes are acknowledged before they land
// cannot promise what a shared filesystem has to promise.
var exportOptions = []string{"rw", "sync", "no_subtree_check", "no_root_squash", "pnfs"}

// Spec is one export to assemble: which device, where it goes, and who may
// mount it.
type Spec struct {
	// NGUID identifies the backing namespace. It is the identifier rather than
	// a device path because a path is assigned by the kernel in attach order
	// and is not the same across hosts, while this is a property of the
	// namespace itself.
	NGUID string

	// Path is the mount point and the exported directory. It carries namespace
	// and volume information, because a PVC name is unique only within a
	// namespace and two same-named claims must not collide on one host.
	Path string

	// FSID is what the export is published under. Holding it stable across
	// hosts is what lets a re-materialized export reproduce the file handles
	// clients already hold.
	FSID string

	// Clients is the set allowed to mount, as exports(5) spells it: addresses,
	// CIDRs, or a single "*." An empty set is refused rather than widened,
	// because the widening would be to everyone.
	Clients []string
}

// Validate reports whether the spec can be assembled at all.
func (s Spec) Validate() error {
	switch {
	case s.NGUID == "":
		return fmt.Errorf("export: no NGUID: %w", ErrInvalidSpec)
	case s.Path == "":
		return fmt.Errorf("export: no path: %w", ErrInvalidSpec)
	case s.FSID == "":
		return fmt.Errorf("export: no fsid: %w", ErrInvalidSpec)
	case len(s.Clients) == 0:
		// Defaulting to "*" here would publish the filesystem to every host
		// that can reach the MDS, which is a decision for the caller to take
		// deliberately rather than for this package to take by omission.
		return fmt.Errorf("export %s: no allowed clients: %w", s.Path, ErrInvalidSpec)
	}
	return nil
}

// dropInName is the exports.d file this export owns. It is derived from the
// FSID rather than the path because the path contains separators and the FSID
// is already unique per export.
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

// Filesystem is the mounting and formatting an export needs. It is the subset
// of volstack's FilesystemOps that applies, named separately so a caller passes
// what it already has.
type Filesystem interface {
	Format(ctx context.Context, device, fsType string, options []string) error
	Mount(ctx context.Context, source, target, fsType string, options []string) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(ctx context.Context, path string) (bool, error)
}

// Config is what an Assembler needs to reach the host.
type Config struct {
	// Devices resolves the backing namespace. The by-NGUID lookup is the one
	// that matters: the layout an MDS hands out names the device by NGUID.
	Devices nvme.DeviceResolver

	// Filesystem formats, mounts, and unmounts.
	Filesystem Filesystem

	// Blank reports whether a device carries no filesystem. Formatting a device
	// that already has one destroys it, so this is asked every time rather than
	// inferred from whether this host has seen the device before.
	Blank func(ctx context.Context, devicePath string) (bool, error)

	// Run executes exportfs. Injected so a test does not need one.
	Run blockdev.Runner

	// ExportsDir is the drop-in directory, conventionally /etc/exports.d.
	ExportsDir string
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
	case cfg.Blank == nil:
		return nil, fmt.Errorf("export: no blank-device check: %w", ErrInvalidSpec)
	case cfg.Run == nil:
		return nil, fmt.Errorf("export: no command runner: %w", ErrInvalidSpec)
	case cfg.ExportsDir == "":
		return nil, fmt.Errorf("export: no exports directory: %w", ErrInvalidSpec)
	}
	return &Assembler{cfg: cfg}, nil
}

// Create assembles the export, skipping whatever is already done.
//
// The order is forced by what depends on what: a filesystem cannot be made on a
// device that is not there, cannot be mounted before it is made, and cannot be
// exported before it is mounted. Publishing last is what makes a partially
// assembled export invisible to clients rather than briefly broken for them.
func (a *Assembler) Create(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	device, err := a.cfg.Devices.ByNGUID(ctx, spec.NGUID)
	if err != nil {
		return fmt.Errorf("export %s: finding device nguid=%s: %w", spec.Path, spec.NGUID, err)
	}
	devPath := device.Namespace.DevicePath
	if devPath == "" {
		return fmt.Errorf("export %s: device nguid=%s has no block node: %w",
			spec.Path, spec.NGUID, errs.ErrNotFound)
	}

	// Format only a device that carries nothing. A device that already holds a
	// filesystem is one this export was assembled on before, or one that
	// belongs to something else, and formatting either destroys data.
	blank, err := a.cfg.Blank(ctx, devPath)
	if err != nil {
		return fmt.Errorf("export %s: probing %s: %w", spec.Path, devPath, err)
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

	if err := a.writeDropIn(spec); err != nil {
		return err
	}
	return a.reexport(ctx, spec.Path)
}

// Delete tears the export down in reverse, and treats every already-absent step
// as done. The caller is a finalizer, so this has to converge rather than fail
// on a host that has already lost the mount.
func (a *Assembler) Delete(ctx context.Context, spec Spec) error {
	if spec.Path == "" || spec.FSID == "" {
		return fmt.Errorf("export: delete needs a path and an fsid: %w", ErrInvalidSpec)
	}

	// Unpublish first. A client that still holds the mount keeps working until
	// it is unmounted, but no new client can arrive while the filesystem is
	// being taken away underneath.
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

	// A non-empty directory means something is still there that this package
	// did not put there, so it is left alone rather than forced.
	if err := os.Remove(spec.Path); err != nil &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrExist) {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return fmt.Errorf("export %s: removing the mount point: %w", spec.Path, err)
		}
	}
	return nil
}

// writeDropIn writes the exports entry, replacing whatever was there. It is
// written to a temporary file and renamed, so a reader never sees half a line:
// exportfs parses the directory, and a torn write would publish a malformed
// export rather than none.
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
