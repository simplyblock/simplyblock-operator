package nvmeof

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
)

// kernel is a fake NVMe stack that models the two states this package exists to
// repair, and models how they clear: a fresh controller re-runs the namespace
// scan, so tearing the broken one down is what recovers it.
//
//   - pathless: connects produce live controllers and the subsystem exports no
//     namespace at all, until every controller has been torn down.
//   - orphan: a controller at this address joins the subsystem but serves no
//     namespace, until that controller has been torn down.
type kernel struct {
	nsids  []nvme.NamespaceID
	uuids  map[nvme.NamespaceID]string
	orphan map[string]bool
	// serves narrows which namespaces a given address serves. nil means all.
	serves   func(addr string, nsid nvme.NamespaceID) bool
	pathless bool
	// sticky keeps a teardown from clearing the fault, modeling a repair that
	// does not stick.
	sticky bool
	// connectErr fails the connect for these addresses.
	connectErr map[string]error

	ctrls   []nvme.Controller
	serving map[nvme.NamespaceID]map[nvme.ControllerID]bool
	inst    int

	connects []string
	deleted  []nvme.ControllerID
}

func newKernel(nsids ...nvme.NamespaceID) *kernel {
	k := &kernel{
		nsids:   nsids,
		uuids:   map[nvme.NamespaceID]string{1: volA, 2: volB},
		orphan:  map[string]bool{},
		serving: map[nvme.NamespaceID]map[nvme.ControllerID]bool{},
	}
	for _, nsid := range nsids {
		k.serving[nsid] = map[nvme.ControllerID]bool{}
	}
	return k
}

func (k *kernel) at(addr string) (nvme.Controller, bool) {
	for _, c := range k.ctrls {
		if c.Address.TrAddr == addr {
			return c, true
		}
	}
	return nvme.Controller{}, false
}

func (k *kernel) Connect(ctx context.Context, t Target) error {
	res, err := k.ConnectPaths(ctx, []Target{t})
	if err != nil {
		return err
	}
	return res[0].Err
}

func (k *kernel) ConnectPaths(_ context.Context, targets []Target) ([]PathResult, error) {
	results := make([]PathResult, 0, len(targets))
	live := 0
	for _, t := range targets {
		r := PathResult{Target: t}
		switch _, present := k.at(t.Address); {
		case present:
			r.AlreadyPresent, r.Live = true, true
		case k.connectErr[t.Address] != nil:
			r.Err = k.connectErr[t.Address]
		default:
			k.attach(t.Address)
			r.Live = true
		}
		if r.Live {
			live++
		}
		results = append(results, r)
	}
	if live == 0 {
		reasons := make([]error, 0, len(results))
		for _, r := range results {
			if r.Err != nil {
				reasons = append(reasons, r.Err)
			}
		}
		return results, fmt.Errorf("connect %s: no path could be established: %w",
			targets[0].NQN, errors.Join(reasons...))
	}
	return results, nil
}

// attach creates a live controller for addr and joins it to the namespaces it
// serves, and none at all when the address is an orphan.
func (k *kernel) attach(addr string) {
	id := fmt.Sprintf("nvme%d", k.inst)
	k.inst++
	k.ctrls = append(k.ctrls, liveCtrl(id, addr))
	k.connects = append(k.connects, addr)
	if k.orphan[addr] {
		return
	}
	for _, nsid := range k.nsids {
		if k.serves == nil || k.serves(addr, nsid) {
			k.serving[nsid][nvme.ControllerID(id)] = true
		}
	}
}

func (k *kernel) Disconnect(ctx context.Context, _ string) error {
	for _, c := range disconnectOrder(k.subsystem()) {
		if err := k.DisconnectController(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (k *kernel) DisconnectController(_ context.Context, ctrl nvme.Controller) error {
	k.deleted = append(k.deleted, ctrl.ID)
	k.ctrls = slices.DeleteFunc(k.ctrls, func(c nvme.Controller) bool { return c.ID == ctrl.ID })
	for _, m := range k.serving {
		delete(m, ctrl.ID)
	}
	if !k.sticky {
		delete(k.orphan, ctrl.Address.TrAddr)
		if len(k.ctrls) == 0 {
			k.pathless = false
		}
	}
	return nil
}

func (k *kernel) IsConnected(context.Context, string) (bool, error) { return len(k.ctrls) > 0, nil }

// subsystem is what a sysfs scan would report for the current state.
func (k *kernel) subsystem() nvme.Subsystem {
	s := nvme.Subsystem{ID: "nvme-subsys0", NQN: testNQN, Controllers: slices.Clone(k.ctrls)}
	if k.pathless {
		return s
	}
	for _, nsid := range k.nsids {
		ns := nvme.Namespace{
			ID:         nsid,
			Name:       fmt.Sprintf("nvme0n%d", nsid),
			DevicePath: fmt.Sprintf("/dev/nvme0n%d", nsid),
			Dev:        fmt.Sprintf("259:%d", nsid),
			UUID:       k.uuids[nsid],
		}
		for _, c := range k.ctrls {
			if k.serving[nsid][c.ID] {
				ns.Paths = append(ns.Paths, nvme.Path{Controller: c.ID, NSID: nsid, ANAState: nvme.ANAOptimized})
			}
		}
		s.Namespaces = append(s.Namespaces, ns)
	}
	return s
}

// kernelSubs and kernelDevs are the two resolver views of one kernel. They are
// separate types because SubsystemResolver.List and DeviceResolver.List differ
// only in return type.
type kernelSubs struct{ k *kernel }

func (r kernelSubs) List(context.Context) ([]nvme.Subsystem, error) {
	return []nvme.Subsystem{r.k.subsystem()}, nil
}

func (r kernelSubs) ByNQN(_ context.Context, nqn string) (nvme.Subsystem, error) {
	if len(r.k.ctrls) == 0 {
		return notFound()
	}
	s := r.k.subsystem()
	if s.NQN != nqn {
		return notFound()
	}
	return s, nil
}

type kernelDevs struct{ k *kernel }

func (r kernelDevs) List(context.Context) ([]nvme.Device, error) {
	s := r.k.subsystem()
	out := make([]nvme.Device, 0, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		out = append(out, nvme.Device{Namespace: ns, Subsystem: s})
	}
	return out, nil
}

func (r kernelDevs) ListWithSelector(ctx context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	return sel.Filter(all), nil
}

func (r kernelDevs) ByUUID(context.Context, string) (nvme.Device, error) { return nvme.Device{}, nil }
func (r kernelDevs) ByDevicePath(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, nil
}

func (r kernelDevs) ByNamespace(context.Context, string, nvme.NamespaceID) (nvme.Device, error) {
	return nvme.Device{}, nil
}

// repairer wires a Repairer to k with a settle window short enough that tests do
// not sit out a real one.
func repairer(k *kernel, opts ...RepairOption) *Repairer {
	opts = append([]RepairOption{WithSettleWindow(5 * time.Millisecond)}, opts...)
	return NewRepairer(k, kernelSubs{k}, kernelDevs{k}, opts...)
}

func repairedKinds(actions []RepairAction) []DefectKind {
	var out []DefectKind
	for _, a := range actions {
		if a.Repaired {
			out = append(out, a.Defect.Kind)
		}
	}
	return out
}

func TestAttach_HappyPath(t *testing.T) {
	k := newKernel(1)
	res, err := repairer(k).Attach(context.Background(), targets("10.0.0.1", "10.0.0.2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Device.Namespace.DevicePath != "/dev/nvme0n1" {
		t.Errorf("device = %q, want /dev/nvme0n1", res.Device.Namespace.DevicePath)
	}
	if len(res.Defects) != 0 || len(res.Repairs) != 0 {
		t.Errorf("defects = %v repairs = %v, want none", res.Defects, res.Repairs)
	}
	if !slices.Equal(k.connects, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Errorf("connects = %v, want both paths in priority order", k.connects)
	}
	if res.Degraded() {
		t.Error("result reports degraded with both paths live")
	}
}

func TestAttach_IsIdempotent(t *testing.T) {
	k := newKernel(1)
	r := repairer(k)
	tgts := targets("10.0.0.1", "10.0.0.2")
	if _, err := r.Attach(context.Background(), tgts, 1); err != nil {
		t.Fatal(err)
	}
	connects := len(k.connects)

	res, err := r.Attach(context.Background(), tgts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.connects) != connects {
		t.Errorf("second attach connected again: %v", k.connects)
	}
	if len(k.deleted) != 0 {
		t.Errorf("second attach tore down %v, want nothing on a healthy fabric", k.deleted)
	}
	if res.Device.Namespace.DevicePath == "" {
		t.Error("second attach returned no device")
	}
}

// The 498c2c5 incident: live controllers, no namespace, and every retry
// short-circuiting on "already connected" while waiting for a device that will
// never appear. Only a teardown makes the kernel re-enumerate.
func TestAttach_RepairsSubsystemExportingNoNamespace(t *testing.T) {
	k := newKernel(1)
	k.pathless = true

	res, err := repairer(k).Attach(context.Background(), targets("10.0.0.1", "10.0.0.2"), 1)
	if err != nil {
		t.Fatalf("attach = %v, want the subsystem repaired", err)
	}
	if res.Device.Namespace.DevicePath != "/dev/nvme0n1" {
		t.Errorf("device = %q, want a device after the repair", res.Device.Namespace.DevicePath)
	}
	if got := repairedKinds(res.Repairs); !slices.Equal(got, []DefectKind{DefectNoNamespace}) {
		t.Errorf("repairs = %v, want one no-namespace repair", res.Repairs)
	}
	if len(k.deleted) != 2 {
		t.Errorf("deleted = %v, want both controllers: the whole subsystem has to go", k.deleted)
	}
	// Torn down and reconnected, so the second pair of controllers is what is
	// serving: four connects in total.
	if len(k.connects) != 4 {
		t.Errorf("connects = %v, want the two paths re-established", k.connects)
	}
}

// The f99b5b5 incident: the volume HAS a device, running at two of three paths,
// which is why waiting for a device never notices, and why the repair has to
// happen on an already-attached volume.
func TestAttach_RepairsOrphanedControllerWhileDeviceIsPresent(t *testing.T) {
	k := newKernel(1)
	k.orphan["10.0.0.3"] = true

	res, err := repairer(k).Attach(context.Background(), targets("10.0.0.1", "10.0.0.2", "10.0.0.3"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := repairedKinds(res.Repairs); !slices.Equal(got, []DefectKind{DefectControllerNotContributing}) {
		t.Errorf("repairs = %v, want the orphaned controller repaired", res.Repairs)
	}
	if !slices.Equal(k.deleted, []nvme.ControllerID{"nvme2"}) {
		t.Errorf("deleted = %v, want only the orphan nvme2: the other two paths carry I/O", k.deleted)
	}
	if got := len(k.subsystem().Namespaces[0].Paths); got != 3 {
		t.Errorf("paths after repair = %d, want all 3 serving the namespace", got)
	}
}

// Repairing one volume by taking another volume's block device away is not a
// repair. The pods on the co-tenant see ext4 remount read-only.
func TestAttach_RefusesToDisturbACoTenant(t *testing.T) {
	k := newKernel(1, 2)
	// Each node serves exactly one of the two volumes, so the controller that
	// contributes nothing to ours is the co-tenant's only path.
	k.serves = func(addr string, nsid nvme.NamespaceID) bool {
		return (addr == "10.0.0.1") == (nsid == 1)
	}

	res, err := repairer(k).Attach(context.Background(), targets("10.0.0.1", "10.0.0.2"), 1)
	if err != nil {
		t.Fatalf("attach = %v, want success: our own volume has its device", err)
	}
	if len(k.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing torn down", k.deleted)
	}
	if len(res.Repairs) != 1 || res.Repairs[0].Repaired {
		t.Fatalf("repairs = %v, want one refusal", res.Repairs)
	}
	if !strings.Contains(res.Repairs[0].Skipped, volB) {
		t.Errorf("skipped = %q, want the co-tenant volume named", res.Repairs[0].Skipped)
	}
	// The refusal must not be silent: the volume stays a path short, and that is
	// what the caller has to be able to report.
	if len(res.Defects) == 0 {
		t.Error("no defects reported for a volume left deliberately degraded")
	}
}

func TestAttach_DisruptiveRepairsWhenExplicitlyAllowed(t *testing.T) {
	k := newKernel(1, 2)
	k.serves = func(addr string, nsid nvme.NamespaceID) bool {
		return (addr == "10.0.0.1") == (nsid == 1)
	}

	res, err := repairer(k, WithDisruptiveRepairs(true)).
		Attach(context.Background(), targets("10.0.0.1", "10.0.0.2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairedKinds(res.Repairs)) == 0 {
		t.Errorf("repairs = %v, want the repair carried out once the caller allowed it", res.Repairs)
	}
}

// A repair that does not stick must degrade into periodic retries, not a
// disconnect/reconnect loop at monitor cadence. The controller id changes every
// time it is recreated, so the cooldown has to key on something that does not.
func TestAttach_CooldownSurvivesControllerRenumbering(t *testing.T) {
	k := newKernel(1)
	k.orphan["10.0.0.3"] = true
	k.sticky = true
	r := repairer(k)
	tgts := targets("10.0.0.1", "10.0.0.2", "10.0.0.3")

	first, err := r.Attach(context.Background(), tgts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(repairedKinds(first.Repairs)); n != 1 {
		t.Fatalf("first attach applied %d repairs, want exactly 1 despite the fault persisting", n)
	}

	second, err := r.Attach(context.Background(), tgts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(repairedKinds(second.Repairs)); n != 0 {
		t.Errorf("second attach applied %d repairs, want none inside the cooldown", n)
	}
	if len(second.Repairs) != 1 || !strings.Contains(second.Repairs[0].Skipped, "cooldown") {
		t.Errorf("repairs = %v, want a cooldown refusal", second.Repairs)
	}
	if len(k.deleted) != 1 {
		t.Errorf("deleted = %v, want a single teardown across both attaches", k.deleted)
	}
}

func TestAttach_RepairsAgainAfterTheCooldownExpires(t *testing.T) {
	k := newKernel(1)
	k.orphan["10.0.0.3"] = true
	k.sticky = true
	r := repairer(k)
	tgts := targets("10.0.0.1", "10.0.0.2", "10.0.0.3")

	if _, err := r.Attach(context.Background(), tgts, 1); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	r.now = func() time.Time { return base.Add(defaultRepairCooldown + time.Second) }

	res, err := r.Attach(context.Background(), tgts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(repairedKinds(res.Repairs)); n != 1 {
		t.Errorf("repairs = %v, want the repair retried once the cooldown expired", res.Repairs)
	}
}

// The cooldown governs repeats across attaches. Within one attach a repair is
// applied at most once whatever the cooldown says, so a caller that disables it
// gets a fresh attempt per call and not a loop inside one.
func TestAttach_CooldownCanBeDisabled(t *testing.T) {
	k := newKernel(1)
	k.orphan["10.0.0.3"] = true
	k.sticky = true
	r := repairer(k, WithRepairCooldown(0))
	tgts := targets("10.0.0.1", "10.0.0.2", "10.0.0.3")

	for i := 1; i <= 2; i++ {
		res, err := r.Attach(context.Background(), tgts, 1)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(repairedKinds(res.Repairs)); n != 1 {
			t.Fatalf("attach %d applied %d repairs, want exactly 1", i, n)
		}
	}
	if len(k.deleted) != 2 {
		t.Errorf("deleted = %v, want one teardown per attach with no cooldown in the way", k.deleted)
	}
}

// A caller that knows an endpoint is genuinely gone, a migration controller
// say, can ask for the repair Attach refuses to make on its own.
func TestAttach_AutoRepairKindsCanOptIntoStaleEndpoints(t *testing.T) {
	k := newKernel(1)
	for _, addr := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.9"} {
		k.attach(addr) // 10.0.0.9 is the node we have since migrated away from
	}

	res, err := repairer(k, WithAutoRepairKinds(DefectStaleEndpoint)).
		Attach(context.Background(), targets("10.0.0.1", "10.0.0.2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := repairedKinds(res.Repairs); !slices.Equal(got, []DefectKind{DefectStaleEndpoint}) {
		t.Fatalf("repairs = %v, want the stale endpoint released", res.Repairs)
	}
	if !slices.Equal(k.deleted, []nvme.ControllerID{"nvme2"}) {
		t.Errorf("deleted = %v, want only the unpublished endpoint's controller", k.deleted)
	}
}

// A cooldown key has to be narrow enough that two outstanding repairs do not
// collide, and stable enough to survive a repair that did not stick. The two
// halves pull in opposite directions, and each defect resolves them differently.
func TestKeyOf(t *testing.T) {
	ctrlDefect := func(id, addr string) Defect {
		return Defect{
			Kind: DefectControllerNotContributing, NQN: testNQN, Scope: ScopeController,
			Controllers: []nvme.Controller{ctrl(id, addr, "live")},
		}
	}
	subDefect := func(kind DefectKind, instance nvme.SubsystemID, ctrlID string) Defect {
		return Defect{
			Kind: kind, NQN: testNQN, Scope: ScopeSubsystem, Subsystem: instance,
			Controllers: []nvme.Controller{ctrl(ctrlID, "10.0.0.1", "live")},
		}
	}

	// A repair renames the controller, and the endpoint is what it cannot change.
	t.Run("controller scope follows the endpoint, not the name", func(t *testing.T) {
		if keyOf(ctrlDefect("nvme3", "10.0.0.3")) != keyOf(ctrlDefect("nvme7", "10.0.0.3")) {
			t.Error("a re-created controller at the same endpoint got a new key; the cooldown would never fire")
		}
		if keyOf(ctrlDefect("nvme3", "10.0.0.3")) == keyOf(ctrlDefect("nvme4", "10.0.0.4")) {
			t.Error("two endpoints share a key; three broken paths are three repairs, not one")
		}
	})

	// Only one of these can be outstanding per NQN, and the teardown itself may
	// change the instance id, so the id must not be part of the key.
	t.Run("subsystem scope ignores the instance", func(t *testing.T) {
		for _, kind := range []DefectKind{DefectNoNamespace, DefectNamespaceMissing} {
			if keyOf(subDefect(kind, "nvme-subsys0", "nvme0")) != keyOf(subDefect(kind, "nvme-subsys4", "nvme5")) {
				t.Errorf("%s keyed on the kernel instance; a reconnect that renumbers it "+
					"would lose the cooldown and reopen the teardown/reconnect loop", kind)
			}
		}
	})

	// The one defect Inspect can report several times for one NQN. Sharing a key
	// would let repairing the first mark the rest as handled, leaving stale heads
	// attached, and a lookup by NQN can then still return the wrong block device.
	t.Run("ambiguous heads stay distinct", func(t *testing.T) {
		first := subDefect(DefectAmbiguousHead, "nvme-subsys1", "nvme8")
		second := subDefect(DefectAmbiguousHead, "nvme-subsys2", "nvme9")
		if keyOf(first) == keyOf(second) {
			t.Fatal("two stale heads share a cooldown key; only one of them could ever be repaired")
		}
		// The same head after a repair that did not stick: its controller came
		// back under a new name, but the head is the same and must stay cooled.
		if keyOf(first) != keyOf(subDefect(DefectAmbiguousHead, "nvme-subsys1", "nvme12")) {
			t.Error("a renamed controller made the same stale head look new; it would be retried immediately")
		}
	})
}

// Inspect reports one defect per stale head, so an attach that finds two must
// clear both rather than let the first mask the second.
func TestAttach_RepairsEveryStaleHead(t *testing.T) {
	k := newKernel(1)
	r := repairer(k)

	stale := func(instance nvme.SubsystemID, ctrlID string) Defect {
		return Defect{
			Kind: DefectAmbiguousHead, NQN: testNQN, Scope: ScopeSubsystem, Subsystem: instance,
			Controllers: []nvme.Controller{ctrl(ctrlID, "10.0.0.9", "live")},
		}
	}
	first, second := stale("nvme-subsys1", "nvme8"), stale("nvme-subsys2", "nvme9")

	acted := map[repairKey]bool{}
	chosen, ok := r.choose([]Defect{first, second}, nvme.Device{}, false, acted)
	if !ok || chosen.Skipped != "" {
		t.Fatalf("choose = %+v, %v; want the first stale head", chosen, ok)
	}
	acted[keyOf(chosen.Defect)] = true
	r.mark(chosen.Defect)

	next, ok := r.choose([]Defect{first, second}, nvme.Device{}, false, acted)
	if !ok {
		t.Fatal("the second stale head was filtered out; repairing the first must not mask it")
	}
	if next.Skipped != "" {
		t.Fatalf("the second stale head was refused: %s", next.Skipped)
	}
	if next.Defect.Subsystem == chosen.Defect.Subsystem {
		t.Errorf("chose %s twice; the other head would be left attached", next.Defect.Subsystem)
	}
}

func TestAttach_RepairRoundsZeroDiagnosesWithoutActing(t *testing.T) {
	k := newKernel(1)
	k.pathless = true

	res, err := repairer(k, WithRepairRounds(0)).Attach(context.Background(), targets("10.0.0.1"), 1)
	if err == nil {
		t.Fatal("attach = nil, want an error: no device came up")
	}
	if len(k.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing torn down with repairs disabled", k.deleted)
	}
	if got := kinds(res.Defects); !slices.Contains(got, DefectNoNamespace) {
		t.Errorf("defects = %v, want the diagnosis reported even without a repair", got)
	}
	// The diagnosis has to reach the error too: "no device turned up" alone is
	// what made the original incidents unreadable.
	if !strings.Contains(err.Error(), string(DefectNoNamespace)) {
		t.Errorf("err = %v, want the diagnosis named", err)
	}
}

func TestAttach_ScopeCapRefusesASubsystemRepair(t *testing.T) {
	k := newKernel(1)
	k.pathless = true

	res, err := repairer(k, WithMaxRepairScope(ScopeController)).
		Attach(context.Background(), targets("10.0.0.1"), 1)
	if err == nil {
		t.Fatal("attach = nil, want an error")
	}
	if len(k.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing torn down above the scope cap", k.deleted)
	}
	if len(res.Repairs) != 1 || !strings.Contains(res.Repairs[0].Skipped, "capped") {
		t.Errorf("repairs = %v, want a scope-cap refusal", res.Repairs)
	}
}

// A missing namespace is the target not publishing it. Reconnecting cannot
// change that, and every namespace that IS there belongs to another volume.
func TestAttach_NeverRepairsAMissingNamespace(t *testing.T) {
	k := newKernel(1, 2)

	res, err := repairer(k).Attach(context.Background(), targets("10.0.0.1"), 3)
	if err == nil {
		t.Fatal("attach = nil, want an error: namespace 3 does not exist")
	}
	if len(k.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing torn down", k.deleted)
	}
	if got := kinds(res.Defects); !slices.Contains(got, DefectNamespaceMissing) {
		t.Errorf("defects = %v, want namespace-missing reported", got)
	}
	if len(repairedKinds(res.Repairs)) != 0 {
		t.Errorf("repairs = %v, want none", res.Repairs)
	}
}

func TestAttach_ConnectFailurePropagates(t *testing.T) {
	k := newKernel(1)
	k.connectErr = map[string]error{"10.0.0.1": errors.New("connection refused")}

	_, err := repairer(k).Attach(context.Background(), targets("10.0.0.1"), 1)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want the connect failure", err)
	}
}

func TestAttach_DegradedWhenAPathIsUnreachable(t *testing.T) {
	k := newKernel(1)
	k.connectErr = map[string]error{"10.0.0.2": errors.New("connection refused")}

	res, err := repairer(k).Attach(context.Background(), targets("10.0.0.1", "10.0.0.2"), 1)
	if err != nil {
		t.Fatalf("attach = %v, want success: one live path serves the volume", err)
	}
	if !res.Degraded() {
		t.Errorf("paths = %v, want degraded", res.Paths)
	}
}

func TestAttach_RejectsNoTargets(t *testing.T) {
	if _, err := repairer(newKernel(1)).Attach(context.Background(), nil, 1); err == nil {
		t.Error("err = nil, want an error for an empty target list")
	}
}

func TestAttach_CallerDeadlineIsNotDiagnosedAsADefect(t *testing.T) {
	k := newKernel(1)
	k.pathless = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := repairer(k).Attach(ctx, targets("10.0.0.1"), 1)
	if err == nil {
		t.Fatal("err = nil, want the context error")
	}
	if len(res.Defects) != 0 || len(k.deleted) != 0 {
		t.Errorf("defects = %v deleted = %v, want the fabric untouched: what ran out was time",
			res.Defects, k.deleted)
	}
}

func TestRepair_RefusesADefectWithNoRemedy(t *testing.T) {
	c := &recordingConnector{}
	err := Repair(context.Background(), c, Defect{Kind: DefectAmbiguousHead, NQN: testNQN, Scope: ScopeNone})
	if !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("err = %v, want errs.ErrUnsupported", err)
	}
	if len(c.controllers) != 0 {
		t.Errorf("tore down %v, want nothing", c.controllers)
	}
}

// Repair applies no policy at all, which is Repairer's job, and tears down in
// the order Inspect put the controllers in, so the optimized path goes last.
func TestRepair_TearsDownInTheGivenOrder(t *testing.T) {
	c := &recordingConnector{}
	d := Defect{
		Kind:  DefectNoNamespace,
		NQN:   testNQN,
		Scope: ScopeSubsystem,
		Controllers: []nvme.Controller{
			liveCtrl("nvme2", "10.0.0.3"),
			liveCtrl("nvme1", "10.0.0.2"),
			liveCtrl("nvme0", "10.0.0.1"),
		},
		CoTenants: []nvme.Namespace{{ID: 2, UUID: volB}}, // policy would refuse, Repair does not
	}
	if err := Repair(context.Background(), c, d); err != nil {
		t.Fatal(err)
	}
	want := []nvme.ControllerID{"nvme2", "nvme1", "nvme0"}
	if !slices.Equal(c.controllers, want) {
		t.Errorf("teardown = %v, want %v", c.controllers, want)
	}
}

// A controller that fails to release must not stop the ones behind it: the
// optimized path still has to be torn down last rather than left behind.
func TestRepair_ContinuesPastAFailedTeardown(t *testing.T) {
	boom := errors.New("device busy")
	c := &failingConnector{failFor: "nvme1", err: boom}
	d := Defect{
		Kind:  DefectNoNamespace,
		NQN:   testNQN,
		Scope: ScopeSubsystem,
		Controllers: []nvme.Controller{
			liveCtrl("nvme1", "10.0.0.2"),
			liveCtrl("nvme0", "10.0.0.1"),
		},
	}
	err := Repair(context.Background(), c, d)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the teardown failure reported", err)
	}
	if !slices.Contains(c.controllers, "nvme0") {
		t.Errorf("torn down = %v, want nvme0 released despite nvme1 failing", c.controllers)
	}
}

type failingConnector struct {
	recordingConnector
	failFor nvme.ControllerID
	err     error
}

func (c *failingConnector) DisconnectController(ctx context.Context, ctrl nvme.Controller) error {
	if ctrl.ID == c.failFor {
		return c.err
	}
	return c.recordingConnector.DisconnectController(ctx, ctrl)
}

func TestOwnDeviceAtRisk(t *testing.T) {
	ours := namespaceOn(1, volA, "nvme0", "nvme1")
	dev := nvme.Device{Namespace: ours, Subsystem: subsystem(
		[]nvme.Controller{liveCtrl("nvme0", "10.0.0.1"), liveCtrl("nvme1", "10.0.0.2")}, ours)}

	defect := func(scope Scope, sub nvme.SubsystemID, ctrls ...string) Defect {
		d := Defect{Kind: DefectControllerNotContributing, Scope: scope, Subsystem: sub}
		for _, id := range ctrls {
			d.Controllers = append(d.Controllers, liveCtrl(id, "10.0.0.9"))
		}
		return d
	}

	t.Run("one of two paths is safe", func(t *testing.T) {
		if ownDeviceAtRisk(defect(ScopeController, "nvme-subsys0", "nvme1"), dev) {
			t.Error("at risk, want safe: nvme0 still serves the device")
		}
	})
	t.Run("every path is not", func(t *testing.T) {
		if !ownDeviceAtRisk(defect(ScopeSubsystem, "nvme-subsys0", "nvme0", "nvme1"), dev) {
			t.Error("safe, want at risk: the teardown takes both paths")
		}
	})
	t.Run("another subsystem instance cannot touch it", func(t *testing.T) {
		if ownDeviceAtRisk(defect(ScopeSubsystem, "nvme-subsys1", "nvme0", "nvme1"), dev) {
			t.Error("at risk, want safe: a stale instance's controllers are not ours")
		}
	})
	t.Run("a device that serves no I/O has nothing to lose", func(t *testing.T) {
		stale := dev
		stale.Namespace.Paths = []nvme.Path{{Controller: "nvme0", NSID: 1, ANAState: nvme.ANAInaccessible}}
		if ownDeviceAtRisk(defect(ScopeSubsystem, "nvme-subsys0", "nvme0"), stale) {
			t.Error("at risk, want safe: tearing down an unusable head is the repair")
		}
	})
}
