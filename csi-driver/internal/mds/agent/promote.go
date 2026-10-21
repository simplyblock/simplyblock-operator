// The guest's role machine: a guest boots as a standby and becomes the active
// metadata server only through Promote, at most once per boot
// (design-pnfs-mds-standby.md §5.3, §7.3 step 4).

package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// ErrNotActive refuses a CreateExport on a guest that is not the active one.
// It carries gRPC's FailedPrecondition, a permanent code the runner passes on
// without retrying.
var ErrNotActive = status.Error(codes.FailedPrecondition, "the guest is not the active metadata server")

// Connector connects one volume as hostNQN over every path of conn and returns
// the namespace's block device.
type Connector interface {
	Connect(ctx context.Context, conn lvol.Connection, hostNQN string) (blockdev.Device, error)
}

// StateMounter mounts the state volume's device at dir, formatting it first
// when it carries no filesystem.
type StateMounter interface {
	Mount(ctx context.Context, dev blockdev.Device, dir string) error
	Unmount(ctx context.Context, dir string) error
}

// KeyWriter sets nfsd's reservation key before nfsd starts.
type KeyWriter interface {
	Write(ctx context.Context, key uint64) error
}

// Units starts and stops the guest's units and runs its tmpfiles.
type Units interface {
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	// CreateTmpfiles runs systemd-tmpfiles --create on conf.
	CreateTmpfiles(ctx context.Context, conf string) error
}

// Tracker keeps a connection's paths up for the guest's path repair.
type Tracker interface {
	Track(name string, conn lvol.Connection, hostNQN string)
	Forget(name string)
}

// GuestConfig is everything the role machine touches outside itself.
type GuestConfig struct {
	Connector Connector
	Open      Opener
	HostID    func(hostNQN string) ([16]byte, error)
	State     StateMounter
	Key       KeyWriter
	Units     Units
	Tracker   Tracker
	StateDir  string
	Log       Logger
}

// Units Promote starts, in this order: nfsd reads the client-recovery database
// through nfsdcld the moment its threads start.
const (
	unitNFSDCLD = "nfsdcld.service"
	unitNFSD    = "nfs-server.service"
	// stateName is the path keeper's name for the state volume. An export's
	// name is its absolute path, so the two cannot collide.
	stateName = "state-volume"

	// tmpfilesConf recreates what the state mount hides: the exports directory,
	// the empty pnfs-root.exports on the state volume, and the link to it from
	// /run/exports.d. All three must exist before nfsd starts.
	tmpfilesConf = "/usr/lib/tmpfiles.d/pnfs.conf"
)

// Guest is the role machine. It implements nfsexportrpc.MDS.
//
// promoteMu serializes Promote calls. mu guards the fields Describe reads, so
// Describe answers Promoting while a promotion runs.
type Guest struct {
	cfg       GuestConfig
	promoteMu sync.Mutex

	mu        sync.Mutex
	role      nfsexportrpc.Role
	epoch     uint64
	key       uint64
	attempted uint64
	hostNQN   string
	held      map[string]bool
}

// NewGuest returns a standby guest.
func NewGuest(cfg GuestConfig) *Guest {
	return &Guest{cfg: cfg, role: nfsexportrpc.RoleStandby, held: map[string]bool{}}
}

// Role returns the guest's current role.
func (g *Guest) Role() nfsexportrpc.Role {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.role
}

// Describe reports the role, the accepted epoch, and the key.
func (g *Guest) Describe(context.Context) (nfsexportrpc.Description, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return nfsexportrpc.Description{Role: g.role, Epoch: g.epoch, Key: g.key}, nil
}

func (g *Guest) logf(format string, args ...any) {
	if g.cfg.Log != nil {
		g.cfg.Log.Infof(format, args...)
	}
}

// Promote turns a standby into the active guest: connect every volume, fence
// every device, mount the state volume, set nfsd's key, start nfsd. It is
// accepted at most once per boot. A failure returns the guest to a standby,
// and devices already fenced stay fenced.
func (g *Guest) Promote(ctx context.Context, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
	if err := validatePromote(req); err != nil {
		return nfsexportrpc.PromoteResult{}, err
	}
	g.promoteMu.Lock()
	defer g.promoteMu.Unlock()

	if res, done := g.admit(req); done {
		return res, nil
	}
	res, err := g.run(ctx, req)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil || res.Outcome != nfsexportrpc.PromoteActive {
		g.role, g.epoch, g.key = nfsexportrpc.RoleStandby, 0, 0
		return res, err
	}
	g.role = nfsexportrpc.RoleActive
	return res, nil
}

func validatePromote(req nfsexportrpc.PromoteRequest) error {
	if req.Epoch == 0 || req.HostNQN == "" {
		return fmt.Errorf("promote: an epoch and a host NQN are required")
	}
	want, err := export.MDSReservationKey(req.StatefulSetUID, req.Epoch)
	if err != nil {
		return fmt.Errorf("promote: deriving the key of epoch %d: %w", req.Epoch, err)
	}
	if req.Key != want {
		return fmt.Errorf("promote: key %x is not the key of epoch %d (%x)", req.Key, req.Epoch, want)
	}
	if req.StateVolume.Connection == nil {
		return fmt.Errorf("promote: the state volume carries no connection")
	}
	for _, e := range req.Exports {
		if e.Connection == nil {
			return fmt.Errorf("promote: export %s carries no connection", e.Path)
		}
	}
	return nil
}

// admit decides whether this call may run, and answers it when it may not. A
// call it admits has moved the guest to Promoting.
func (g *Guest) admit(req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	stale := nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteStaleEpoch,
		Message: fmt.Sprintf("epoch %d, the guest is at %d", req.Epoch, max(g.epoch, g.attempted))}
	switch {
	case g.role == nfsexportrpc.RoleActive && req.Epoch == g.epoch:
		return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteActive}, true
	case g.role == nfsexportrpc.RoleActive, req.Epoch < g.attempted:
		return stale, true
	}
	g.role, g.epoch, g.key = nfsexportrpc.RolePromoting, req.Epoch, req.Key
	g.attempted = req.Epoch
	g.hostNQN = req.HostNQN
	return nfsexportrpc.PromoteResult{}, false
}

func fenceResult(e *FenceError) nfsexportrpc.PromoteResult {
	return nfsexportrpc.PromoteResult{Outcome: e.Outcome, Device: e.Device, Step: e.Step, Message: e.Error()}
}

// volume is one device of a promotion: the name the path keeper keeps it
// under, its connection, and the device once connected.
type volume struct {
	name string
	conn lvol.Connection
	dev  blockdev.Device
}

func (g *Guest) run(ctx context.Context, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
	hostID, err := g.cfg.HostID(req.HostNQN)
	if err != nil {
		return nfsexportrpc.PromoteResult{}, fmt.Errorf("promote: the host id of %s: %w", req.HostNQN, err)
	}
	vols := []*volume{{name: stateName, conn: *req.StateVolume.Connection}}
	for _, e := range req.Exports {
		vols = append(vols, &volume{name: e.Path, conn: *e.Connection})
	}

	// Connect everything first: a device that does not connect is found before
	// anything is fenced.
	for _, v := range vols {
		dev, err := g.cfg.Connector.Connect(ctx, v.conn, req.HostNQN)
		if err != nil {
			return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteFenceFailed,
				Device: v.conn.NQN, Step: "connect", Message: err.Error()}, nil
		}
		v.dev = dev
	}

	paths := make([]string, len(vols))
	for i, v := range vols {
		paths[i] = v.dev.Path
	}
	fencer := &Fencer{Open: g.cfg.Open, HostID: hostID, Key: req.Key, Log: g.cfg.Log}
	if ferr := fencer.Fence(ctx, paths); ferr != nil {
		return fenceResult(ferr), nil
	}

	for _, v := range vols {
		g.cfg.Tracker.Track(v.name, v.conn, req.HostNQN)
	}
	if err := g.start(ctx, req, vols[0].dev); err != nil {
		for _, v := range vols {
			g.cfg.Tracker.Forget(v.name)
		}
		return nfsexportrpc.PromoteResult{}, err
	}

	g.mu.Lock()
	for _, v := range vols[1:] {
		g.held[v.name] = true
	}
	g.mu.Unlock()
	g.logf("promoted at epoch %d with key %x", req.Epoch, req.Key)
	return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteActive}, nil
}

// start is what follows the fence: mount the state volume, set the key, start
// nfsd. A failure undoes what it did, so the standby is as it booted.
func (g *Guest) start(ctx context.Context, req nfsexportrpc.PromoteRequest, state blockdev.Device) error {
	if err := g.cfg.State.Mount(ctx, state, g.cfg.StateDir); err != nil {
		return fmt.Errorf("promote: mounting the state volume: %w", err)
	}
	undo := func() {
		if err := g.cfg.State.Unmount(ctx, g.cfg.StateDir); err != nil {
			g.logf("promote: unmounting the state volume after a failure: %v", err)
		}
	}
	if err := g.cfg.Units.CreateTmpfiles(ctx, tmpfilesConf); err != nil {
		undo()
		return fmt.Errorf("promote: recreating the state volume's files: %w", err)
	}
	if err := g.cfg.Key.Write(ctx, req.Key); err != nil {
		undo()
		return fmt.Errorf("promote: setting nfsd's key: %w", err)
	}
	for _, unit := range []string{unitNFSDCLD, unitNFSD} {
		if err := g.cfg.Units.Start(ctx, unit); err != nil {
			if unit == unitNFSD {
				_ = g.cfg.Units.Stop(ctx, unit)
			}
			undo()
			return fmt.Errorf("promote: starting %s: %w", unit, err)
		}
	}
	return nil
}

// Guard refuses CreateExport while the guest is not active, so no reconcile
// can make a standby mount. On an active guest an export the guest does not
// hold yet is fenced before the assembler makes its filesystem. An export it
// holds is passed through and fences nothing: a guest that was fenced must not
// register again. CheckExport and DeleteExport are always allowed.
func (g *Guest) Guard(inner Assembler) Assembler { return activeGuard{g: g, inner: inner} }

type activeGuard struct {
	g     *Guest
	inner Assembler
}

func (a activeGuard) Create(ctx context.Context, spec export.Spec) error {
	g := a.g
	g.mu.Lock()
	role, held, hostNQN, key := g.role, g.held[spec.Path], g.hostNQN, g.key
	g.mu.Unlock()
	if role != nfsexportrpc.RoleActive {
		return ErrNotActive
	}
	if !held {
		if err := g.fenceNew(ctx, spec, hostNQN, key); err != nil {
			return err
		}
	}
	if err := a.inner.Create(ctx, spec); err != nil {
		return err
	}
	g.mu.Lock()
	g.held[spec.Path] = true
	g.mu.Unlock()
	return nil
}

func (g *Guest) fenceNew(ctx context.Context, spec export.Spec, hostNQN string, key uint64) error {
	if spec.Connection == nil {
		return fmt.Errorf("export %s: a new export carries no connection to fence it through", spec.Path)
	}
	hostID, err := g.cfg.HostID(hostNQN)
	if err != nil {
		return fmt.Errorf("export %s: the host id of %s: %w", spec.Path, hostNQN, err)
	}
	dev, err := g.cfg.Connector.Connect(ctx, *spec.Connection, hostNQN)
	if err != nil {
		return fmt.Errorf("export %s: connecting before the fence: %w", spec.Path, err)
	}
	fencer := &Fencer{Open: g.cfg.Open, HostID: hostID, Key: key, Log: g.cfg.Log}
	if ferr := fencer.Fence(ctx, []string{dev.Path}); ferr != nil {
		return fmt.Errorf("export %s: %w", spec.Path, ferr)
	}
	return nil
}

func (a activeGuard) Delete(ctx context.Context, spec export.Spec) error {
	if err := a.inner.Delete(ctx, spec); err != nil {
		return err
	}
	a.g.mu.Lock()
	delete(a.g.held, spec.Path)
	a.g.mu.Unlock()
	return nil
}

func (a activeGuard) Check(ctx context.Context, spec export.Spec) error {
	return a.inner.Check(ctx, spec)
}

var _ nfsexportrpc.MDS = (*Guest)(nil)
var _ = errors.New
