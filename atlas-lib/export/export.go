// Package export publishes a pNFS export on its metadata-server host.
//
// An export is a volume stack (the namespace attached, formatted, and mounted)
// with an exports(5) entry in front of it. The stack is the same one the block
// path stages: the consumer builds the plan, volstack's runner walks it, and
// what is left here is the publishing.
package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
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
	// VolumeUUID identifies the backing namespace, which for a simplyblock
	// volume is the logical volume's own id. Not a device path: the kernel
	// assigns those in attach order, so they differ across hosts.
	VolumeUUID string

	// ClusterID and PoolID are where the planner asks after the namespace.
	ClusterID string
	PoolID    string

	// Path is the mount point and the exported directory.
	Path string

	// FSID is what the export is published under. Stable across hosts, so a
	// re-materialized export reproduces the file handles clients hold.
	FSID string

	// Encrypted says the control plane stacks a crypto bdev under this
	// namespace, which makes an unwritten block read as pseudo-random plaintext
	// rather than as zeros. The filesystem layer needs it, or an empty
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

// StackHandle is what this export's stack record is keyed by. Prefixed so it
// cannot collide with a volume the node plugin staged, which records into the
// same directory.
func (s Spec) StackHandle() string { return "pnfs-" + s.FSID }

// line is the exports(5) entry for this export.
func (s Spec) line() string {
	opts := append(append([]string{}, exportOptions...), "fsid="+s.FSID)
	clients := make([]string, 0, len(s.Clients))
	for _, c := range s.Clients {
		clients = append(clients, c+"("+strings.Join(opts, ",")+")")
	}
	return s.Path + " " + strings.Join(clients, " ") + "\n"
}

// Runner is the subset of volstack.Runner an export walks its stack with.
type Runner interface {
	Up(ctx context.Context, handle string, plan volstack.Plan) (volstack.Artifact, error)
	Grow(ctx context.Context, plan volstack.Plan) error
	Down(ctx context.Context, handle string, plan volstack.Plan) error
}

// Config is what an Assembler needs to reach the host.
type Config struct {
	// Plan is the stack this export sits on: the namespace attached and its
	// filesystem mounted at spec.Path.
	//
	// Injected rather than built here, because resolving where a namespace is
	// published means asking the control plane, and the consumer owns that
	// client. It is called on the teardown path too, which is why it has to
	// answer for a volume this host may no longer reach.
	Plan func(ctx context.Context, spec Spec) (volstack.Plan, error)

	// Stack walks that plan.
	Stack Runner

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
	case cfg.Plan == nil:
		return nil, fmt.Errorf("export: no stack planner: %w", ErrInvalidSpec)
	case cfg.Stack == nil:
		return nil, fmt.Errorf("export: no stack runner: %w", ErrInvalidSpec)
	case cfg.Run == nil:
		return nil, fmt.Errorf("export: no command runner: %w", ErrInvalidSpec)
	case cfg.ExportsDir == "":
		return nil, fmt.Errorf("export: no exports directory: %w", ErrInvalidSpec)
	}
	return &Assembler{cfg: cfg}, nil
}

// Create assembles the export. Every step converges, because the caller is a
// reconciler that runs this on each pass.
func (a *Assembler) Create(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	plan, err := a.cfg.Plan(ctx, spec)
	if err != nil {
		return fmt.Errorf("export %s: planning the stack: %w", spec.Path, err)
	}
	if _, err := a.cfg.Stack.Up(ctx, spec.StackHandle(), plan); err != nil {
		return fmt.Errorf("export %s: bringing the stack up: %w", spec.Path, err)
	}

	// The control plane can enlarge the volume under a live export, and no CSI
	// call ever reaches this host to grow the filesystem after it: kubelet
	// stages on the nodes running the pods. Assembly is the only thing here.
	if err := a.cfg.Stack.Grow(ctx, plan); err != nil {
		return fmt.Errorf("export %s: growing onto the volume: %w", spec.Path, err)
	}

	// Published last, which is what makes a half-assembled export invisible to
	// clients rather than briefly broken for them.
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

	plan, err := a.cfg.Plan(ctx, spec)
	if err != nil {
		return fmt.Errorf("export %s: planning the stack: %w", spec.Path, err)
	}
	// Down and never Destroy: the volume is the control plane's, and this host
	// only ever held it.
	if err := a.cfg.Stack.Down(ctx, spec.StackHandle(), plan); err != nil {
		return fmt.Errorf("export %s: taking the stack down: %w", spec.Path, err)
	}

	// A non-empty directory holds something this package did not put there.
	if err := os.Remove(spec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return fmt.Errorf("export %s: removing the mount point: %w", spec.Path, err)
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
