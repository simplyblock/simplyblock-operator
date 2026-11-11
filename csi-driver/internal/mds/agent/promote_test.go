// Tests for the guest's role machine (U-15 to U-22, U-31, and U-32 of
// test-plan-pnfs-mds-standby.md). Everything the sequence touches outside the
// process is a fake that appends to one event list, so a test asserts the
// order of the steps and that a failure stops the ones after it. The
// reservation commands run against nvmeprtest.

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/nvmepr"
	"github.com/simplyblock/atlas/nvmepr/nvmeprtest"
)

const (
	prefixStart    = "start"
	prefixWriteKey = "write-key"
	forgetState    = "forget state-volume"
	unmountDir     = "unmount-state /var/lib/nfs"
)

const podHostNQN = "nqn.2014-08.io.simplyblock:uuid:11111111-2222-3333-4444-555555555555"

// events is the order in which the fakes were called.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, fmt.Sprintf(format, args...))
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

// only is the events with the given prefix.
func (e *events) only(prefix string) []string {
	var out []string
	for _, s := range e.all() {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			out = append(out, s)
		}
	}
	return out
}

type fakeConnector struct {
	ev   *events
	fail map[string]error // by subsystem NQN
	// flaky fails a subsystem's next n connects, then lets it through.
	flaky map[string]int
}

func (f *fakeConnector) Connect(_ context.Context, conn lvol.Connection, hostNQN string) (blockdev.Device, error) {
	f.ev.add("connect %s as %s", conn.NQN, hostNQN)
	if err := f.fail[conn.NQN]; err != nil {
		return blockdev.Device{}, err
	}
	if f.flaky[conn.NQN] > 0 {
		f.flaky[conn.NQN]--
		return blockdev.Device{}, errors.New("could not add new controller: failed to write to nvme-fabrics device")
	}
	return blockdev.Device{Path: "/dev/" + conn.NQN}, nil
}

type fakeState struct {
	ev            *events
	mountErr      error
	unmountCalled bool
}

func (f *fakeState) Mount(_ context.Context, dev blockdev.Device, dir string) error {
	f.ev.add("mount-state %s at %s", dev.Path, dir)
	return f.mountErr
}

func (f *fakeState) Unmount(_ context.Context, dir string) error {
	f.ev.add("unmount-state %s", dir)
	f.unmountCalled = true
	return nil
}

type fakeKey struct {
	ev  *events
	err error
}

func (f *fakeKey) Write(_ context.Context, key uint64) error {
	f.ev.add("write-key %d", key)
	return f.err
}

type fakeSystemd struct {
	ev      *events
	fail    map[string]error
	started chan struct{} // signaled once a Start of nfs-server is reached
	release chan struct{} // when set, that Start waits for it
}

func (f *fakeSystemd) Start(_ context.Context, unit string) error {
	f.ev.add("start %s", unit)
	if unit == "nfs-server.service" && f.release != nil {
		f.started <- struct{}{}
		<-f.release
	}
	return f.fail[unit]
}

func (f *fakeSystemd) CreateTmpfiles(_ context.Context, conf string) error {
	f.ev.add("tmpfiles %s", conf)
	return f.fail["tmpfiles"]
}

func (f *fakeSystemd) AllowNFSD(context.Context) error {
	f.ev.add("allow-nfsd")
	return f.fail["allow"]
}

func (f *fakeSystemd) RevokeNFSD(context.Context) error {
	f.ev.add("revoke-nfsd")
	return nil
}

func (f *fakeSystemd) Stop(_ context.Context, unit string) error {
	f.ev.add("stop %s", unit)
	return nil
}

type fakeTracker struct{ ev *events }

func (f fakeTracker) Track(name string, conn lvol.Connection, hostNQN string) {
	f.ev.add("track %s %s as %s", name, conn.NQN, hostNQN)
}
func (f fakeTracker) Forget(name string) { f.ev.add("forget %s", name) }

// innerAssembler records what reaches the real assembler.
type innerAssembler struct{ ev *events }

func (a innerAssembler) Create(_ context.Context, s export.Spec) error {
	a.ev.add("create %s", s.Path)
	return nil
}
func (a innerAssembler) Delete(_ context.Context, s export.Spec) error {
	a.ev.add("delete %s", s.Path)
	return nil
}
func (a innerAssembler) Check(_ context.Context, s export.Spec) error {
	a.ev.add("check %s", s.Path)
	return nil
}

type rig struct {
	t     *testing.T
	ev    *events
	conn  *fakeConnector
	state *fakeState
	key   *fakeKey
	sd    *fakeSystemd
	cfg   GuestConfig
	g     *Guest

	mu sync.Mutex
	ns map[string]*nvmeprtest.Namespace
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ev := &events{}
	r := &rig{
		t: t, ev: ev, ns: map[string]*nvmeprtest.Namespace{},
		conn:  &fakeConnector{ev: ev, fail: map[string]error{}},
		state: &fakeState{ev: ev},
		key:   &fakeKey{ev: ev},
		sd:    &fakeSystemd{ev: ev, fail: map[string]error{}},
	}
	r.cfg = GuestConfig{
		Connector: r.conn,
		Open: func(path string) (nvmepr.Device, error) {
			ev.add("fence %s", path)
			return r.namespace(path).Host(hostID(1)), nil
		},
		HostID:   func(string) ([16]byte, error) { return hostID(1), nil },
		State:    r.state,
		Key:      r.key,
		Units:    r.sd,
		Tracker:  fakeTracker{ev},
		StateDir: stateDir,
	}
	r.g = NewGuest(r.cfg)
	return r
}

// rebuild makes the guest again from the rig's config, after a test changed it.
func (r *rig) rebuild() { r.g = NewGuest(r.cfg) }

// namespace is the namespace behind a device path, created on first use.
func (r *rig) namespace(path string) *nvmeprtest.Namespace {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ns[path] == nil {
		r.ns[path] = nvmeprtest.New()
	}
	return r.ns[path]
}

// withOldActive makes an earlier guest the holder of every device in paths.
func (r *rig) withOldActive(paths ...string) {
	for _, p := range paths {
		become(r.t, r.namespace(p), hostID(2), mdsKey(r.t, fenceUID, 1), true)
	}
}

func (r *rig) request(epoch uint64, exports ...string) nfsexportrpc.PromoteRequest {
	req := nfsexportrpc.PromoteRequest{
		Epoch:          epoch,
		Key:            mdsKey(r.t, fenceUID, epoch),
		HostNQN:        podHostNQN,
		StatefulSetUID: fenceUID,
		StateVolume:    nfsexportrpc.VolumeSpec{VolumeUUID: "state", Connection: &lvol.Connection{NQN: "nqn:state"}},
	}
	for _, n := range exports {
		req.Exports = append(req.Exports, export.Spec{
			VolumeUUID: n, Path: "/exports/" + n, Connection: &lvol.Connection{NQN: "nqn:" + n},
		})
	}
	return req
}

func (r *rig) promote(req nfsexportrpc.PromoteRequest) nfsexportrpc.PromoteResult {
	r.t.Helper()
	res, err := r.g.Promote(context.Background(), req)
	if err != nil {
		r.t.Fatalf("Promote(epoch %d): %v", req.Epoch, err)
	}
	return res
}

func (r *rig) describe() nfsexportrpc.Description {
	r.t.Helper()
	d, err := r.g.Describe(context.Background())
	if err != nil {
		r.t.Fatalf("Describe: %v", err)
	}
	return d
}

func TestGuestBootsAsAStandby(t *testing.T) {
	r := newRig(t)
	if d := r.describe(); d.Role != nfsexportrpc.RoleStandby || d.Epoch != 0 || d.Key != 0 {
		t.Errorf("Describe = %+v, want a standby with no epoch and no key", d)
	}
	if got := r.ev.all(); len(got) != 0 {
		t.Errorf("a booted guest did %v, want nothing", got)
	}
}

func TestPromoteU15RunsConnectFenceMountKeyAndStartInOrder(t *testing.T) {
	r := newRig(t)
	req := r.request(3, "a", "b")

	res := r.promote(req)
	if res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("Promote = %+v, want Active", res)
	}
	want := []string{
		"connect nqn:state as " + podHostNQN,
		"connect nqn:a as " + podHostNQN,
		"connect nqn:b as " + podHostNQN,
		"fence /dev/nqn:state",
		"fence /dev/nqn:a",
		"fence /dev/nqn:b",
		"track state-volume nqn:state as " + podHostNQN,
		"track /exports/a nqn:a as " + podHostNQN,
		"track /exports/b nqn:b as " + podHostNQN,
		"mount-state /dev/nqn:state at /var/lib/nfs",
		"tmpfiles /usr/lib/tmpfiles.d/pnfs.conf",
		fmt.Sprintf("write-key %d", req.Key),
		"allow-nfsd",
		"start nfsdcld.service",
		"start nfs-server.service",
	}
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Errorf("events:\n got  %q\n want %q", got, want)
	}
	if d := r.describe(); d.Role != nfsexportrpc.RoleActive || d.Epoch != 3 || d.Key != req.Key {
		t.Errorf("Describe = %+v, want Active at epoch 3 with the key", d)
	}
	for _, p := range []string{"/dev/nqn:state", "/dev/nqn:a", "/dev/nqn:b"} {
		if k, ok := holderKey(t, r.namespace(p)); !ok || k != req.Key {
			t.Errorf("%s: holder = %x (held %v), want %x", p, k, ok, req.Key)
		}
	}
}

func TestPromoteWithNoExportsFencesAndMountsTheStateVolumeAlone(t *testing.T) {
	r := newRig(t)
	if res := r.promote(r.request(1)); res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("Promote = %+v, want Active", res)
	}
	if got := r.ev.only("fence"); !slices.Equal(got, []string{"fence /dev/nqn:state"}) {
		t.Errorf("fenced %v, want the state volume alone", got)
	}
}

func TestPromoteU16AFenceFailingOnTheLastOfFiveDevicesStopsEverythingAfterIt(t *testing.T) {
	r := newRig(t)
	paths := []string{"/dev/nqn:state", "/dev/nqn:a", "/dev/nqn:b", "/dev/nqn:c", "/dev/nqn:d"}
	r.withOldActive(paths...)
	r.namespace("/dev/nqn:d").FailNext(opPre, errors.New("target timeout"))
	req := r.request(2, "a", "b", "c", "d")

	res := r.promote(req)
	if res.Outcome != nfsexportrpc.PromoteFenceFailed || res.Device != "/dev/nqn:d" || res.Step != opPre {
		t.Fatalf("Promote = %+v, want FenceFailed on /dev/nqn:d at preempt", res)
	}
	for _, prefix := range []string{"track", "mount-state", prefixWriteKey, prefixStart, "unmount-state"} {
		if got := r.ev.only(prefix); len(got) != 0 {
			t.Errorf("%s happened after the failed fence: %v", prefix, got)
		}
	}
	if d := r.describe(); d.Role != nfsexportrpc.RoleStandby || d.Epoch != 0 {
		t.Errorf("Describe = %+v, want a standby again", d)
	}
	// the devices fenced before the failure stay fenced
	for _, p := range paths[:4] {
		if k, _ := holderKey(t, r.namespace(p)); k != req.Key {
			t.Errorf("%s: holder = %x, want the new key to stay", p, k)
		}
	}
}

func TestPromoteRetryAtTheSameEpochAfterAFailedFenceBecomesActive(t *testing.T) {
	r := newRig(t)
	r.withOldActive("/dev/nqn:state", "/dev/nqn:a")
	r.namespace("/dev/nqn:a").FailNext(opPre, errors.New("target timeout"))
	req := r.request(2, "a")

	if res := r.promote(req); res.Outcome != nfsexportrpc.PromoteFenceFailed {
		t.Fatalf("first Promote = %+v, want FenceFailed", res)
	}
	if res := r.promote(req); res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("retry = %+v, want Active", res)
	}
	if d := r.describe(); d.Role != nfsexportrpc.RoleActive || d.Epoch != 2 {
		t.Errorf("Describe = %+v, want Active at epoch 2", d)
	}
}

func TestPromoteAKeyThatIsNotTheEpochsIsRefusedBeforeAnythingHappens(t *testing.T) {
	r := newRig(t)
	req := r.request(2)
	req.Key = mdsKey(t, fenceUID, 3)

	if _, err := r.g.Promote(context.Background(), req); err == nil {
		t.Fatal("Promote accepted a key that is not the epoch's")
	}
	req = r.request(2)
	req.StatefulSetUID = "another"
	if _, err := r.g.Promote(context.Background(), req); err == nil {
		t.Fatal("Promote accepted a key made from another StatefulSet")
	}
	req = r.request(1)
	req.Epoch, req.Key = 0, 0
	if _, err := r.g.Promote(context.Background(), req); err == nil {
		t.Fatal("Promote accepted epoch 0")
	}
	if got := r.ev.all(); len(got) != 0 {
		t.Errorf("a refused request still did %v", got)
	}
	if d := r.describe(); d.Role != nfsexportrpc.RoleStandby {
		t.Errorf("Describe = %+v, want a standby", d)
	}
}

func TestPromoteU17TheSameEpochTwiceRepeatsNothing(t *testing.T) {
	r := newRig(t)
	req := r.request(4, "a")
	r.promote(req)
	before := r.ev.all()

	if res := r.promote(req); res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("second Promote = %+v, want Active", res)
	}
	if got := r.ev.all(); !slices.Equal(got, before) {
		t.Errorf("the second Promote did %v", got[len(before):])
	}
}

func TestPromoteU18AnEpochBelowOrAboveTheAcceptedOneIsRefused(t *testing.T) {
	r := newRig(t)
	r.promote(r.request(5))
	before := r.ev.all()

	for _, epoch := range []uint64{4, 6} {
		if res := r.promote(r.request(epoch)); res.Outcome != nfsexportrpc.PromoteStaleEpoch {
			t.Errorf("Promote at %d = %+v, want StaleEpoch", epoch, res)
		}
	}
	if got := r.ev.all(); !slices.Equal(got, before) {
		t.Errorf("a refused Promote did %v", got[len(before):])
	}
	if d := r.describe(); d.Role != nfsexportrpc.RoleActive || d.Epoch != 5 {
		t.Errorf("Describe = %+v, want Active at epoch 5", d)
	}
}

func TestPromoteAnEpochBelowOneAlreadyTriedThisBootIsRefused(t *testing.T) {
	r := newRig(t)
	r.withOldActive("/dev/nqn:state")
	r.namespace("/dev/nqn:state").FailNext(opPre, errors.New("target timeout"))
	if res := r.promote(r.request(5)); res.Outcome != nfsexportrpc.PromoteFenceFailed {
		t.Fatalf("Promote at 5 = %+v, want FenceFailed", res)
	}
	if res := r.promote(r.request(4)); res.Outcome != nfsexportrpc.PromoteStaleEpoch {
		t.Errorf("Promote at 4 = %+v, want StaleEpoch", res)
	}
}

func TestPromoteASupersededEpochLeavesTheNewerKeyAlone(t *testing.T) {
	r := newRig(t)
	// a newer active, another host, already holds the state volume at epoch 7
	become(t, r.namespace("/dev/nqn:state"), hostID(2), mdsKey(t, fenceUID, 7), true)

	res := r.promote(r.request(6))
	if res.Outcome != nfsexportrpc.PromoteSuperseded {
		t.Fatalf("Promote = %+v, want Superseded", res)
	}
	if k, _ := holderKey(t, r.namespace("/dev/nqn:state")); k != mdsKey(t, fenceUID, 7) {
		t.Errorf("holder = %x, want the newer key untouched", k)
	}
	if got := r.ev.only("mount-state"); len(got) != 0 {
		t.Errorf("mounted after being superseded: %v", got)
	}
}

func TestPromoteACollisionIsReportedAsOne(t *testing.T) {
	r := newRig(t)
	become(t, r.namespace("/dev/nqn:state"), hostID(2), mdsKey(t, fenceUID, 6), false)
	if res := r.promote(r.request(6)); res.Outcome != nfsexportrpc.PromoteCollision {
		t.Fatalf("Promote = %+v, want Collision", res)
	}
}

func TestPromoteAConnectFailureStopsBeforeAnyFence(t *testing.T) {
	r := newRig(t)
	r.conn.fail["nqn:b"] = errors.New("no path came up")

	res := r.promote(r.request(1, "a", "b"))
	if res.Outcome != nfsexportrpc.PromoteFenceFailed || res.Step != "connect" {
		t.Fatalf("Promote = %+v, want FenceFailed at connect", res)
	}
	if got := r.ev.only("fence"); len(got) != 0 {
		t.Errorf("fenced %v although a device did not connect", got)
	}
	if d := r.describe(); d.Role != nfsexportrpc.RoleStandby {
		t.Errorf("Describe = %+v, want a standby", d)
	}
}

func TestPromoteAMissingConnectionIsAnErrorAndDoesNothing(t *testing.T) {
	r := newRig(t)
	req := r.request(1, "a")
	req.Exports[0].Connection = nil
	if _, err := r.g.Promote(context.Background(), req); err == nil {
		t.Fatal("Promote passed with an export that carries no connection")
	}
	if got := r.ev.all(); len(got) != 0 {
		t.Errorf("did %v before refusing", got)
	}
}

func TestPromoteAFailureAfterTheFenceStopsTheLaterStepsAndLeavesAStandby(t *testing.T) {
	type tc struct {
		name   string
		break_ func(*rig)
		absent []string // event prefixes that must not appear
		want   []string // events that must appear after the failure
	}
	for _, c := range []tc{
		{"mount", func(r *rig) { r.state.mountErr = errors.New("mount: wrong fs type") },
			[]string{prefixWriteKey, prefixStart, "unmount-state"}, []string{forgetState, "forget /exports/a"}},
		{"tmpfiles", func(r *rig) { r.sd.fail["tmpfiles"] = errors.New("tmpfiles failed") },
			[]string{prefixWriteKey, prefixStart}, []string{unmountDir, forgetState}},
		{"key", func(r *rig) { r.key.err = errors.New("write: device or resource busy") },
			[]string{prefixStart}, []string{unmountDir, forgetState}},
		{"nfsdcld", func(r *rig) { r.sd.fail["nfsdcld.service"] = errors.New("unit failed") },
			[]string{"start nfs-server"}, []string{"revoke-nfsd", unmountDir, forgetState}},
		{"nfs-server", func(r *rig) { r.sd.fail["nfs-server.service"] = errors.New("unit failed") },
			nil, []string{"stop nfs-server.service", "revoke-nfsd", unmountDir, forgetState}},
		{"allow", func(r *rig) { r.sd.fail["allow"] = errors.New("cannot write the gate") },
			[]string{prefixStart}, []string{unmountDir, forgetState}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			c.break_(r)
			if _, err := r.g.Promote(context.Background(), r.request(2, "a")); err == nil {
				t.Fatal("Promote passed with a failing step")
			}
			for _, p := range c.absent {
				if got := r.ev.only(p); len(got) != 0 {
					t.Errorf("%s happened after the failure: %v", p, got)
				}
			}
			all := r.ev.all()
			for _, w := range c.want {
				if !slices.Contains(all, w) {
					t.Errorf("missing %q in %q", w, all)
				}
			}
			if d := r.describe(); d.Role != nfsexportrpc.RoleStandby {
				t.Errorf("Describe = %+v, want a standby", d)
			}
		})
	}
}

func TestPromoteRetryAfterAFailedMountIsAccepted(t *testing.T) {
	r := newRig(t)
	r.state.mountErr = errors.New("mount: wrong fs type")
	req := r.request(2)
	if _, err := r.g.Promote(context.Background(), req); err == nil {
		t.Fatal("Promote passed with a failing mount")
	}
	r.state.mountErr = nil
	if res := r.promote(req); res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("retry = %+v, want Active", res)
	}
}

func TestDescribeShowsPromotingWhileThePromotionRuns(t *testing.T) {
	r := newRig(t)
	r.sd.started, r.sd.release = make(chan struct{}), make(chan struct{})
	done := make(chan nfsexportrpc.PromoteResult)
	go func() { done <- r.promote(r.request(2)) }()

	<-r.sd.started
	if d := r.describe(); d.Role != nfsexportrpc.RolePromoting || d.Epoch != 2 {
		t.Errorf("Describe = %+v, want Promoting at epoch 2", d)
	}
	close(r.sd.release)
	if res := <-done; res.Outcome != nfsexportrpc.PromoteActive {
		t.Errorf("Promote = %+v, want Active", res)
	}
}

func TestPromoteTwoCallsAtOneEpochRunTheSideEffectsOnce(t *testing.T) {
	r := newRig(t)
	req := r.request(2, "a")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if res := r.promote(req); res.Outcome != nfsexportrpc.PromoteActive {
				t.Errorf("Promote = %+v, want Active", res)
			}
		})
	}
	wg.Wait()
	if got := r.ev.only("start nfs-server"); len(got) != 1 {
		t.Errorf("nfs-server started %d times, want once", len(got))
	}
}

func guarded(r *rig) Assembler { return r.g.Guard(innerAssembler{r.ev}) }

func newSpec(name string) export.Spec {
	return export.Spec{
		VolumeUUID: name, Path: "/exports/" + name, Connection: &lvol.Connection{NQN: "nqn:" + name},
	}
}

func TestGuardU19RefusesCreateOnAStandbyWithoutConnectingOrMounting(t *testing.T) {
	r := newRig(t)
	err := guarded(r).Create(context.Background(), newSpec("a"))
	if !errors.Is(err, ErrNotActive) {
		t.Fatalf("Create = %v, want ErrNotActive", err)
	}
	if got := r.ev.all(); len(got) != 0 {
		t.Errorf("a refused Create did %v", got)
	}
}

func TestGuardLetsCheckAndDeleteThroughOnAStandby(t *testing.T) {
	r := newRig(t)
	a := guarded(r)
	if err := a.Check(context.Background(), newSpec("a")); err != nil {
		t.Errorf("Check: %v", err)
	}
	if err := a.Delete(context.Background(), newSpec("a")); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if got := r.ev.all(); !slices.Equal(got, []string{"check /exports/a", "delete /exports/a"}) {
		t.Errorf("events = %q, want the check and the delete", got)
	}
}

func TestGuardRefusesCreateWhilePromoting(t *testing.T) {
	r := newRig(t)
	r.sd.started, r.sd.release = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { r.promote(r.request(2)); close(done) }()
	<-r.sd.started

	if err := guarded(r).Create(context.Background(), newSpec("a")); !errors.Is(err, ErrNotActive) {
		t.Errorf("Create = %v, want ErrNotActive", err)
	}
	close(r.sd.release)
	<-done
}

func TestGuardU20FencesANewExportBeforeTheAssemblerMakesItsFilesystem(t *testing.T) {
	r := newRig(t)
	r.promote(r.request(2))
	r.withOldActive("/dev/nqn:new")
	mark := len(r.ev.all())

	if err := guarded(r).Create(context.Background(), newSpec("new")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []string{"connect nqn:new as " + podHostNQN, "fence /dev/nqn:new", "create /exports/new"}
	if got := r.ev.all()[mark:]; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if k, _ := holderKey(t, r.namespace("/dev/nqn:new")); k != mdsKey(t, fenceUID, 2) {
		t.Errorf("holder = %x, want this promotion's key", k)
	}
}

func TestGuardAFailedFenceOfANewExportKeepsTheAssemblerOut(t *testing.T) {
	r := newRig(t)
	r.promote(r.request(2))
	r.withOldActive("/dev/nqn:new")
	r.namespace("/dev/nqn:new").FailNext(opPre, errors.New("target timeout"))

	if err := guarded(r).Create(context.Background(), newSpec("new")); err == nil {
		t.Fatal("Create passed with a failing fence")
	}
	if got := r.ev.only("create"); len(got) != 0 {
		t.Errorf("the assembler ran: %v", got)
	}
}

func TestGuardU31AnExportThePromotionHeldIsCreatedWithoutAReservationCommand(t *testing.T) {
	r := newRig(t)
	r.promote(r.request(2, "a"))
	ns := r.namespace("/dev/nqn:a")
	commands := len(ns.Calls())
	mark := len(r.ev.all())

	if err := guarded(r).Create(context.Background(), newSpec("a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(ns.Calls()); got != commands {
		t.Errorf("%d reservation commands for a held export, want none", got-commands)
	}
	if got := r.ev.all()[mark:]; !slices.Equal(got, []string{"create /exports/a"}) {
		t.Errorf("events = %q, want only the assembler's create", got)
	}
}

func TestGuardAFencedGuestCannotRegisterBackThroughAnExportItHeld(t *testing.T) {
	r := newRig(t)
	r.promote(r.request(2, "a"))
	// a newer active takes the device
	newer := nvmeprtest.New()
	r.ns["/dev/nqn:a"] = newer
	become(t, newer, hostID(5), mdsKey(t, fenceUID, 3), true)

	if err := guarded(r).Create(context.Background(), newSpec("a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := commandsFrom(newer, hostID(1)); len(got) != 0 {
		t.Errorf("the fenced guest sent %v", got)
	}
}

func TestGuardADeletedExportIsFencedAgainWhenItIsCreatedAgain(t *testing.T) {
	r := newRig(t)
	r.promote(r.request(2, "a"))
	a := guarded(r)
	ctx := context.Background()
	if err := a.Delete(ctx, newSpec("a")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// a newer active holds it by now: the old guest is refused, not re-admitted
	newer := nvmeprtest.New()
	r.ns["/dev/nqn:a"] = newer
	become(t, newer, hostID(5), mdsKey(t, fenceUID, 3), true)

	if err := a.Create(ctx, newSpec("a")); err == nil {
		t.Fatal("Create passed for a device a newer active holds")
	}
	if k, _ := holderKey(t, newer); k != mdsKey(t, fenceUID, 3) {
		t.Errorf("holder = %x, want the newer key", k)
	}
}

func TestPromoteU32TheKeepersTrackEverythingThePromotionConnected(t *testing.T) {
	r := newRig(t)
	rec := &reconciles{}
	keeper := newKeeper(rec, &recordingLog{})
	r.g.cfg.Tracker = keeper

	r.promote(r.request(2, "a", "b"))
	keeper.Tick(context.Background())

	got := rec.calls()
	slices.Sort(got)
	want := []string{"nqn:a as " + podHostNQN, "nqn:b as " + podHostNQN, "nqn:state as " + podHostNQN}
	if !slices.Equal(got, want) {
		t.Errorf("reconciled %q, want %q", got, want)
	}
}

func TestPromoteHostIDComesFromTheHostNQN(t *testing.T) {
	id, err := HostIDFromNQN(nqn.Host("11111111-2222-3333-4444-555555555555"))
	if err != nil {
		t.Fatalf("HostIDFromNQN: %v", err)
	}
	want := [16]byte{0x11, 0x11, 0x11, 0x11, 0x22, 0x22, 0x33, 0x33, 0x44, 0x44, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55}
	if id != want {
		t.Errorf("host id = %x, want %x", id, want)
	}
	if _, err := HostIDFromNQN("nqn.2014-08.org.nvmexpress:no-uuid"); err == nil {
		t.Error("a host NQN with no UUID gave a host id")
	}
}

// A connect right after boot sometimes fails and succeeds a moment later, so the
// agent retries it before it gives the promotion up.
func TestPromoteRetriesAConnectThatFailsAtFirst(t *testing.T) {
	r := newRig(t)
	r.conn.flaky = map[string]int{"nqn:a": 2}
	var slept []time.Duration
	r.cfg.ConnectAttempts, r.cfg.ConnectDelay = 3, 2*time.Second
	r.cfg.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	r.rebuild()

	res := r.promote(r.request(1, "a"))

	if res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("Promote = %+v, want Active after the retries", res)
	}
	if got := len(r.ev.only("connect nqn:a")); got != 3 {
		t.Errorf("connect attempts of nqn:a = %d, want 3", got)
	}
	if len(slept) != 2 || slept[0] != 2*time.Second {
		t.Errorf("waits = %v, want two of 2s", slept)
	}
}

func TestPromoteGivesUpAConnectThatNeverSucceedsAfterTheAttempts(t *testing.T) {
	r := newRig(t)
	r.conn.fail["nqn:a"] = errors.New("no path came up")
	var slept []time.Duration
	r.cfg.ConnectAttempts, r.cfg.ConnectDelay = 3, time.Second
	r.cfg.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	r.rebuild()

	res := r.promote(r.request(1, "a"))

	if res.Outcome != nfsexportrpc.PromoteFenceFailed || res.Step != "connect" || res.Device != "nqn:a" {
		t.Fatalf("Promote = %+v, want FenceFailed at connect on nqn:a", res)
	}
	if got := len(r.ev.only("connect nqn:a")); got != 3 {
		t.Errorf("connect attempts = %d, want 3 and no more", got)
	}
	if len(slept) != 2 {
		t.Errorf("waits = %v, want one between each pair of attempts", slept)
	}
	if got := r.ev.only("fence"); len(got) != 0 {
		t.Errorf("fenced %v although a device never connected", got)
	}
}

func TestPromoteStopsRetryingWhenItsContextEnds(t *testing.T) {
	r := newRig(t)
	r.conn.fail["nqn:a"] = errors.New("no path came up")
	r.cfg.ConnectAttempts, r.cfg.ConnectDelay = 5, time.Second
	r.cfg.Sleep = func(ctx context.Context, _ time.Duration) error { return context.Canceled }
	r.rebuild()

	res := r.promote(r.request(1, "a"))

	if res.Outcome != nfsexportrpc.PromoteFenceFailed {
		t.Fatalf("Promote = %+v, want FenceFailed", res)
	}
	if got := len(r.ev.only("connect nqn:a")); got != 1 {
		t.Errorf("connect attempts = %d, want 1 once the wait was cut short", got)
	}
}
