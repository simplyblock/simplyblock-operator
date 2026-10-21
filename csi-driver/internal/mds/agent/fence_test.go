// Tests for the guest's fence (U-05 to U-14 and U-26 to U-30 of
// test-plan-pnfs-mds-standby.md). Two or more hosts share one in-memory
// namespace, so a test plays an old active, a newer one, and the clients
// registered beside them.

package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	"github.com/simplyblock/atlas/nvmepr"
	"github.com/simplyblock/atlas/nvmepr/nvmeprtest"
)

const fenceUID = "statefulset-uid-a"

func hostID(n byte) [16]byte {
	var h [16]byte
	h[0], h[15] = 0xaa, n
	return h
}

func mdsKey(t *testing.T, uid string, epoch uint64) uint64 {
	t.Helper()
	k, err := export.MDSReservationKey(uid, epoch)
	if err != nil {
		t.Fatalf("MDSReservationKey(%s, %d): %v", uid, epoch, err)
	}
	return k
}

// A client key as nfsd derives it: the server's boot time over a client id, so
// its top byte is not the metadata server's.
func clientKey(id uint64) uint64 { return 0x6a0b_0000_0000_0000 | id }

// become registers host under key on ns and, when hold, takes the reservation.
func become(t *testing.T, ns *nvmeprtest.Namespace, host [16]byte, key uint64, hold bool) {
	t.Helper()
	dev := ns.Host(host)
	ctx := context.Background()
	if err := dev.Register(ctx, 0, key, false); err != nil {
		t.Fatalf("Register(%x): %v", key, err)
	}
	if hold {
		if err := dev.Acquire(ctx, key, nvmepr.TypeExclusiveAccessRegOnly); err != nil {
			t.Fatalf("Acquire(%x): %v", key, err)
		}
	}
}

func reportOf(t *testing.T, ns *nvmeprtest.Namespace) nvmepr.Report {
	t.Helper()
	r, err := ns.Host(hostID(0xff)).Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	return r
}

func keysOf(r nvmepr.Report) []uint64 {
	var out []uint64
	for _, reg := range r.Registrants {
		out = append(out, reg.Key)
	}
	slices.Sort(out)
	return out
}

func holderKey(t *testing.T, ns *nvmeprtest.Namespace) (uint64, bool) {
	t.Helper()
	h, ok := reportOf(t, ns).Holder()
	return h.Key, ok
}

// opens serves each path from its namespace, as host sees it.
func opens(host [16]byte, byPath map[string]*nvmeprtest.Namespace) Opener {
	return func(path string) (nvmepr.Device, error) {
		ns, ok := byPath[path]
		if !ok {
			return nil, errors.New("no such device " + path)
		}
		return ns.Host(host), nil
	}
}

// commandsFrom is the commands host sent that change the target, in order.
func commandsFrom(ns *nvmeprtest.Namespace, host [16]byte) []string {
	var out []string
	for _, c := range ns.Calls() {
		if c.Host == host && c.Op != "report" {
			out = append(out, c.Op)
		}
	}
	return out
}

// noPlainRegister fails the test when host sent a Register with a zero old
// key after this call, which a target refuses for a host registered under another
// key.
func noPlainRegister(t *testing.T, host [16]byte, nss ...*nvmeprtest.Namespace) {
	t.Helper()
	start := map[*nvmeprtest.Namespace]int{}
	for _, ns := range nss {
		start[ns] = len(ns.Calls())
	}
	t.Cleanup(func() {
		for _, ns := range nss {
			for _, c := range ns.Calls()[start[ns]:] {
				if c.Host == host && c.Op == "register" && c.Keys[0] == 0 {
					t.Errorf("the fence sent a plain Register: %+v", c)
				}
			}
		}
	})
}

func fencerFor(t *testing.T, epoch uint64, byPath map[string]*nvmeprtest.Namespace) *Fencer {
	t.Helper()
	return &Fencer{Open: opens(hostID(1), byPath), HostID: hostID(1), Key: mdsKey(t, fenceUID, epoch)}
}

func one(ns *nvmeprtest.Namespace) map[string]*nvmeprtest.Namespace {
	return map[string]*nvmeprtest.Namespace{"/dev/nvme1n1": ns}
}

func TestFenceU05PreemptsAnOlderActiveAndHoldsTheReservation(t *testing.T) {
	ns := nvmeprtest.New()
	old := mdsKey(t, fenceUID, 1)
	become(t, ns, hostID(2), old, true)
	f := fencerFor(t, 2, one(ns))
	noPlainRegister(t, hostID(1), ns)

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if k, ok := holderKey(t, ns); !ok || k != f.Key {
		t.Errorf("holder = %x (held %v), want the new key %x", k, ok, f.Key)
	}
	if got := keysOf(reportOf(t, ns)); !slices.Equal(got, []uint64{f.Key}) {
		t.Errorf("registered keys = %x, want only %x", got, f.Key)
	}
	if ns.CanWrite(hostID(2)) {
		t.Error("the old active can still write")
	}
}

func TestFenceU06LeavesClientKeysAlone(t *testing.T) {
	ns := nvmeprtest.New()
	become(t, ns, hostID(2), mdsKey(t, fenceUID, 1), true)
	become(t, ns, hostID(3), clientKey(1), false)
	become(t, ns, hostID(4), clientKey(2), false)
	f := fencerFor(t, 2, one(ns))

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	want := []uint64{clientKey(1), clientKey(2), f.Key}
	slices.Sort(want)
	if got := keysOf(reportOf(t, ns)); !slices.Equal(got, want) {
		t.Errorf("registered keys = %x, want %x", got, want)
	}
	if !ns.CanWrite(hostID(3)) || !ns.CanWrite(hostID(4)) {
		t.Error("a client lost its access")
	}
}

func TestFenceU07RemovesAStaleKeyThatOnlyRegistered(t *testing.T) {
	ns := nvmeprtest.New()
	holding := mdsKey(t, fenceUID, 1)
	registeredOnly := mdsKey(t, "another-statefulset", 4)
	become(t, ns, hostID(2), holding, true)
	become(t, ns, hostID(3), registeredOnly, false)
	f := fencerFor(t, 2, one(ns))

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if got := keysOf(reportOf(t, ns)); !slices.Equal(got, []uint64{f.Key}) {
		t.Errorf("registered keys = %x, want only %x", got, f.Key)
	}
}

func TestFenceU08RegistersThenAcquiresOnANewNamespace(t *testing.T) {
	ns := nvmeprtest.New()
	f := fencerFor(t, 1, one(ns))
	noPlainRegister(t, hostID(1), ns)

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if got, want := commandsFrom(ns, hostID(1)), []string{"register", "acquire"}; !slices.Equal(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}
}

func TestFenceU09SendsNoCommandWhenItsKeyAlreadyHolds(t *testing.T) {
	ns := nvmeprtest.New()
	f := fencerFor(t, 3, one(ns))
	become(t, ns, hostID(1), f.Key, true)

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	// setup's own register and acquire are the only commands from this host
	if got := commandsFrom(ns, hostID(1)); !slices.Equal(got, []string{"register", "acquire"}) {
		t.Errorf("commands = %v, want only the setup's", got)
	}
}

func TestFenceU10AFailedPreemptNamesTheDeviceAndLeavesTheRestAlone(t *testing.T) {
	first, second := nvmeprtest.New(), nvmeprtest.New()
	become(t, first, hostID(2), mdsKey(t, fenceUID, 1), true)
	become(t, second, hostID(2), mdsKey(t, fenceUID, 1), true)
	first.FailNext("preempt", errors.New("target timeout"))
	byPath := map[string]*nvmeprtest.Namespace{"/dev/first": first, "/dev/second": second}
	f := fencerFor(t, 2, byPath)

	err := f.Fence(context.Background(), []string{"/dev/first", "/dev/second"})
	if err == nil {
		t.Fatal("Fence passed with a failing preempt")
	}
	if err.Outcome != nfsexportrpc.PromoteFenceFailed || err.Device != "/dev/first" || err.Step != "preempt" {
		t.Errorf("error = %+v, want FenceFailed on /dev/first at preempt", err)
	}
	if got := commandsFrom(second, hostID(1)); len(got) != 0 {
		t.Errorf("the second device was touched: %v", got)
	}
}

func TestFenceU11RefusesAKeyAnotherHostIsRegisteredUnder(t *testing.T) {
	ns := nvmeprtest.New()
	f := fencerFor(t, 2, one(ns))
	become(t, ns, hostID(9), f.Key, false)

	err := f.Fence(context.Background(), []string{"/dev/nvme1n1"})
	if err == nil || err.Outcome != nfsexportrpc.PromoteCollision {
		t.Fatalf("error = %+v, want a collision", err)
	}
	if got := commandsFrom(ns, hostID(1)); len(got) != 0 {
		t.Errorf("the fence changed the target before refusing: %v", got)
	}
}

func TestFenceU12RefusesAClientKeyHoldingTheReservation(t *testing.T) {
	ns := nvmeprtest.New()
	become(t, ns, hostID(3), clientKey(1), true)
	f := fencerFor(t, 2, one(ns))

	err := f.Fence(context.Background(), []string{"/dev/nvme1n1"})
	if err == nil || err.Outcome != nfsexportrpc.PromoteFenceFailed {
		t.Fatalf("error = %+v, want a failed fence", err)
	}
	if got := commandsFrom(ns, hostID(1)); len(got) != 0 {
		t.Errorf("the fence changed the target before refusing: %v", got)
	}
	if k, _ := holderKey(t, ns); k != clientKey(1) {
		t.Errorf("holder = %x, want the client's key untouched", k)
	}
}

// ignoresPreempt answers every Preempt with success and does nothing, as a
// target that took the command and did not act on it.
type ignoresPreempt struct{ nvmepr.Device }

func (ignoresPreempt) Preempt(context.Context, uint64, uint64, nvmepr.Type, bool) error { return nil }

func TestFenceU13RefusesWhenTheReportStillShowsAnotherMDSKey(t *testing.T) {
	ns := nvmeprtest.New()
	become(t, ns, hostID(2), mdsKey(t, fenceUID, 1), true)
	f := fencerFor(t, 2, one(ns))
	f.Open = func(string) (nvmepr.Device, error) { return ignoresPreempt{ns.Host(hostID(1))}, nil }

	err := f.Fence(context.Background(), []string{"/dev/nvme1n1"})
	if err == nil || err.Outcome != nfsexportrpc.PromoteFenceFailed || err.Step != "confirm" {
		t.Fatalf("error = %+v, want a failed fence at confirm", err)
	}
}

// abortLog records the abort flag of every Preempt, and can fail the abort form.
type abortLog struct {
	nvmepr.Device
	flags   *[]bool
	failAbt error
}

func (a abortLog) Preempt(ctx context.Context, key, victim uint64, t nvmepr.Type, abort bool) error {
	*a.flags = append(*a.flags, abort)
	if abort && a.failAbt != nil {
		return a.failAbt
	}
	return a.Device.Preempt(ctx, key, victim, t, abort)
}

func TestFenceU14FallsBackToAPlainPreemptOnInvalidFieldAndRemembersIt(t *testing.T) {
	ns := nvmeprtest.New(nvmeprtest.WithoutPreemptAndAbort())
	become(t, ns, hostID(2), mdsKey(t, fenceUID, 1), true)
	become(t, ns, hostID(3), mdsKey(t, "another-statefulset", 4), false)
	f := fencerFor(t, 2, one(ns))
	var flags []bool
	f.Open = func(string) (nvmepr.Device, error) { return abortLog{Device: ns.Host(hostID(1)), flags: &flags}, nil }

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if want := []bool{true, false, false}; !slices.Equal(flags, want) {
		t.Errorf("abort flags = %v, want %v: one abort attempt, then plain only", flags, want)
	}
	if got := keysOf(reportOf(t, ns)); !slices.Equal(got, []uint64{f.Key}) {
		t.Errorf("registered keys = %x, want only %x", got, f.Key)
	}
}

func TestFenceU14AnyOtherErrorFromPreemptAndAbortFailsTheFence(t *testing.T) {
	ns := nvmeprtest.New()
	become(t, ns, hostID(2), mdsKey(t, fenceUID, 1), true)
	f := fencerFor(t, 2, one(ns))
	var flags []bool
	f.Open = func(string) (nvmepr.Device, error) {
		return abortLog{Device: ns.Host(hostID(1)), flags: &flags, failAbt: errors.New("abort wait expired")}, nil
	}

	err := f.Fence(context.Background(), []string{"/dev/nvme1n1"})
	if err == nil || err.Step != "preempt" {
		t.Fatalf("error = %+v, want a failed fence at preempt", err)
	}
	if want := []bool{true}; !slices.Equal(flags, want) {
		t.Errorf("abort flags = %v, want %v: no plain preempt after another error", flags, want)
	}
}

func TestFenceU26ReplacesTheKeyOfAHostRegisteredByAnEarlierBoot(t *testing.T) {
	ns := nvmeprtest.New()
	previous := mdsKey(t, fenceUID, 1)
	become(t, ns, hostID(1), previous, true)
	f := fencerFor(t, 2, one(ns))
	noPlainRegister(t, hostID(1), ns)

	if err := f.Fence(context.Background(), []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if got := keysOf(reportOf(t, ns)); !slices.Equal(got, []uint64{f.Key}) {
		t.Errorf("registered keys = %x, want only the new key %x", got, f.Key)
	}
	if k, ok := holderKey(t, ns); !ok || k != f.Key {
		t.Errorf("holder = %x (held %v), want the new key", k, ok)
	}
	// the replace form: the old key is the new key, never zero
	var sent bool
	for _, c := range ns.Calls() {
		if c.Op == "register" && c.Keys[0] == f.Key && c.Keys[1] == f.Key {
			sent = true
		}
	}
	if !sent {
		t.Errorf("no Register(old=new=%x): %+v", f.Key, ns.Calls())
	}
}

func TestFenceU27AFullNamespaceFailsTheRegistrationAndPreemptsNothing(t *testing.T) {
	ns := nvmeprtest.New(nvmeprtest.WithMaxRegistrants(16))
	for i := range 16 {
		become(t, ns, hostID(byte(10+i)), clientKey(uint64(i+1)), false)
	}
	f := fencerFor(t, 1, one(ns))

	err := f.Fence(context.Background(), []string{"/dev/nvme1n1"})
	if err == nil || err.Outcome != nfsexportrpc.PromoteFenceFailed || err.Step != "register" || err.Device != "/dev/nvme1n1" {
		t.Fatalf("error = %+v, want FenceFailed on the device at register", err)
	}
	if got := commandsFrom(ns, hostID(1)); !slices.Equal(got, []string{"register"}) {
		t.Errorf("commands = %v, want the failed register alone", got)
	}
}

// A newer active fences an older one. The older guest's own promotion then
// runs again at its older epoch and must find the newer key and stop.
func TestFenceU28ASupersededEpochIsRefusedWithoutPreemptingTheNewerActive(t *testing.T) {
	ns := nvmeprtest.New()
	older := fencerFor(t, 1, one(ns))
	older.HostID, older.Open = hostID(2), opens(hostID(2), one(ns))
	newer := fencerFor(t, 2, one(ns))
	ctx := context.Background()

	if err := older.Fence(ctx, []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("older Fence: %v", err)
	}
	if err := newer.Fence(ctx, []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("newer Fence: %v", err)
	}
	before := len(ns.Calls())

	err := older.Fence(ctx, []string{"/dev/nvme1n1"})
	if err == nil || err.Outcome != nfsexportrpc.PromoteSuperseded {
		t.Fatalf("error = %+v, want Superseded", err)
	}
	if got := commandsFrom(ns, hostID(2)); !slices.Equal(got, []string{"register", "acquire"}) {
		t.Errorf("the older guest sent %v after being superseded", got[2:])
	}
	if k, ok := holderKey(t, ns); !ok || k != newer.Key {
		t.Errorf("holder = %x (held %v), want the newer key", k, ok)
	}
	if after := len(ns.Calls()); after-before > 2 { // its report, and this test's
		t.Errorf("%d commands after the refusal", after-before)
	}
}

func TestFenceU29FencesEveryMemberOfAStripedExport(t *testing.T) {
	byPath := map[string]*nvmeprtest.Namespace{}
	var paths []string
	for _, p := range []string{"/dev/m0", "/dev/m1", "/dev/m2"} {
		ns := nvmeprtest.New()
		become(t, ns, hostID(2), mdsKey(t, fenceUID, 1), true)
		byPath[p] = ns
		paths = append(paths, p)
	}
	f := fencerFor(t, 2, byPath)

	if err := f.Fence(context.Background(), paths); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	for p, ns := range byPath {
		if k, ok := holderKey(t, ns); !ok || k != f.Key {
			t.Errorf("%s: holder = %x (held %v), want %x", p, k, ok, f.Key)
		}
		if ns.CanWrite(hostID(2)) {
			t.Errorf("%s: the old active can still write", p)
		}
	}
}

func TestFenceU30AMemberThatKeepsAnotherKeyFailsTheWholeFence(t *testing.T) {
	good, bad := nvmeprtest.New(), nvmeprtest.New()
	become(t, good, hostID(2), mdsKey(t, fenceUID, 1), true)
	become(t, bad, hostID(2), mdsKey(t, fenceUID, 1), true)
	f := fencerFor(t, 2, map[string]*nvmeprtest.Namespace{"/dev/m0": good, "/dev/m1": bad})
	f.Open = func(path string) (nvmepr.Device, error) {
		if path == "/dev/m1" {
			return ignoresPreempt{bad.Host(hostID(1))}, nil
		}
		return good.Host(hostID(1)), nil
	}

	err := f.Fence(context.Background(), []string{"/dev/m0", "/dev/m1"})
	if err == nil || err.Device != "/dev/m1" || err.Step != "confirm" {
		t.Fatalf("error = %+v, want a failed fence of /dev/m1 at confirm", err)
	}
}

func TestFenceRetryAfterAFailedPreemptConverges(t *testing.T) {
	ns := nvmeprtest.New()
	become(t, ns, hostID(2), mdsKey(t, fenceUID, 1), true)
	become(t, ns, hostID(3), mdsKey(t, "another-statefulset", 4), false)
	ns.FailNext("preempt", errors.New("target timeout"))
	f := fencerFor(t, 2, one(ns))
	noPlainRegister(t, hostID(1), ns)
	ctx := context.Background()

	if err := f.Fence(ctx, []string{"/dev/nvme1n1"}); err == nil {
		t.Fatal("the first fence passed with a failing preempt")
	}
	if err := f.Fence(ctx, []string{"/dev/nvme1n1"}); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := keysOf(reportOf(t, ns)); !slices.Equal(got, []uint64{f.Key}) {
		t.Errorf("registered keys = %x, want only %x", got, f.Key)
	}
	if k, ok := holderKey(t, ns); !ok || k != f.Key {
		t.Errorf("holder = %x (held %v), want %x", k, ok, f.Key)
	}
}

func TestFenceReportsAnOpenFailureWithTheDevice(t *testing.T) {
	f := fencerFor(t, 1, nil)
	err := f.Fence(context.Background(), []string{"/dev/missing"})
	if err == nil || err.Device != "/dev/missing" || err.Step != "open" {
		t.Fatalf("error = %+v, want a failed fence of /dev/missing at open", err)
	}
}
