package volumemigration

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
)

// Released names one controller that was disconnected, for the Job log and the
// operator's post-mortem. The address is what an operator recognises a path by; the
// controller ID is what the kernel logged it under.
type Released struct {
	Controller string // "nvme7"
	Address    string // "192.168.10.112:4428"
	Reason     string // why it was disconnected
}

func (r Released) String() string {
	return fmt.Sprintf("%s at %s (%s)", r.Controller, r.Address, r.Reason)
}

// reapableKind is the one defect this package tears controllers down for: a live
// controller that serves no path to a namespace of the subsystem.
//
// It is deliberately the narrowest of the kinds atlas is willing to repair unattended,
// because a repair here is not followed by a connect. Attach repairs this defect to make
// the kernel re-enumerate — tear the controller down, reconnect, get a fresh namespace
// scan — and Repair is explicit that the reconnect is the caller's half. This caller has
// no such half: it runs in front of a validation that must not touch paths it did not
// establish. So a teardown here is permanent until the CSI driver notices, and the
// selection has to be safe on that basis rather than on Attach's.
//
// That is what the extra test in reapableDefects is for. Atlas reports this defect for a
// controller that serves some of the subsystem's namespaces and not the one asked about,
// and repairing that is right when a reconnect follows and wrong when none does: the
// controller is a working path for another volume on the same subsystem, and reaping it
// without reconnecting just removes that path. Only a controller serving nothing at all
// is taken.
//
// Inspect's other kinds stay out. DefectNamespaceMissing and DefectStaleEndpoint are the
// two atlas itself refuses to repair unattended — the target simply is not publishing,
// and a node in restart looks exactly like an endpoint that is gone. DefectAmbiguousHead
// and DefectNoNamespace are ScopeSubsystem: they repair by tearing down every controller
// of a kernel subsystem instance, which would drop this host's connection to the
// subsystem altogether — and "this node consumes this subsystem" is the signal the
// validation Job gates on, with nothing here to reconnect it.
const reapableKind = nvmeof.DefectControllerNotContributing

// controllerAddress renders a controller's endpoint the way an expected path names it.
func controllerAddress(c nvme.Controller) string {
	return c.Address.TrAddr + ":" + c.Address.TrSvcID
}

// servedPaths groups the subsystem's namespace paths by the controller serving them.
// A controller absent from the result carries no namespace at all.
func servedPaths(s nvme.Subsystem) map[nvme.ControllerID][]nvme.Path {
	byController := map[nvme.ControllerID][]nvme.Path{}
	for _, ns := range s.Namespaces {
		for _, p := range ns.Paths {
			byController[p.Controller] = append(byController[p.Controller], p)
		}
	}
	return byController
}

// carriesIO reports whether the kernel can route I/O to any namespace over this
// controller. It is the one question that decides whether a target path may be
// released: everything else about a path is recoverable, and an in-flight request on it
// is not.
func carriesIO(paths []nvme.Path) bool {
	for _, p := range paths {
		if p.ANAState.Accessible() {
			return true
		}
	}
	return false
}

// migrationPathVictims selects the migration's own target paths that carry nothing.
// See ReleaseMigrationPaths for why "carries nothing" is the safe test, and for why
// nothing is selected when the whole subsystem carries nothing.
func migrationPathVictims(s nvme.Subsystem, conns []Connection) []nvme.Controller {
	targets := make(map[string]bool, len(conns))
	for _, c := range conns {
		targets[c.Address()] = true
	}

	served := servedPaths(s)

	// A subsystem with no accessible path anywhere is the cutover window, not a leak.
	// The control plane drives every path of the subsystem inaccessible for a couple of
	// seconds while it moves the volume, and in that window "carries no I/O" is true of
	// every controller on it — including the one that is about to become the data path.
	// Releasing then would tear down the live path, which is the outage this is meant to
	// prevent, so the state is declined rather than acted on.
	//
	// What makes this safe to decline is that it costs nothing but a round: a leak is
	// recognisable precisely because some path is serving while the migration's are not,
	// so a real one is still there to release on the next attempt, and the husk it leaves
	// is ReapDeadControllers' to clear — that pass reads namespace legs rather than ANA
	// states and is unaffected by the window.
	if !slices.ContainsFunc(s.Controllers, func(c nvme.Controller) bool {
		return carriesIO(served[c.ID])
	}) {
		return nil
	}

	var victims []nvme.Controller
	for _, ctrl := range s.Controllers {
		if !targets[controllerAddress(ctrl)] || carriesIO(served[ctrl.ID]) {
			continue
		}
		victims = append(victims, ctrl)
	}
	return victims
}

// detach tears down every controller given, and reports what went. One that fails to
// release does not stop the ones behind it: a partial cleanup is strictly better than
// none, and every caller is already on a failure path.
func detach(
	ctx context.Context,
	d nvmeof.ControllerDetacher,
	victims []nvme.Controller,
	reason string,
) ([]Released, error) {
	var (
		done     []Released
		firstErr error
	)
	for _, ctrl := range victims {
		if err := d.DisconnectController(ctx, ctrl); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("disconnect %s at %s: %w", ctrl.ID, controllerAddress(ctrl), err)
			}
			continue
		}
		done = append(done, Released{
			Controller: string(ctrl.ID),
			Address:    controllerAddress(ctrl),
			Reason:     reason,
		})
	}
	return done, firstErr
}

// ReleaseMigrationPaths disconnects the target paths a validation run established on
// this host, and returns the controllers it took down.
//
// It is the counterpart EnsureMigrationPaths did not have. A migration that never cuts
// over leaves its target paths connected on every consumer node, and they do not go
// away promptly on their own: the kernel resets its reconnect counter whenever a
// reconnect gets far enough, so a controller whose target has stopped answering for it
// can outlive its ctrl_loss_tmo. Every abandoned attempt adds another, and once one of
// them is live without serving a namespace, the pre-cutover check in
// VerifyMigrationPaths reports it forever — so the leak does not merely accumulate, it
// stops the next migration of that subsystem from ever passing.
//
// Only pre-cutover. After cutover the target paths are the volume's data path, and
// releasing them then is the outage this whole package exists to avoid. The caller must
// know the migration did not continue.
//
// A controller is released only when it can carry no I/O — no namespace of the
// subsystem is accessible over it. That is what makes this safe without a record of
// which paths the run created: a target path held pre-cutover is required to be
// inaccessible on every namespace (VerifyMigrationPaths rejects a run where it is not),
// while a path that was already there for another reason is a live HA path and is
// serving. The one case the rule gives up on is an HA path at a migration target's
// address whose own node is down, and therefore inaccessible, at the moment of release:
// it is disconnected, and the CSI driver reconnects it. Losing a path that was already
// carrying nothing is the cheaper mistake — the alternative is tearing down a path with
// I/O on it.
//
// The rule has one blind spot, and it is handled rather than accepted: during cutover the
// control plane drives every path of the subsystem inaccessible for about two seconds, and
// in that window every controller looks like it carries nothing. Nothing is released while
// no path on the subsystem is accessible — see migrationPathVictims.
//
// This is not expressed as an Inspect defect because it is not one: a parked target
// path is exactly what a correct pre-cutover fabric looks like, and Inspect is right
// not to flag it. What makes these releasable is knowing the migration was abandoned,
// which is caller knowledge no diagnosis has.
func ReleaseMigrationPaths(
	ctx context.Context,
	sysRoot, nqn string,
	conns []Connection,
) ([]Released, error) {
	return releaseMigrationPaths(ctx, sysRoot, nqn, conns, connector(sysRoot))
}

func releaseMigrationPaths(
	ctx context.Context,
	sysRoot, nqn string,
	conns []Connection,
	d nvmeof.ControllerDetacher,
) ([]Released, error) {
	if nqn == "" {
		return nil, fmt.Errorf("release migration paths: empty subsystem NQN")
	}
	s, err := subsystem(ctx, sysRoot, nqn)
	if err != nil {
		return nil, err
	}
	if s.NQN == "" {
		// Nothing attached for this NQN: the paths are already gone.
		return nil, nil
	}
	return detach(ctx, d, migrationPathVictims(s, conns),
		"migration target path, serving no accessible namespace")
}

// ReapDeadControllers tears down the controllers of nqn that Inspect diagnoses as
// carrying no path to a namespace, and returns what it took down.
//
// This is the state a leaked migration path settles into, and the reason it is worth a
// pass of its own is that it is self-perpetuating: Inspect reports such a controller,
// VerifyMigrationPaths turns that into a failure, and the failure aborts the very
// migration whose release would have cleaned it up. One leak — from an operator
// restart, a killed Job, a node that rebooted mid-validation — then blocks every later
// migration of the subsystem indefinitely. Reaping first is what bounds the damage of a
// missed release to a single attempt.
//
// The diagnosis is atlas's, not this package's, and so is the decision about blast
// radius: a defect whose repair would take the last usable path from another volume on
// the subsystem is left alone and reported, never repaired. That check is the reason to
// go through Inspect rather than pattern-match sysfs here — these are shared,
// multi-namespace subsystems, so "this controller serves nothing of mine" and "tearing
// it down harms nobody" are genuinely different questions.
//
// Only ScopeController defects are acted on. A wider repair means taking a whole kernel
// subsystem instance down, which is not something a pre-flight check should do on its
// own initiative.
//
// A controller stuck in "connecting" is not diagnosed as any of this, and is therefore
// left alone even though the leak produces those too. From a snapshot it is
// indistinguishable from an HA path in a normal reconnect, and racing the kernel's own
// recovery to remove a path that may be about to come back is not a trade worth making:
// those are for ReleaseMigrationPaths to prevent and for ctrl_loss_tmo to expire.
func ReapDeadControllers(ctx context.Context, sysRoot, nqn string) ([]Released, error) {
	return reapDeadControllers(ctx, sysRoot, nqn, connector(sysRoot))
}

func reapDeadControllers(
	ctx context.Context,
	sysRoot, nqn string,
	d nvmeof.ControllerDetacher,
) ([]Released, error) {
	if nqn == "" {
		return nil, fmt.Errorf("reap dead controllers: empty subsystem NQN")
	}
	s, err := subsystem(ctx, sysRoot, nqn)
	if err != nil {
		return nil, err
	}
	if s.NQN == "" {
		return nil, nil
	}

	var (
		released []Released
		firstErr error
	)
	seen := map[nvme.ControllerID]bool{}
	for _, def := range reapableDefects(ctx, sysRoot, s) {
		// A repair re-diagnosed across selectors names the same controller more than
		// once — one defect per namespace it fails to serve — and tearing it down twice
		// would report a second release for a controller that is already gone.
		fresh := make([]nvme.Controller, 0, len(def.Controllers))
		for _, ctrl := range def.Controllers {
			if !seen[ctrl.ID] {
				seen[ctrl.ID] = true
				fresh = append(fresh, ctrl)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		done, err := detach(ctx, d, fresh, string(def.Kind))
		released = append(released, done...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return released, firstErr
}

// reapableDefects asks Inspect about the subsystem and returns the defects that may be
// repaired here.
//
// Deriving this from Inspect rather than reading sysfs directly is the point of the
// exercise: what makes the leak self-perpetuating is that VerifyMigrationPaths rejects a
// migration over Inspect's verdict, so a reaper working from its own idea of a dead
// controller could clear something Inspect still complains about and leave the migration
// blocked anyway. Asking the same question the gate asks makes "the reap clears what
// validation rejects" true by construction rather than by agreement.
//
// A defect qualifies when it is the reapable kind, when repairing it tears down single
// controllers rather than a whole subsystem instance, when no other volume on the
// subsystem loses its last usable path to it — atlas's own blast-radius answer, which is
// the other thing worth reusing here — and when the controller serves no namespace at
// all. See reapableKind for why that last test is this caller's and not atlas's.
//
// Inspect is asked once per exported namespace as well as once for the bare NQN, because
// its controller-level check needs to know which namespace is meant and stands down when
// a selector matches several — which a bare NQN does on exactly the multi-namespace
// subsystems this package migrates. That is the same reason diagnose asks that way.
func reapableDefects(ctx context.Context, sysRoot string, s nvme.Subsystem) []nvmeof.Defect {
	subs := snapshot{s}
	devs := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{SysRoot: sysRoot})
	served := servedPaths(s)

	selectors := make([]nvme.DeviceSelector, 0, len(s.Namespaces)+1)
	selectors = append(selectors, nvme.DeviceSelector{NQN: s.NQN})
	for _, ns := range s.Namespaces {
		selectors = append(selectors, nvme.DeviceSelector{NQN: s.NQN, NSID: ns.ID})
	}

	var out []nvmeof.Defect
	for i, sel := range selectors {
		// Only the first selector carries the device resolver: the duplicate-head check
		// is per-subsystem, and repeating it per namespace would report one stale head
		// once per namespace. It is not a reapable kind anyway, so this only avoids
		// wasted sysfs walks.
		var d nvme.DeviceResolver
		if i == 0 {
			d = devs
		}
		defects, err := nvmeof.Inspect(ctx, subs, d, sel, nil)
		if err != nil {
			// A diagnosis that cannot be made is not a reason to tear anything down.
			continue
		}
		for _, def := range defects {
			if def.Kind != reapableKind || def.Scope != nvmeof.ScopeController || len(def.CoTenants) > 0 {
				continue
			}
			if slices.ContainsFunc(def.Controllers, func(c nvme.Controller) bool {
				return len(served[c.ID]) > 0
			}) {
				continue
			}
			out = append(out, def)
		}
	}
	return out
}

// FormatReleased renders released controllers as one log-ready line, sorted so the same
// cleanup reads the same way twice.
func FormatReleased(rs []Released) string {
	if len(rs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}
