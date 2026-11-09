// The node's half of moving a staged volume's namespace to another NVMe-oF
// subsystem under a live consumer: hold the volume's I/O on its dm-linear
// mapping, re-point the mapping once the namespace is under the new subsystem,
// and never leave it held past a deadline (design-cg-live-subsystem-flip.md §6).
//
// It is node-level and not Kubernetes-shaped, so it lives in atlas-lib beside
// the volume stack it re-points, and the gRPC service in this package is a thin
// layer over it.

package swaprpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/simplyblock/atlas/devmapper"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

var (
	// ErrNotSwappable reports a volume whose stack has no dm-linear mapping to
	// re-point, or that is not staged on this node at all.
	ErrNotSwappable = errors.New("swap: the volume is not staged behind the device-mapper indirection")
	// ErrDeadlinePassed reports a suspend asked for after the window it was
	// meant to fit in had already closed.
	ErrDeadlinePassed = errors.New("swap: the suspend deadline has already passed")
	// ErrNamespaceNotFound reports a namespace that did not appear under the
	// subsystem it was said to be in before the volume's deadline.
	ErrNamespaceNotFound = errors.New("swap: the namespace did not appear under the subsystem")
)

// ReasonWatchdogFired is the event a node emits when it resumed a volume that
// nobody resumed before its deadline.
const ReasonWatchdogFired = "SubsystemSwapWatchdogFired"

// Watchdog outcomes, by where the watchdog found the volume's namespace.
const (
	OutcomeTarget = "target"
	OutcomeSource = "source"
	OutcomeNone   = "none"
)

const (
	defaultGrace        = 5 * time.Second
	defaultPollInterval = 200 * time.Millisecond
	// defaultResumeWait bounds a resume of a volume this process never
	// suspended, which has no deadline of its own.
	defaultResumeWait = 30 * time.Second
	mappingPrefix     = "sb-"
)

// VolumeState is one volume as the node sees it.
type VolumeState struct {
	// Handle is the CSI volume handle.
	Handle string
	// Staged reports that the node holds a stack record for the volume.
	Staged bool
	// Swappable reports that the stack has a dm-linear mapping to re-point.
	Swappable bool
	// Suspended reports that the mapping is holding I/O.
	Suspended bool
	// NQN is the subsystem whose namespace the mapping points at.
	NQN string
	// NamespaceUUID is the volume's namespace UUID.
	NamespaceUUID string
}

// Timer is a pending watchdog.
type Timer interface{ Stop() bool }

// Mapper is the device-mapper surface a swap uses. *devmapper.Mapper is the
// real one.
type Mapper interface {
	Table(ctx context.Context, name string) (devmapper.Target, error)
	Sectors(ctx context.Context, device string) (uint64, error)
	Suspend(ctx context.Context, name string) error
	ReloadResume(ctx context.Context, name, device string, sectors uint64) error
	Resume(ctx context.Context, name string) error
	Suspended(ctx context.Context, name string) (bool, error)
	List(ctx context.Context, prefix string) ([]devmapper.Mapping, error)
}

// Records is the node's stack records. *volstack.Store is the real one.
type Records interface {
	Load(handle string) (volstack.Record, error)
	Write(rec volstack.Record) error
	List() ([]volstack.Record, error)
}

// Events reports what a user has to know about a volume, by its handle.
type Events interface {
	Warning(handle, reason, message string)
}

// Metrics observes the swaps. Any method may be a no-op.
type Metrics interface {
	SetSuspended(n int)
	ObserveRescan(d time.Duration)
	Watchdog(outcome string)
}

// Config is what a Swapper is built from.
type Config struct {
	Records   Records
	Mapper    Mapper
	Devices   nvme.DeviceResolver
	Connector nvmeof.Connector
	// Rescan asks the controllers of a subsystem to look for new namespaces
	// (`nvme ns-rescan`).
	Rescan func(ctx context.Context, nqn string) error
	// DropStalePaths disconnects the controllers of a subsystem that are not
	// live, and reports how many it dropped. Nil drops nothing.
	DropStalePaths func(ctx context.Context, nqn string) (int, error)

	// Events and Metrics are optional.
	Events  Events
	Metrics Metrics

	// Lock serializes this service's work on one volume with the node's own
	// operations on it (stage, unstage, publish, expand), which share the
	// mapping and the record a swap rewrites. It takes the volume's handle and
	// returns the release. It is held for one call's work on the volume, never
	// across the suspend window; the window is guarded by Held. Nil locks
	// nothing.
	Lock func(handle string) func()

	// HostNQN and HostID are this node's identity, presented on every connect.
	HostNQN string
	HostID  string

	// Now, AfterFunc, and Sleep default to the real clock.
	Now       func() time.Time
	AfterFunc func(d time.Duration, fn func()) Timer
	Sleep     func(d time.Duration)

	// PollInterval is how often a resume looks for a namespace that has not
	// appeared yet, and Grace is how long past the deadline the watchdog waits.
	PollInterval time.Duration
	Grace        time.Duration
}

// held is a volume this process suspended and has not resumed.
type held struct {
	deadline time.Time
	timer    Timer
}

// Swapper moves the volumes staged on one node between subsystems.
type Swapper struct {
	cfg Config

	mu sync.Mutex
	// targets is the subsystem each volume was prepared toward, which the
	// watchdog prefers when it finds the namespace there.
	targets map[string]string
	held    map[string]*held
}

// New checks the seams that have no default.
func New(cfg Config) (*Swapper, error) {
	if cfg.Records == nil || cfg.Mapper == nil || cfg.Devices == nil || cfg.Connector == nil || cfg.Rescan == nil {
		return nil, errors.New("swap: records, mapper, devices, connector, and rescan are all required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Grace <= 0 {
		cfg.Grace = defaultGrace
	}
	return &Swapper{cfg: cfg, targets: map[string]string{}, held: map[string]*held{}}, nil
}

// volume is what the record says about one staged volume.
type volume struct {
	handle  string
	staged  bool
	mapping string // empty when the stack has no indirection
	uuid    string
	fabric  layers.FabricParams
	record  volstack.Record
	fabricI int
}

func (s *Swapper) load(handle string) (volume, error) {
	rec, err := s.cfg.Records.Load(handle)
	if errors.Is(err, volstack.ErrNoRecord) {
		return volume{handle: handle}, nil
	}
	if err != nil {
		return volume{}, err
	}
	v := volume{handle: handle, staged: true, record: rec, fabricI: -1}
	for i, e := range rec.Plan {
		switch e.Layer {
		case "fabric":
			if json.Unmarshal(e.Params, &v.fabric) == nil {
				v.fabricI = i
			}
		case "dmLinear":
			var p layers.DMLinearParams
			if json.Unmarshal(e.Params, &p) == nil && strings.HasPrefix(p.Name, mappingPrefix) {
				v.mapping = p.Name
				v.uuid = strings.TrimPrefix(p.Name, mappingPrefix)
			}
		}
	}
	return v, nil
}

func (v volume) swappable() bool { return v.mapping != "" && v.fabricI >= 0 }

// lock takes the node's lock on handle, when the service was given one.
func (s *Swapper) lock(handle string) func() {
	if s.cfg.Lock == nil {
		return func() {}
	}
	return s.cfg.Lock(handle)
}

// Held reports whether this process is holding handle's I/O: suspended by a
// Suspend and not yet resumed by a Resume or the watchdog. The node refuses
// its own operations on such a volume, because they would resume or remove the
// mapping under the swap (a grow reloads it, a heal re-points it, an unstage
// removes it), and because they would block on it.
func (s *Swapper) Held(handle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, held := s.held[handle]
	return held
}

// state describes v as the kernel has it now.
func (s *Swapper) state(ctx context.Context, v volume) (VolumeState, error) {
	st := VolumeState{Handle: v.handle, Staged: v.staged, Swappable: v.swappable(), NamespaceUUID: v.uuid, NQN: v.fabric.NQN}
	if !st.Swappable {
		return st, nil
	}
	suspended, err := s.cfg.Mapper.Suspended(ctx, v.mapping)
	if err != nil {
		return st, fmt.Errorf("swap: %s: %w", v.handle, err)
	}
	st.Suspended = suspended
	if nqn, ok := s.pointsAt(ctx, v); ok {
		st.NQN = nqn
	}
	return st, nil
}

// pointsAt is the subsystem of the namespace device the mapping's table names.
func (s *Swapper) pointsAt(ctx context.Context, v volume) (string, bool) {
	table, err := s.cfg.Mapper.Table(ctx, v.mapping)
	if err != nil {
		return "", false
	}
	found, err := s.cfg.Devices.ListWithSelector(ctx, nvme.DeviceSelector{UUID: v.uuid})
	if err != nil {
		return "", false
	}
	for _, d := range found {
		if d.Namespace.Dev == table.Device {
			return d.Subsystem.NQN, true
		}
	}
	return "", false
}

func (s *Swapper) loadAll(handles []string) ([]volume, error) {
	vols := make([]volume, 0, len(handles))
	for _, h := range handles {
		v, err := s.load(h)
		if err != nil {
			return nil, err
		}
		vols = append(vols, v)
	}
	return vols, nil
}

func (s *Swapper) describe(ctx context.Context, vols []volume) ([]VolumeState, error) {
	out := make([]VolumeState, 0, len(vols))
	for _, v := range vols {
		st, err := s.state(ctx, v)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// Describe reports each volume, in the order asked.
func (s *Swapper) Describe(ctx context.Context, handles []string) ([]VolumeState, error) {
	vols, err := s.loadAll(handles)
	if err != nil {
		return nil, err
	}
	return s.describe(ctx, vols)
}

// Prepare connects the target subsystem's paths, in the order given, as this
// node. It touches no mapping.
func (s *Swapper) Prepare(
	ctx context.Context, handles []string, targetNQN string, paths []nvmeof.Target,
) ([]VolumeState, error) {
	vols, err := s.loadAll(handles)
	if err != nil {
		return nil, err
	}
	targets := make([]nvmeof.Target, len(paths))
	for i, p := range paths {
		p.NQN = targetNQN
		if s.cfg.HostNQN != "" {
			p.HostNQN = s.cfg.HostNQN
		}
		if s.cfg.HostID != "" {
			p.HostID = s.cfg.HostID
		}
		targets[i] = p
	}
	results, err := s.cfg.Connector.ConnectPaths(ctx, targets)
	if err != nil {
		return nil, fmt.Errorf("swap: connect %s: %w", targetNQN, err)
	}
	live := false
	for _, r := range results {
		live = live || r.Live
	}
	if !live {
		return nil, fmt.Errorf("swap: no path to %s is live", targetNQN)
	}
	s.mu.Lock()
	for _, v := range vols {
		s.targets[v.handle] = targetNQN
	}
	s.mu.Unlock()
	return s.describe(ctx, vols)
}

// Suspend holds each volume's I/O and starts its watchdog. Either every volume
// can be suspended or none is: a volume that cannot be swapped refuses the
// whole call before anything is held.
func (s *Swapper) Suspend(ctx context.Context, handles []string, deadline time.Time) ([]VolumeState, error) {
	if !deadline.After(s.cfg.Now()) {
		return nil, ErrDeadlinePassed
	}
	vols, err := s.loadAll(handles)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if !v.swappable() {
			return nil, fmt.Errorf("%s: %w", v.handle, ErrNotSwappable)
		}
	}
	for _, v := range vols {
		unlock := s.lock(v.handle)
		err := s.hold(ctx, v, deadline)
		unlock()
		if err != nil {
			return nil, err
		}
	}
	s.reportHeld()
	return s.describe(ctx, vols)
}

func (s *Swapper) hold(ctx context.Context, v volume, deadline time.Time) error {
	s.mu.Lock()
	h, ok := s.held[v.handle]
	s.mu.Unlock()
	suspended, err := s.cfg.Mapper.Suspended(ctx, v.mapping)
	if err != nil {
		return fmt.Errorf("swap: %s: %w", v.handle, err)
	}
	if ok && suspended && h.deadline.Equal(deadline) {
		return nil
	}
	if !suspended {
		if err := s.cfg.Mapper.Suspend(ctx, v.mapping); err != nil {
			return fmt.Errorf("swap: %s: %w", v.handle, err)
		}
	}
	handle := v.handle
	timer := s.cfg.AfterFunc(deadline.Sub(s.cfg.Now())+s.cfg.Grace, func() { s.fire(handle) })
	s.mu.Lock()
	if old, ok := s.held[v.handle]; ok && old.timer != nil {
		old.timer.Stop()
	}
	s.held[v.handle] = &held{deadline: deadline, timer: timer}
	s.mu.Unlock()
	return nil
}

func (s *Swapper) release(handle string) {
	s.mu.Lock()
	if h, ok := s.held[handle]; ok {
		if h.timer != nil {
			h.timer.Stop()
		}
		delete(s.held, handle)
	}
	delete(s.targets, handle)
	s.mu.Unlock()
	s.reportHeld()
}

func (s *Swapper) reportHeld() {
	if s.cfg.Metrics == nil {
		return
	}
	s.mu.Lock()
	n := len(s.held)
	s.mu.Unlock()
	s.cfg.Metrics.SetSuspended(n)
}

// Resume points each volume's mapping at its namespace under `nqn` and releases
// its I/O. A namespace that has not appeared yet gets one rescan of the
// subsystem and is waited for until the volume's deadline. A volume whose
// namespace never appears stays suspended and the call fails, so the caller can
// put the namespace back and resume on the source.
func (s *Swapper) Resume(ctx context.Context, handles []string, nqn string) ([]VolumeState, error) {
	vols, err := s.loadAll(handles)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if !v.swappable() {
			return nil, fmt.Errorf("%s: %w", v.handle, ErrNotSwappable)
		}
	}
	for _, v := range vols {
		if err := s.resumeOne(ctx, v, nqn); err != nil {
			return nil, err
		}
	}
	return s.Describe(ctx, handles)
}

// resumeOne resumes one volume under the node's lock on it, so the reload and
// the record rewrite never interleave with a stage, heal, or grow of the same
// volume.
func (s *Swapper) resumeOne(ctx context.Context, v volume, nqn string) error {
	unlock := s.lock(v.handle)
	defer unlock()
	dev, err := s.await(ctx, v, nqn)
	if err != nil {
		return err
	}
	if err := s.repoint(ctx, v, dev); err != nil {
		return err
	}
	s.release(v.handle)
	return nil
}

// await finds v's namespace under `nqn`, rescanning once and polling until the
// volume's deadline.
func (s *Swapper) await(ctx context.Context, v volume, nqn string) (nvme.Device, error) {
	until := s.cfg.Now().Add(defaultResumeWait)
	s.mu.Lock()
	if h, ok := s.held[v.handle]; ok {
		until = h.deadline
	}
	s.mu.Unlock()

	sel := nvme.DeviceSelector{UUID: v.uuid, NQN: nqn}
	started := s.cfg.Now()
	rescanned := false
	var dropErr error
	for {
		found, err := s.cfg.Devices.ListWithSelector(ctx, sel)
		if err != nil {
			return nvme.Device{}, fmt.Errorf("swap: look up %s under %s: %w", v.uuid, nqn, err)
		}
		if len(found) > 0 {
			if rescanned && s.cfg.Metrics != nil {
				s.cfg.Metrics.ObserveRescan(s.cfg.Now().Sub(started))
			}
			return best(found), nil
		}
		if !rescanned {
			rescanned = true
			// The kernel admits a namespace only while no other subsystem on the
			// host holds its identity. A controller of the source that is not
			// live never processed the namespace's removal and still holds it,
			// so the target's copy would be ignored however often it is
			// rescanned. Such a controller carries no I/O, and is dropped first.
			dropErr = s.dropStaleSource(ctx, v, nqn)
			if err := s.cfg.Rescan(ctx, nqn); err != nil {
				return nvme.Device{}, fmt.Errorf("swap: rescan %s: %w", nqn, err)
			}
			continue
		}
		if !s.cfg.Now().Before(until) || ctx.Err() != nil {
			if dropErr != nil {
				return nvme.Device{}, fmt.Errorf("%s under %s: %w (dropping the source's dead paths failed: %v)",
					v.uuid, nqn, ErrNamespaceNotFound, dropErr)
			}
			return nvme.Device{}, fmt.Errorf("%s under %s: %w", v.uuid, nqn, ErrNamespaceNotFound)
		}
		s.cfg.Sleep(s.cfg.PollInterval)
	}
}

// dropStaleSource drops the controllers of v's source subsystem that are not
// live, when v is being resumed somewhere else. A resume onto the source itself
// is a rollback, and needs every path the source still has.
func (s *Swapper) dropStaleSource(ctx context.Context, v volume, nqn string) error {
	source := v.fabric.NQN
	if s.cfg.DropStalePaths == nil || source == "" || source == nqn {
		return nil
	}
	if _, err := s.cfg.DropStalePaths(ctx, source); err != nil {
		return fmt.Errorf("drop the dead paths of %s: %w", source, err)
	}
	return nil
}

// best is the most serviceable of several devices carrying one namespace.
func best(found []nvme.Device) nvme.Device {
	b := found[0]
	for _, d := range found[1:] {
		if d.Accessible() && !b.Accessible() {
			b = d
		}
	}
	return b
}

// repoint makes v's mapping point at dev and run, and records the subsystem
// in the stack record so a later restage or heal connects where the namespace
// is.
func (s *Swapper) repoint(ctx context.Context, v volume, dev nvme.Device) error {
	table, err := s.cfg.Mapper.Table(ctx, v.mapping)
	if err != nil {
		return fmt.Errorf("swap: %s: %w", v.handle, err)
	}
	suspended, err := s.cfg.Mapper.Suspended(ctx, v.mapping)
	if err != nil {
		return fmt.Errorf("swap: %s: %w", v.handle, err)
	}
	switch {
	case table.Device == dev.Namespace.Dev && !suspended:
	case table.Device == dev.Namespace.Dev:
		if err := s.cfg.Mapper.Resume(ctx, v.mapping); err != nil {
			return fmt.Errorf("swap: %s: %w", v.handle, err)
		}
	default:
		if !suspended {
			if err := s.cfg.Mapper.Suspend(ctx, v.mapping); err != nil {
				return fmt.Errorf("swap: %s: %w", v.handle, err)
			}
		}
		sectors, err := s.cfg.Mapper.Sectors(ctx, dev.Namespace.DevicePath)
		if err != nil {
			return fmt.Errorf("swap: %s: %w", v.handle, err)
		}
		if err := s.cfg.Mapper.ReloadResume(ctx, v.mapping, dev.Namespace.Dev, sectors); err != nil {
			return fmt.Errorf("swap: %s: %w", v.handle, err)
		}
	}
	return s.record(v, dev)
}

func (s *Swapper) record(v volume, dev nvme.Device) error {
	nsid := uint32(dev.Namespace.ID)
	if v.fabricI < 0 {
		return nil
	}
	if v.fabric.NQN == dev.Subsystem.NQN && v.fabric.NSID == nsid {
		return nil
	}
	params := v.fabric
	params.NQN = dev.Subsystem.NQN
	params.NSID = nsid
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	rec := v.record
	rec.Plan = append([]volstack.Entry(nil), rec.Plan...)
	rec.Plan[v.fabricI].Params = raw
	if err := s.cfg.Records.Write(rec); err != nil {
		return fmt.Errorf("swap: record %s under %s: %w", v.handle, params.NQN, err)
	}
	return nil
}

// Release disconnects `nqn` unless a volume staged here still points at it.
func (s *Swapper) Release(ctx context.Context, nqn string) (bool, error) {
	records, err := s.cfg.Records.List()
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		v, err := s.load(rec.VolumeHandle)
		if err != nil {
			return false, err
		}
		if v.fabric.NQN == nqn {
			return false, nil
		}
	}
	if err := s.cfg.Connector.Disconnect(ctx, nqn); err != nil {
		return false, fmt.Errorf("swap: disconnect %s: %w", nqn, err)
	}
	return true, nil
}
