// Recorded doubles for the swap tests: a device-mapper that keeps each
// mapping's table and suspension, an NVMe view that a rescan can change, a
// connector that remembers what it attached, and a clock whose timers fire
// when a test says so.

package swaprpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/atlas/devmapper"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

const (
	sourceNQN = "nqn.2023-02.io.simplyblock:c:lvol:source"
	targetNQN = "nqn.2023-02.io.simplyblock:c:lvol:target"
	otherNQN  = "nqn.2023-02.io.simplyblock:c:lvol:other"
)

// mapping is one dm-linear device as the fake kernel holds it.
type mapping struct {
	device    string
	suspended bool
}

type fakeMapper struct {
	mu      sync.Mutex
	maps    map[string]*mapping
	calls   []string
	failOn  map[string]error
	sectors uint64
}

func newFakeMapper() *fakeMapper {
	return &fakeMapper{maps: map[string]*mapping{}, failOn: map[string]error{}, sectors: 2048}
}

func (m *fakeMapper) record(call string) error {
	m.calls = append(m.calls, call)
	for prefix, err := range m.failOn {
		if strings.HasPrefix(call, prefix) {
			return err
		}
	}
	return nil
}

// mutations are the calls that change a mapping, which is what a no-op has to
// be free of.
func (m *fakeMapper) mutations() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		if strings.HasPrefix(c, "suspend") || strings.HasPrefix(c, "reload") || strings.HasPrefix(c, "resume") {
			out = append(out, c)
		}
	}
	return out
}

func (m *fakeMapper) Table(_ context.Context, name string) (devmapper.Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.maps[name]
	if !ok {
		return devmapper.Target{}, devmapper.ErrNotFound
	}
	return devmapper.Target{Sectors: m.sectors, Device: mp.device}, nil
}

func (m *fakeMapper) Sectors(context.Context, string) (uint64, error) { return m.sectors, nil }

func (m *fakeMapper) Suspend(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("suspend " + name); err != nil {
		return err
	}
	m.maps[name].suspended = true
	return nil
}

func (m *fakeMapper) ReloadResume(_ context.Context, name, device string, _ uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("reload " + name + " " + device); err != nil {
		return err
	}
	m.maps[name].device = device
	m.calls = append(m.calls, "resume "+name)
	m.maps[name].suspended = false
	return nil
}

func (m *fakeMapper) Resume(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("resume " + name); err != nil {
		return err
	}
	m.maps[name].suspended = false
	return nil
}

func (m *fakeMapper) Suspended(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.maps[name]
	if !ok {
		return false, devmapper.ErrNotFound
	}
	return mp.suspended, nil
}

func (m *fakeMapper) List(_ context.Context, prefix string) ([]devmapper.Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []devmapper.Mapping
	for name, mp := range m.maps {
		if strings.HasPrefix(name, prefix) {
			out = append(out, devmapper.Mapping{Name: name, Suspended: mp.suspended})
		}
	}
	return out, nil
}

func (m *fakeMapper) isSuspended(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maps[name].suspended
}

func (m *fakeMapper) deviceOf(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maps[name].device
}

// fakeDevices is the host's NVMe view. onRescan lets a test make a namespace
// appear only once the node asked the controller to look again.
type fakeDevices struct {
	mu       sync.Mutex
	devices  []nvme.Device
	rescans  []string
	onRescan func(nqn string)
}

func (d *fakeDevices) add(nqn, uuid, dev string, nsid uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.devices = append(d.devices, nvme.Device{
		Subsystem: nvme.Subsystem{NQN: nqn},
		Namespace: nvme.Namespace{ID: nvme.NamespaceID(nsid), UUID: uuid, Dev: dev, DevicePath: "/dev/" + dev},
	})
}

func (d *fakeDevices) remove(nqn string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := d.devices[:0]
	for _, dev := range d.devices {
		if dev.Subsystem.NQN != nqn {
			kept = append(kept, dev)
		}
	}
	d.devices = kept
}

func (d *fakeDevices) rescan(_ context.Context, nqn string) error {
	d.mu.Lock()
	d.rescans = append(d.rescans, nqn)
	hook := d.onRescan
	d.mu.Unlock()
	if hook != nil {
		hook(nqn)
	}
	return nil
}

func (d *fakeDevices) List(context.Context) ([]nvme.Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]nvme.Device(nil), d.devices...), nil
}

func (d *fakeDevices) ListWithSelector(_ context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []nvme.Device
	for _, dev := range d.devices {
		if sel.Matches(dev) {
			out = append(out, dev)
		}
	}
	return out, nil
}

func (d *fakeDevices) ByUUID(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, fmt.Errorf("unused")
}

func (d *fakeDevices) ByDevicePath(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, fmt.Errorf("unused")
}

func (d *fakeDevices) ByNamespace(context.Context, string, nvme.NamespaceID) (nvme.Device, error) {
	return nvme.Device{}, fmt.Errorf("unused")
}

type fakeConnector struct {
	connected    [][]nvmeof.Target
	disconnected []string
	err          error
}

func (c *fakeConnector) Connect(context.Context, nvmeof.Target) error { return nil }

func (c *fakeConnector) ConnectPaths(_ context.Context, targets []nvmeof.Target) ([]nvmeof.PathResult, error) {
	c.connected = append(c.connected, targets)
	if c.err != nil {
		return nil, c.err
	}
	results := make([]nvmeof.PathResult, len(targets))
	for i, t := range targets {
		results[i] = nvmeof.PathResult{Target: t, Live: true}
	}
	return results, nil
}

func (c *fakeConnector) Disconnect(_ context.Context, nqn string) error {
	c.disconnected = append(c.disconnected, nqn)
	return nil
}

func (c *fakeConnector) DisconnectController(context.Context, nvme.Controller) error { return nil }

func (c *fakeConnector) IsConnected(context.Context, string) (bool, error) { return true, nil }

// fakeClock hands out timers that fire only when the test advances past them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}

// advance moves the clock and runs every timer now due, in order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.fn()
	}
}

func (c *fakeClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

type warning struct{ handle, reason, message string }

type fakeEvents struct {
	mu       sync.Mutex
	warnings []warning
}

func (e *fakeEvents) Warning(handle, reason, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.warnings = append(e.warnings, warning{handle, reason, message})
}

type fakeMetrics struct {
	mu        sync.Mutex
	suspended int
	rescans   int
	watchdog  map[string]int
}

func (m *fakeMetrics) SetSuspended(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suspended = n
}

func (m *fakeMetrics) ObserveRescan(time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rescans++
}

func (m *fakeMetrics) Watchdog(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watchdog == nil {
		m.watchdog = map[string]int{}
	}
	m.watchdog[outcome]++
}

// rig is one node with volumes staged on it.
type rig struct {
	t         *testing.T
	store     *volstack.Store
	mapper    *fakeMapper
	devices   *fakeDevices
	connector *fakeConnector
	clock     *fakeClock
	events    *fakeEvents
	metrics   *fakeMetrics
	swapper   *Swapper
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		t:         t,
		store:     volstack.NewStore(t.TempDir()),
		mapper:    newFakeMapper(),
		devices:   &fakeDevices{},
		connector: &fakeConnector{},
		clock:     &fakeClock{now: time.Unix(1_800_000_000, 0)},
		events:    &fakeEvents{},
		metrics:   &fakeMetrics{},
	}
	s, err := New(Config{
		Records:      r.store,
		Mapper:       r.mapper,
		Devices:      r.devices,
		Connector:    r.connector,
		Rescan:       r.devices.rescan,
		Events:       r.events,
		Metrics:      r.metrics,
		HostNQN:      "nqn.2014-08.org.nvmexpress:uuid:host",
		HostID:       "host",
		Now:          r.clock.Now,
		AfterFunc:    r.clock.AfterFunc,
		Sleep:        func(time.Duration) { r.clock.advance(100 * time.Millisecond) },
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.swapper = s
	return r
}

// handle is the CSI volume handle of the volume with UUID uuid.
func handle(uuid string) string { return "cluster:pool:" + uuid }

// stage records a volume as the node service would have staged it, with or
// without the indirection, on the source subsystem at device dev.
func (r *rig) stage(uuid, dev string, indirect bool) {
	r.t.Helper()
	fabric, _ := json.Marshal(layers.FabricParams{NQN: sourceNQN, NSID: 1, HostNQN: "nqn.host", SecretRef: "ns/secret"})
	plan := []volstack.Entry{{Layer: "fabric", Params: fabric, Attempted: true}}
	if indirect {
		dm, _ := json.Marshal(layers.DMLinearParams{Name: layers.DMLinearName(uuid)})
		plan = append(plan, volstack.Entry{Layer: "dmLinear", Params: dm, Attempted: true})
		r.mapper.maps[layers.DMLinearName(uuid)] = &mapping{device: dev}
	}
	plan = append(plan, volstack.Entry{Layer: "filesystem", Attempted: true})
	if err := r.store.Write(volstack.Record{Version: volstack.RecordVersion, VolumeHandle: handle(uuid), Plan: plan}); err != nil {
		r.t.Fatal(err)
	}
	r.devices.add(sourceNQN, uuid, dev, 1)
}

// fabricOf reads back what the record says the fabric layer is.
func (r *rig) fabricOf(uuid string) layers.FabricParams {
	r.t.Helper()
	rec, err := r.store.Load(handle(uuid))
	if err != nil {
		r.t.Fatal(err)
	}
	var p layers.FabricParams
	if err := json.Unmarshal(rec.Plan[0].Params, &p); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *rig) deadline(d time.Duration) time.Time { return r.clock.Now().Add(d) }
