package nvmeof

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

const (
	volA = "6dbb7d4e-2f1a-4a55-9d3c-1f2e3a4b5c6d"
	volB = "7ecc8e5f-3a2b-4b66-8e4d-2f3e4a5b6c7e"
)

// liveCtrl is a live controller fronting addr, with the sysfs path a teardown
// needs.
func liveCtrl(id, addr string) nvme.Controller {
	c := ctrl(id, addr, "live")
	c.SysfsPath = "/sys/class/nvme/" + id
	c.DevicePath = "/dev/" + id
	return c
}

// namespaceOn is a namespace served by the named controllers over optimized ANA
// paths — a healthy multipath namespace.
func namespaceOn(nsid nvme.NamespaceID, uuid string, ctrlIDs ...string) nvme.Namespace {
	ns := nvme.Namespace{
		ID:         nsid,
		Name:       "nvme0n" + string(rune('0'+nsid)),
		DevicePath: "/dev/nvme0n" + string(rune('0'+nsid)),
		UUID:       uuid,
	}
	for _, id := range ctrlIDs {
		ns.Paths = append(ns.Paths, nvme.Path{
			Controller: nvme.ControllerID(id),
			NSID:       nsid,
			ANAState:   nvme.ANAOptimized,
		})
	}
	return ns
}

// subsystem assembles an attached subsystem from live controllers and namespaces.
func subsystem(ctrls []nvme.Controller, nss ...nvme.Namespace) nvme.Subsystem {
	return nvme.Subsystem{
		ID:          "nvme-subsys0",
		NQN:         testNQN,
		Controllers: ctrls,
		Namespaces:  nss,
	}
}

// inspectSub runs Inspect over a single fixed subsystem, with no duplicate-head
// check (that needs a device view; see the AmbiguousHead tests).
func inspectSub(t *testing.T, s nvme.Subsystem, sel nvme.DeviceSelector, tgts []Target) []Defect {
	t.Helper()
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return s, nil }}
	defects, err := Inspect(context.Background(), subs, nil, sel, tgts)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return defects
}

func kinds(defects []Defect) []DefectKind {
	out := make([]DefectKind, len(defects))
	for i, d := range defects {
		out[i] = d.Kind
	}
	return out
}

func only(t *testing.T, defects []Defect, kind DefectKind) Defect {
	t.Helper()
	if len(defects) != 1 || defects[0].Kind != kind {
		t.Fatalf("defects = %v, want exactly one %s", kinds(defects), kind)
	}
	return defects[0]
}

func TestInspect_HealthySubsystemIsSilent(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
		namespaceOn(1, volA, "nvme0", "nvme1"),
	)
	defects := inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, NSID: 1}, targets("10.0.0.1", "10.0.0.2"))
	if len(defects) != 0 {
		t.Errorf("defects = %v, want none for a healthy subsystem", defects)
	}
}

func TestInspect_NotAttachedIsNotADefect(t *testing.T) {
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return notFound() }}
	defects, err := Inspect(context.Background(), subs, nil, nvme.DeviceSelector{NQN: testNQN}, nil)
	if err != nil || len(defects) != 0 {
		t.Errorf("Inspect = %v, %v; want no defects and no error: a plain connect is the fix", defects, err)
	}
}

func TestInspect_RequiresAnNQN(t *testing.T) {
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return notFound() }}
	_, err := Inspect(context.Background(), subs, nil, nvme.DeviceSelector{}, nil)
	if !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("err = %v, want errs.ErrUnsupported", err)
	}
}

// The 498c2c5 state: the connection came up, the controllers are live, and the
// subsystem exports nothing. No amount of waiting produces a block device.
func TestInspect_NoNamespaceExported(t *testing.T) {
	s := subsystem([]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")})

	d := only(t, inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN}, nil), DefectNoNamespace)
	if d.Scope != ScopeSubsystem {
		t.Errorf("scope = %s, want subsystem: only a teardown re-runs the namespace scan", d.Scope)
	}
	if d.Disruptive() {
		t.Errorf("co-tenants = %v, want none: a subsystem exporting nothing has no volume to lose", d.CoTenants)
	}
	if !d.Repairable() {
		t.Error("defect is not repairable, want a teardown of every controller")
	}
	// Teardown order with no ANA information at all: reverse of connect order,
	// since the kernel numbers controllers as they are created.
	if got := ctrlIDs(d.Controllers); !slices.Equal(got, []nvme.ControllerID{"nvme1", "nvme0"}) {
		t.Errorf("teardown order = %v, want [nvme1 nvme0]", got)
	}
}

// A subsystem still connecting is not broken, just early: tearing it down would
// destroy the connect that is in progress.
func TestInspect_NoNamespaceNeedsALiveController(t *testing.T) {
	s := subsystem([]nvme.Controller{ctrl("nvme0", "10.0.0.1", "connecting")})
	if defects := inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN}, nil); len(defects) != 0 {
		t.Errorf("defects = %v, want none while the controller is still connecting", kinds(defects))
	}
}

// The f99b5b5 state: the controller is live and at a published endpoint, so a
// connect declines to act, but the namespace's own path list does not mention
// it — the volume runs a path short and the two views never reconcile.
func TestInspect_ControllerContributesNoPath(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2"), liveCtrl("nvme2", "10.0.0.3")},
		namespaceOn(1, volA, "nvme0", "nvme1"), // nvme2 joined no namespace
	)

	d := only(t,
		inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, NSID: 1}, targets("10.0.0.1", "10.0.0.2", "10.0.0.3")),
		DefectControllerNotContributing)
	if d.Scope != ScopeController {
		t.Errorf("scope = %s, want controller: the other two paths must survive the repair", d.Scope)
	}
	if got := ctrlIDs(d.Controllers); !slices.Equal(got, []nvme.ControllerID{"nvme2"}) {
		t.Errorf("controllers = %v, want only the orphan [nvme2]", got)
	}
	if d.Disruptive() {
		t.Errorf("co-tenants = %v, want none: nothing else depends on nvme2", d.CoTenants)
	}
	if !strings.Contains(d.Detail, "10.0.0.3") {
		t.Errorf("detail = %q, want the orphan's endpoint named", d.Detail)
	}
}

// Without native multipath the kernel publishes no per-controller ANA view.
// Reading that absence as "no controller contributes" would condemn every path
// of a perfectly healthy subsystem.
func TestInspect_NoPathListMeansNoVerdict(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
		nvme.Namespace{ID: 1, Name: "nvme0n1", UUID: volA}, // no Paths
	)
	if defects := inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, NSID: 1},
		targets("10.0.0.1", "10.0.0.2")); len(defects) != 0 {
		t.Errorf("defects = %v, want none when the kernel publishes no ANA view", kinds(defects))
	}
}

// One controller, two verdicts is one repair too many: an endpoint the control
// plane dropped is stale, not broken, and the two have different remedies.
func TestInspect_StaleEndpointIsNotAlsoNotContributing(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme9", "10.0.0.9")},
		namespaceOn(1, volA, "nvme0"), // the migrated-away node serves nothing
	)

	d := only(t, inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, NSID: 1}, targets("10.0.0.1")),
		DefectStaleEndpoint)
	if got := ctrlIDs(d.Controllers); !slices.Equal(got, []nvme.ControllerID{"nvme9"}) {
		t.Errorf("controllers = %v, want [nvme9]", got)
	}
	if d.Repairable() && slices.Contains(autoRepairKinds, d.Kind) {
		t.Error("a stale endpoint is auto-repairable; dropping a live data path is the caller's call")
	}
}

// Namespaces, but not this one. The connection works — the target simply is not
// publishing it — and every namespace that is there belongs to someone else.
func TestInspect_NamespaceMissing(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1")},
		namespaceOn(1, volA, "nvme0"),
		namespaceOn(2, volB, "nvme0"),
	)

	d := only(t, inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, NSID: 3}, targets("10.0.0.1")),
		DefectNamespaceMissing)
	if len(d.CoTenants) != 2 {
		t.Errorf("co-tenants = %v, want both existing namespaces: a teardown here is pure collateral damage",
			d.CoTenants)
	}
	if !d.Disruptive() {
		t.Error("defect is not disruptive; tearing the subsystem down would take two other volumes with it")
	}
	if slices.Contains(autoRepairKinds, d.Kind) {
		t.Error("a missing namespace is auto-repaired; reconnecting cannot make the target publish it")
	}
}

// Tearing down one leg of a shared subsystem is safe exactly when every other
// volume keeps a usable path through a surviving leg.
func TestInspect_BlastRadiusOfAControllerRepair(t *testing.T) {
	sel := nvme.DeviceSelector{NQN: testNQN, NSID: 1}
	tgts := targets("10.0.0.1", "10.0.0.2")

	t.Run("co-tenant keeps another path", func(t *testing.T) {
		s := subsystem(
			[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
			namespaceOn(1, volA, "nvme0"),          // ours: nvme1 contributes nothing
			namespaceOn(2, volB, "nvme0", "nvme1"), // co-tenant: two paths
		)
		d := only(t, inspectSub(t, s, sel, tgts), DefectControllerNotContributing)
		if d.Disruptive() {
			t.Errorf("co-tenants = %v, want none: volume B still has nvme0", d.CoTenants)
		}
	})

	t.Run("co-tenant loses its last path", func(t *testing.T) {
		s := subsystem(
			[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
			namespaceOn(1, volA, "nvme0"), // ours: nvme1 contributes nothing
			namespaceOn(2, volB, "nvme1"), // co-tenant: only nvme1
		)
		d := only(t, inspectSub(t, s, sel, tgts), DefectControllerNotContributing)
		if len(d.CoTenants) != 1 || d.CoTenants[0].UUID != volB {
			t.Fatalf("co-tenants = %v, want volume B, which has no other path", d.CoTenants)
		}
	})

	t.Run("co-tenant path is already inaccessible", func(t *testing.T) {
		coTenant := namespaceOn(2, volB, "nvme0", "nvme1")
		coTenant.Paths[0].ANAState = nvme.ANAInaccessible // nvme0 cannot serve it
		s := subsystem(
			[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
			namespaceOn(1, volA, "nvme0"),
			coTenant,
		)
		d := only(t, inspectSub(t, s, sel, tgts), DefectControllerNotContributing)
		if len(d.CoTenants) != 1 {
			t.Errorf("co-tenants = %v, want volume B: its only accessible path is the one being torn down",
				d.CoTenants)
		}
	})
}

// Two kernel subsystem instances for one NQN. Handing back either block device
// is a coin flip and the wrong side of it is silent corruption.
func TestInspect_AmbiguousHead(t *testing.T) {
	fresh := consistent(reachableDev("nvme-subsys0", testNQN, "nvme0n1", "259:1", 1))
	stale := consistent(staleDev("nvme-subsys1", testNQN, "nvme1n1", "259:2", 1))
	devs := &fakeDevs{snapshots: [][]nvme.Device{{fresh, stale}}}
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return fresh.Subsystem, nil }}

	defects, err := Inspect(context.Background(), subs, devs, nvme.DeviceSelector{NQN: testNQN, NSID: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := only(t, defects, DefectAmbiguousHead)
	if d.Subsystem != "nvme-subsys1" {
		t.Errorf("subsystem = %s, want the stale instance nvme-subsys1", d.Subsystem)
	}
	if d.Scope != ScopeSubsystem || !d.Repairable() {
		t.Errorf("scope = %s repairable = %v, want a repairable subsystem-scope defect", d.Scope, d.Repairable())
	}
	if got := ctrlIDs(d.Controllers); !slices.Equal(got, []nvme.ControllerID{"nvme1"}) {
		t.Errorf("controllers = %v, want only the stale instance's [nvme1]", got)
	}
}

// Reachability says which devices are serviceable, not which one this connect
// created. Tearing down a guess is how a healthy volume loses its data path.
func TestInspect_AmbiguousHeadWithNoStaleSideIsNotRepairable(t *testing.T) {
	a := consistent(reachableDev("nvme-subsys0", testNQN, "nvme0n1", "259:1", 1))
	b := consistent(reachableDev("nvme-subsys1", testNQN, "nvme1n1", "259:2", 1))
	devs := &fakeDevs{snapshots: [][]nvme.Device{{a, b}}}
	subs := fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return a.Subsystem, nil }}

	defects, err := Inspect(context.Background(), subs, devs, nvme.DeviceSelector{NQN: testNQN, NSID: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := only(t, defects, DefectAmbiguousHead)
	if d.Scope != ScopeNone || d.Repairable() {
		t.Errorf("scope = %s repairable = %v, want an unrepairable report", d.Scope, d.Repairable())
	}
}

// One namespace reported once per controller is one volume on several paths, not
// several volumes.
func TestInspect_DuplicateNamespaceEntriesAreOneNamespace(t *testing.T) {
	ns := namespaceOn(1, volA, "nvme0")
	s := subsystem([]nvme.Controller{liveCtrl("nvme0", "10.0.0.1")}, ns, ns)
	if defects := inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, NSID: 1},
		targets("10.0.0.1")); len(defects) != 0 {
		t.Errorf("defects = %v, want none: the repeat is the same namespace", kinds(defects))
	}
}

// A selector matching several namespaces of one subsystem is under-specified,
// which no reconnect fixes; naming "the" namespace's controllers would be a
// guess about which one was meant.
func TestInspect_UnderSpecifiedSelectorYieldsNoNamespaceVerdict(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
		namespaceOn(1, volA, "nvme0"),
		namespaceOn(2, volB, "nvme0"),
	)
	defects := inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN}, targets("10.0.0.1", "10.0.0.2"))
	for _, d := range defects {
		if d.Kind == DefectControllerNotContributing || d.Kind == DefectNamespaceMissing {
			t.Errorf("defect %s reported for an under-specified selector", d.Kind)
		}
	}
}

func TestInspect_SelectsNamespaceByUUID(t *testing.T) {
	s := subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")},
		namespaceOn(1, volA, "nvme0", "nvme1"),
		namespaceOn(2, volB, "nvme0"), // volume B is a path short, but is not ours
	)
	if defects := inspectSub(t, s, nvme.DeviceSelector{NQN: testNQN, UUID: volA},
		targets("10.0.0.1", "10.0.0.2")); len(defects) != 0 {
		t.Errorf("defects = %v, want none: volume A has both paths", kinds(defects))
	}
}

// consistent fills in the subsystem's namespace list from the device's own
// namespace, which the dev() helpers leave empty but a real resolver populates.
// Without it a fixture looks like a subsystem exporting nothing.
func consistent(d nvme.Device) nvme.Device {
	d.Subsystem.Namespaces = []nvme.Namespace{d.Namespace}
	return d
}

func ctrlIDs(ctrls []nvme.Controller) []nvme.ControllerID {
	out := make([]nvme.ControllerID, len(ctrls))
	for i, c := range ctrls {
		out[i] = c.ID
	}
	return out
}
