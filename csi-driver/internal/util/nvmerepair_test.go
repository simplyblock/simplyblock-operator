package util

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
)

// Detection is atlas's and is tested there, including against real kernel state.
// What is tested here is the part that is this driver's own: which diagnosed
// defect gets acted on, when policy refuses, and that the teardown runs the right
// nvme-cli command in the right order.

const (
	repairTestNQN = "nqn.2023-02.io.simplyblock:cluster:lvol:6dbb7d4e-2f1a-4a55-9d3c-1f2e3a4b5c6d"
	volA          = "6dbb7d4e-2f1a-4a55-9d3c-1f2e3a4b5c6d"
	volB          = "7ecc8e5f-3a2b-4b66-8e4d-2f3e4a5b6c7e"
)

func testCtrl(id, addr string) nvme.Controller {
	return nvme.Controller{
		ID:        nvme.ControllerID(id),
		Transport: "tcp",
		State:     "live",
		Address:   nvme.Address{TrAddr: addr, TrSvcID: "4420"},
	}
}

// defect builds a diagnosed defect of the shape atlas produces.
func defect(
	kind nvmeof.DefectKind,
	scope nvmeof.Scope,
	ctrls []nvme.Controller,
	coTenants ...nvme.Namespace,
) nvmeof.Defect {
	return nvmeof.Defect{
		Kind:        kind,
		NQN:         repairTestNQN,
		Scope:       scope,
		Controllers: ctrls,
		CoTenants:   coTenants,
		Subsystem:   "nvme-subsys0",
	}
}

// recordingRepairer is a repairer whose teardown is captured instead of run.
func recordingRepairer(t *testing.T) (*nvmeRepairer, *[]string) {
	t.Helper()
	var torn []string
	r := newNVMeRepairer()
	r.detacher = detacherFunc(func(_ context.Context, ctrl nvme.Controller) error {
		torn = append(torn, string(ctrl.ID))
		return nil
	})
	return r, &torn
}

func TestChoose_PrefersTheNarrowestRepair(t *testing.T) {
	r, _ := recordingRepairer(t)
	wide := defect(nvmeof.DefectNoNamespace, nvmeof.ScopeSubsystem,
		[]nvme.Controller{testCtrl("nvme0", "10.0.0.1"), testCtrl("nvme1", "10.0.0.2")})
	narrow := defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme1", "10.0.0.2")})

	action, ok := r.choose([]nvmeof.Defect{wide, narrow})
	if !ok || action.skipped != "" {
		t.Fatalf("choose = %+v, %v; want the narrow defect chosen", action, ok)
	}
	if action.defect.Kind != nvmeof.DefectControllerNotContributing {
		t.Errorf("chose %s, want the controller-scope repair: it disturbs less", action.defect.Kind)
	}
}

// A repair that would strand another volume is refused, and the refusal names
// the volume so the reason survives into the log.
func TestChoose_RefusesToStrandACoTenant(t *testing.T) {
	r, _ := recordingRepairer(t)
	d := defect(nvmeof.DefectNoNamespace, nvmeof.ScopeSubsystem,
		[]nvme.Controller{testCtrl("nvme0", "10.0.0.1")},
		nvme.Namespace{ID: 2, UUID: volB})

	action, ok := r.choose([]nvmeof.Defect{d})
	if !ok {
		t.Fatal("choose returned nothing; the refusal must be reported")
	}
	if action.skipped == "" {
		t.Fatal("the repair was permitted; it would leave another volume with no path")
	}
	if !strings.Contains(action.skipped, volB) {
		t.Errorf("skipped = %q, want the co-tenant volume named", action.skipped)
	}
}

func TestChoose_DisruptiveAllowedWhenAsked(t *testing.T) {
	r, _ := recordingRepairer(t)
	r.allowDisruptive = true
	d := defect(nvmeof.DefectNoNamespace, nvmeof.ScopeSubsystem,
		[]nvme.Controller{testCtrl("nvme0", "10.0.0.1")},
		nvme.Namespace{ID: 2, UUID: volB})

	if action, ok := r.choose([]nvmeof.Defect{d}); !ok || action.skipped != "" {
		t.Errorf("choose = %+v, %v; want the repair permitted once the caller allowed it", action, ok)
	}
}

func TestChoose_ScopeCap(t *testing.T) {
	r, _ := recordingRepairer(t)
	r.maxScope = nvmeof.ScopeController
	d := defect(nvmeof.DefectNoNamespace, nvmeof.ScopeSubsystem,
		[]nvme.Controller{testCtrl("nvme0", "10.0.0.1")})

	action, ok := r.choose([]nvmeof.Defect{d})
	if !ok || action.skipped == "" {
		t.Fatalf("choose = %+v, %v; want a scope-cap refusal", action, ok)
	}
	if !strings.Contains(action.skipped, "capped") {
		t.Errorf("skipped = %q, want the cap named", action.skipped)
	}
}

// A missing namespace is the target not publishing it, and a stale endpoint is
// indistinguishable from a node restarting. Neither is acted on unattended.
func TestChoose_SkipsKindsThatAreNotAutoRepaired(t *testing.T) {
	r, _ := recordingRepairer(t)
	for _, kind := range []nvmeof.DefectKind{nvmeof.DefectNamespaceMissing, nvmeof.DefectStaleEndpoint} {
		t.Run(string(kind), func(t *testing.T) {
			d := defect(kind, nvmeof.ScopeController, []nvme.Controller{testCtrl("nvme9", "10.0.0.9")})
			if action, ok := r.choose([]nvmeof.Defect{d}); ok {
				t.Errorf("choose = %+v; %s must not be repaired unattended", action, kind)
			}
		})
	}
}

func TestChoose_IgnoresDefectsWithNoRemedy(t *testing.T) {
	r, _ := recordingRepairer(t)
	d := defect(nvmeof.DefectAmbiguousHead, nvmeof.ScopeNone, nil)
	if action, ok := r.choose([]nvmeof.Defect{d}); ok {
		t.Errorf("choose = %+v; a ScopeNone defect has nothing to tear down", action)
	}
}

// The controller name is exactly what a repair changes — tearing down nvme3 and
// reconnecting yields nvme7 at the same address — so the cooldown has to key on
// the endpoint or it never recognises the same repair twice.
func TestCooldownKey_ControllerScopeKeysOnEndpoint(t *testing.T) {
	first := defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme3", "10.0.0.3")})
	renamed := defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme7", "10.0.0.3")})

	if cooldownKeyOf(first) != cooldownKeyOf(renamed) {
		t.Errorf("keys differ across a rename:\n  %+v\n  %+v", cooldownKeyOf(first), cooldownKeyOf(renamed))
	}

	other := defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme4", "10.0.0.4")})
	if cooldownKeyOf(first) == cooldownKeyOf(other) {
		t.Error("two different endpoints share a key; three broken paths are three repairs, not one")
	}
}

// A subsystem-scope repair concerns the subsystem behind this NQN, whatever
// instance the kernel hands out after the teardown. Keying on the controllers or
// on the instance id would lose the cooldown as soon as a reconnect renumbered
// either — reopening the loop the cooldown exists to close.
func TestCooldownKey_SubsystemScopeIgnoresInstanceAndControllers(t *testing.T) {
	withInstance := func(kind nvmeof.DefectKind, instance nvme.SubsystemID, ctrlID string) nvmeof.Defect {
		d := defect(kind, nvmeof.ScopeSubsystem, []nvme.Controller{testCtrl(ctrlID, "10.0.0.1")})
		d.Subsystem = instance
		return d
	}
	for _, kind := range []nvmeof.DefectKind{nvmeof.DefectNoNamespace, nvmeof.DefectNamespaceMissing} {
		t.Run(string(kind), func(t *testing.T) {
			a := withInstance(kind, "nvme-subsys0", "nvme0")
			b := withInstance(kind, "nvme-subsys4", "nvme5")
			if cooldownKeyOf(a) != cooldownKeyOf(b) {
				t.Errorf("%s keyed on state the repair itself re-creates; the cooldown would never fire", kind)
			}
		})
	}
}

// Inspect reports one ambiguous-head defect per stale instance, so several can
// be outstanding for one NQN. A shared key would let repairing the first mark
// the rest as handled and leave stale heads attached — and a lookup by NQN can
// then still return the wrong block device.
func TestCooldownKey_AmbiguousHeadsStayDistinct(t *testing.T) {
	head := func(instance nvme.SubsystemID, ctrlID string) nvmeof.Defect {
		d := defect(nvmeof.DefectAmbiguousHead, nvmeof.ScopeSubsystem,
			[]nvme.Controller{testCtrl(ctrlID, "10.0.0.9")})
		d.Subsystem = instance
		return d
	}
	if cooldownKeyOf(head("nvme-subsys1", "nvme8")) == cooldownKeyOf(head("nvme-subsys2", "nvme9")) {
		t.Fatal("two stale heads share a key; only one of them could ever be repaired")
	}
	// The same head after a repair that did not stick: the kernel re-created its
	// controller under a new name, but it is the same head and must stay cooled.
	if cooldownKeyOf(head("nvme-subsys1", "nvme8")) != cooldownKeyOf(head("nvme-subsys1", "nvme12")) {
		t.Error("a renamed controller made the same stale head look new; it would be retried immediately")
	}
}

func TestCooldown_BlocksTheSecondRepairAndExpires(t *testing.T) {
	r, _ := recordingRepairer(t)
	d := defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme3", "10.0.0.3")})

	if action, _ := r.choose([]nvmeof.Defect{d}); action.skipped != "" {
		t.Fatalf("first choose was refused: %s", action.skipped)
	}
	r.mark(d)

	action, ok := r.choose([]nvmeof.Defect{d})
	if !ok || action.skipped == "" {
		t.Fatalf("choose = %+v, %v; want a cooldown refusal", action, ok)
	}
	if !strings.Contains(action.skipped, "cooldown") {
		t.Errorf("skipped = %q, want the cooldown named", action.skipped)
	}

	base := time.Now()
	r.now = func() time.Time { return base.Add(defaultRepairCooldown + time.Second) }
	if action, _ := r.choose([]nvmeof.Defect{d}); action.skipped != "" {
		t.Errorf("still refused after the cooldown expired: %s", action.skipped)
	}
}

func TestCooldown_Disabled(t *testing.T) {
	r, _ := recordingRepairer(t)
	r.cooldown = 0
	d := defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme3", "10.0.0.3")})
	r.mark(d)
	if action, _ := r.choose([]nvmeof.Defect{d}); action.skipped != "" {
		t.Errorf("refused with the cooldown disabled: %s", action.skipped)
	}
}

// mark prunes long-expired entries so a node plugin running for months does not
// accumulate one per repair it ever made.
func TestCooldown_PrunesExpiredEntries(t *testing.T) {
	r, _ := recordingRepairer(t)
	base := time.Now()
	r.now = func() time.Time { return base }
	for _, addr := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		r.mark(defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
			[]nvme.Controller{testCtrl("nvme0", addr)}))
	}
	if got := len(r.last); got != 3 {
		t.Fatalf("tracked %d repairs, want 3", got)
	}

	r.now = func() time.Time { return base.Add(3 * defaultRepairCooldown) }
	r.mark(defect(nvmeof.DefectControllerNotContributing, nvmeof.ScopeController,
		[]nvme.Controller{testCtrl("nvme0", "10.0.0.9")}))
	if got := len(r.last); got != 1 {
		t.Errorf("tracked %d repairs after pruning, want only the fresh one", got)
	}
}

// Teardown follows the order atlas put the controllers in — paths that cannot
// serve I/O first, the optimized path last — and one that fails does not stop
// the ones behind it, or the optimized path would be left behind.
func TestRepair_TearsDownInOrderAndContinuesPastFailure(t *testing.T) {
	var attempted []string
	r := newNVMeRepairer()
	r.detacher = detacherFunc(func(_ context.Context, ctrl nvme.Controller) error {
		attempted = append(attempted, string(ctrl.ID))
		if ctrl.ID == "nvme1" {
			return errors.New("device busy")
		}
		return nil
	})

	d := defect(nvmeof.DefectNoNamespace, nvmeof.ScopeSubsystem, []nvme.Controller{
		testCtrl("nvme2", "10.0.0.3"),
		testCtrl("nvme1", "10.0.0.2"),
		testCtrl("nvme0", "10.0.0.1"),
	})
	err := r.repair(context.Background(), d)
	if err == nil {
		t.Error("err = nil, want the teardown failure reported")
	}
	if want := []string{"nvme2", "nvme1", "nvme0"}; !slices.Equal(attempted, want) {
		t.Errorf("teardown = %v, want %v", attempted, want)
	}
}

func TestRepair_RefusesADefectWithNoRemedy(t *testing.T) {
	r, torn := recordingRepairer(t)
	if err := r.repair(context.Background(), defect(nvmeof.DefectAmbiguousHead, nvmeof.ScopeNone, nil)); err == nil {
		t.Error("err = nil, want a refusal")
	}
	if len(*torn) != 0 {
		t.Errorf("tore down %v, want nothing", *torn)
	}
}

func TestTargetsFromConnections(t *testing.T) {
	conns := []*LvolConnectResp{
		{Nqn: repairTestNQN, IP: "10.0.0.1", Port: 4420, TargetType: "TCP"},
		nil, // the API has produced these; a nil must not panic or become a target
		{Nqn: repairTestNQN, IP: "10.0.0.2", Port: 4421},
	}
	targets := targetsFromConnections(repairTestNQN, conns)
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 (the nil is skipped)", len(targets))
	}
	if targets[0].Address != "10.0.0.1" || targets[0].Port != 4420 {
		t.Errorf("target[0] = %+v, want the first endpoint in order", targets[0])
	}
	for i, tgt := range targets {
		if tgt.NQN != repairTestNQN {
			t.Errorf("target[%d] NQN = %q, want the volume's", i, tgt.NQN)
		}
		if tgt.Transport != nvmeof.TransportTCP {
			t.Errorf("target[%d] transport = %q, want tcp (defaulted when the API omits it)", i, tgt.Transport)
		}
	}
}

// healSubsystem drives atlas's Inspect over a fake kernel and then this file's
// policy, so the seam between the two is covered end to end.
func TestHealSubsystem_RepairsAnOrphanedController(t *testing.T) {
	// nvme1 is live at a published endpoint and serves no path to the namespace.
	ns := nvme.Namespace{
		ID: 1, Name: "nvme0n1", DevicePath: "/dev/nvme0n1", UUID: volA,
		Paths: []nvme.Path{{Controller: "nvme0", NSID: 1, ANAState: nvme.ANAOptimized}},
	}
	sub := nvme.Subsystem{
		ID: "nvme-subsys0", NQN: repairTestNQN,
		Controllers: []nvme.Controller{testCtrl("nvme0", "10.0.0.1"), testCtrl("nvme1", "10.0.0.2")},
		Namespaces:  []nvme.Namespace{ns},
	}

	r, torn := recordingRepairer(t)
	r.subs = fakeSubs{sub: sub}
	r.devs = fakeDevs{devices: []nvme.Device{{Namespace: ns, Subsystem: sub}}}

	targets := targetsFromConnections(repairTestNQN, []*LvolConnectResp{
		{IP: "10.0.0.1", Port: 4420}, {IP: "10.0.0.2", Port: 4420},
	})
	defects, actions := r.healSubsystem(context.Background(),
		nvme.DeviceSelector{NQN: repairTestNQN, UUID: volA}, targets)

	if len(defects) == 0 {
		t.Fatal("no defect diagnosed; nvme1 serves no path to the namespace")
	}
	if len(actions) != 1 || !actions[0].repaired {
		t.Fatalf("actions = %v, want one repair", actions)
	}
	if actions[0].defect.Kind != nvmeof.DefectControllerNotContributing {
		t.Errorf("repaired %s, want controller-not-contributing", actions[0].defect.Kind)
	}
	if want := []string{"nvme1"}; !slices.Equal(*torn, want) {
		t.Errorf("tore down %v, want only the orphan %v", *torn, want)
	}
}

func TestHealSubsystem_HealthyVolumeIsUntouched(t *testing.T) {
	ns := nvme.Namespace{
		ID: 1, Name: "nvme0n1", DevicePath: "/dev/nvme0n1", UUID: volA,
		Paths: []nvme.Path{
			{Controller: "nvme0", NSID: 1, ANAState: nvme.ANAOptimized},
			{Controller: "nvme1", NSID: 1, ANAState: nvme.ANANonOptimized},
		},
	}
	sub := nvme.Subsystem{
		ID: "nvme-subsys0", NQN: repairTestNQN,
		Controllers: []nvme.Controller{testCtrl("nvme0", "10.0.0.1"), testCtrl("nvme1", "10.0.0.2")},
		Namespaces:  []nvme.Namespace{ns},
	}

	r, torn := recordingRepairer(t)
	r.subs = fakeSubs{sub: sub}
	r.devs = fakeDevs{devices: []nvme.Device{{Namespace: ns, Subsystem: sub}}}

	targets := targetsFromConnections(repairTestNQN, []*LvolConnectResp{
		{IP: "10.0.0.1", Port: 4420}, {IP: "10.0.0.2", Port: 4420},
	})
	defects, actions := r.healSubsystem(context.Background(),
		nvme.DeviceSelector{NQN: repairTestNQN, UUID: volA}, targets)

	if len(defects) != 0 || len(actions) != 0 {
		t.Errorf("defects = %v actions = %v, want nothing on a healthy volume", defects, actions)
	}
	if len(*torn) != 0 {
		t.Errorf("tore down %v on a healthy volume", *torn)
	}
}

// detacherFunc adapts a function to nvmeof.ControllerDetacher so a test can
// watch the teardown without running nvme-cli.
type detacherFunc func(ctx context.Context, ctrl nvme.Controller) error

func (f detacherFunc) DisconnectController(ctx context.Context, ctrl nvme.Controller) error {
	return f(ctx, ctrl)
}

// fakeSubs and fakeDevs stand in for the local sysfs.
type fakeSubs struct{ sub nvme.Subsystem }

func (f fakeSubs) List(context.Context) ([]nvme.Subsystem, error) {
	return []nvme.Subsystem{f.sub}, nil
}

func (f fakeSubs) ByNQN(_ context.Context, nqn string) (nvme.Subsystem, error) {
	if f.sub.NQN != nqn {
		return nvme.Subsystem{}, errs404
	}
	return f.sub, nil
}

type fakeDevs struct{ devices []nvme.Device }

func (f fakeDevs) List(context.Context) ([]nvme.Device, error) { return f.devices, nil }

func (f fakeDevs) ListWithSelector(_ context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	return sel.Filter(f.devices), nil
}

func (f fakeDevs) ByUUID(context.Context, string) (nvme.Device, error) { return nvme.Device{}, errs404 }
func (f fakeDevs) ByDevicePath(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, errs404
}

func (f fakeDevs) ByNamespace(context.Context, string, nvme.NamespaceID) (nvme.Device, error) {
	return nvme.Device{}, errs404
}

// errs404 must wrap atlas's sentinel: Inspect reads errs.ErrNotFound as "this
// subsystem is not attached", which is not a defect, and anything else as a
// failure to diagnose.
var errs404 = fmt.Errorf("subsystem: %w", errs.ErrNotFound)
