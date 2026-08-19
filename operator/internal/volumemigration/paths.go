package volumemigration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
)

// PathState is what the host reports about one target path of a subsystem.
type PathState struct {
	Address string // "<ip>:<port>"
	// Present is false when no controller for this address exists at all.
	Present bool
	// State is the controller's kernel state: "live", "connecting", "resetting", ...
	// Only a live controller can carry I/O.
	State string
	// ANAStates are the ANA states of this controller's paths to the subsystem's
	// namespaces. Pre-cutover every one of them must be inaccessible: the path is
	// established but the target is not serving yet.
	ANAStates []string
	// PreExisting records that this address already had a controller before we
	// connected. A pre-existing path proves nothing about our connect, and on an HA
	// cluster the migration target may well already be a listener for the subsystem.
	PreExisting bool
}

// Accessible reports whether any of the path's namespaces is in a state the kernel
// will route I/O to.
func (p PathState) Accessible() bool {
	for _, s := range p.ANAStates {
		if nvme.ANAState(s).Accessible() {
			return true
		}
	}
	return false
}

func (p PathState) String() string {
	origin := "new"
	if p.PreExisting {
		origin = "pre-existing"
	}
	if !p.Present {
		return fmt.Sprintf("%s: absent", p.Address)
	}
	return fmt.Sprintf("%s: %s state=%s ana=%s",
		p.Address, origin, p.State, strings.Join(p.ANAStates, ","))
}

// resolver reads the host's NVMe subsystems under sysRoot (empty for /sys).
func resolver(sysRoot string) *nvme.SysfsSubsystemResolver {
	return nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{SysRoot: sysRoot})
}

// subsystem returns the attached subsystem for nqn. A subsystem that is not
// attached at all comes back as the zero value rather than an error: every caller
// here has something to say about that state, and none of them can say it from an
// error.
func subsystem(ctx context.Context, sysRoot, nqn string) (nvme.Subsystem, error) {
	s, err := resolver(sysRoot).ByNQN(ctx, nqn)
	if errors.Is(err, errs.ErrNotFound) {
		return nvme.Subsystem{}, nil
	}
	if err != nil {
		return nvme.Subsystem{}, fmt.Errorf("read host NVMe subsystem %s: %w", nqn, err)
	}
	return s, nil
}

// snapshot answers every lookup from one already-read subsystem. Inspect is asked
// once per exported namespace (see diagnose), and each call would otherwise walk
// sysfs again — on a subsystem shared by many namespaces, which is the case this
// whole package exists for, that is the same tree read once per volume on it.
type snapshot struct{ s nvme.Subsystem }

func (r snapshot) List(context.Context) ([]nvme.Subsystem, error) {
	return []nvme.Subsystem{r.s}, nil
}

func (r snapshot) ByNQN(_ context.Context, nqn string) (nvme.Subsystem, error) {
	if r.s.NQN != nqn {
		return nvme.Subsystem{}, errs.ErrNotFound
	}
	return r.s, nil
}

// inspectPaths reports the state of each expected target address, as the host sees it.
// preExisting marks the addresses that already had a controller before connecting.
func inspectPaths(s nvme.Subsystem, conns []Connection, preExisting map[string]bool) []PathState {
	// Controllers of this subsystem, by address, with the ANA states of their paths.
	states := map[string]*PathState{}
	anaByController := map[nvme.ControllerID][]string{}
	for _, ns := range s.Namespaces {
		for _, p := range ns.Paths {
			anaByController[p.Controller] = append(anaByController[p.Controller], string(p.ANAState))
		}
	}
	for _, c := range s.Controllers {
		addr := c.Address.TrAddr + ":" + c.Address.TrSvcID
		ana := anaByController[c.ID]
		sort.Strings(ana)
		// Several controllers can front one address (HA re-connects); keep the
		// healthiest view rather than whichever came last.
		if prev, ok := states[addr]; ok && prev.State == "live" {
			prev.ANAStates = append(prev.ANAStates, ana...)
			continue
		}
		states[addr] = &PathState{
			Address: addr, Present: true, State: c.State, ANAStates: ana,
		}
	}

	out := make([]PathState, 0, len(conns))
	for _, c := range conns {
		addr := c.Address()
		ps := PathState{Address: addr, PreExisting: preExisting[addr]}
		if found, ok := states[addr]; ok {
			ps.Present, ps.State, ps.ANAStates = true, found.State, found.ANAStates
		}
		out = append(out, ps)
	}
	return out
}

// PresentAddresses returns the addresses of the subsystem's existing controllers, used
// to tell a path we created from one that was already there.
func PresentAddresses(ctx context.Context, sysRoot, nqn string) (map[string]bool, error) {
	s, err := subsystem(ctx, sysRoot, nqn)
	if err != nil {
		return nil, err
	}
	addrs := map[string]bool{}
	for _, c := range s.Controllers {
		addrs[c.Address.TrAddr+":"+c.Address.TrSvcID] = true
	}
	return addrs, nil
}

// diagnose runs atlas's NVMe-oF inspection over the subsystem and returns what it
// found wrong, rendered for the migration's error.
//
// Inspect is what names the defects a connect cannot see — a live controller that
// serves no namespace at all, or serves the subsystem's other namespaces but not
// this one, or an NQN answered by two kernel subsystem instances at once. Those are
// the states in which every expected path looks established while the volume has
// nothing to take over at cutover, and diagnosing them from what the kernel already
// publishes is what atlas centralises.
//
// It is asked once per exported namespace rather than once for the subsystem, and
// that is the whole reason it can say anything here: its controller-level check
// needs to know which namespace is meant, and stands down when a selector matches
// several — which a bare NQN does on exactly the multi-namespace subsystems this
// package migrates.
//
// No target list is passed. Targets are how Inspect tells an attached endpoint the
// control plane no longer publishes from one it wants, and pre-cutover that verdict
// is inverted here: the source paths are absent from the migration's target list and
// must stay attached, so comparing against it would condemn every path the volume is
// currently served over.
func diagnose(ctx context.Context, sysRoot string, s nvme.Subsystem) []string {
	if s.NQN == "" {
		return nil
	}
	subs := snapshot{s}
	// The duplicate-head check needs a view wider than one subsystem — the point of
	// it is that a second instance exists — so it reads sysfs directly, and only
	// once, on the selector that names no namespace.
	devs := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{SysRoot: sysRoot})

	selectors := make([]nvme.DeviceSelector, 0, len(s.Namespaces)+1)
	selectors = append(selectors, nvme.DeviceSelector{NQN: s.NQN})
	for _, ns := range s.Namespaces {
		selectors = append(selectors, nvme.DeviceSelector{NQN: s.NQN, NSID: ns.ID})
	}

	var problems []string
	seen := map[string]bool{}
	for i, sel := range selectors {
		// Only the first selector carries the device resolver; repeating the
		// duplicate-head check per namespace would report one stale head once per
		// namespace on the subsystem.
		var d nvme.DeviceResolver
		if i == 0 {
			d = devs
		}
		defects, err := nvmeof.Inspect(ctx, subs, d, sel, nil)
		if err != nil {
			problems = append(problems, fmt.Sprintf("inspection failed: %v", err))
			continue
		}
		for _, def := range defects {
			if msg := def.Detail; msg != "" && !seen[msg] {
				seen[msg] = true
				problems = append(problems, fmt.Sprintf("%s: %s", def.Kind, msg))
			}
		}
	}
	return problems
}

// VerifyMigrationPaths checks that every expected target path is established on this
// host and parked, i.e. ready to take over at cutover but not serving yet.
//
// Each path must be:
//
//   - present — a controller for that address exists. A path that our connect failed to
//     create is the case that used to slip through: the target refuses the connect
//     (typically because the subsystem does not exist there yet) while an unrelated
//     controller satisfies a laxer check.
//   - live — a controller stuck in "connecting" carries no I/O, so it is no proof that
//     the host can reach the target.
//   - inaccessible on every namespace path — the target must not be serving before
//     cutover. An accessible path here means reads can already land on a target that
//     may not hold the data yet.
//
// Presence and liveness are read back from the host rather than taken from what the
// connect reported, and deliberately so: a connect that returns zero is no evidence
// the path exists, since the kernel may still be retrying an admin queue the target
// refuses. Whether the established paths can actually carry a namespace is atlas's
// question, asked through diagnose.
//
// A path that existed before we connected is reported but not rejected: on an HA
// cluster the migration target can already be a listener for the subsystem. What is
// rejected is a run where *no* expected path is ours and none is new — that means the
// connect did nothing and the check would be vacuous.
func VerifyMigrationPaths(
	ctx context.Context,
	sysRoot, nqn string,
	conns []Connection,
	preExisting map[string]bool,
) ([]PathState, error) {
	if nqn == "" {
		return nil, fmt.Errorf("verify migration paths: empty subsystem NQN")
	}
	s, err := subsystem(ctx, sysRoot, nqn)
	if err != nil {
		return nil, err
	}
	paths := inspectPaths(s, conns, preExisting)

	var problems []string
	for _, p := range paths {
		switch {
		case !p.Present:
			problems = append(problems, fmt.Sprintf(
				"%s: no controller for this address — the connect did not take effect", p.Address))
		case p.State != "live":
			problems = append(problems, fmt.Sprintf(
				"%s: controller state %q, want live — the path cannot carry I/O", p.Address, p.State))
		case p.Accessible():
			problems = append(problems, fmt.Sprintf(
				"%s: ANA state %s is accessible before cutover — reads may land on the target already",
				p.Address, strings.Join(p.ANAStates, ",")))
		}
	}
	problems = append(problems, diagnose(ctx, sysRoot, s)...)

	if len(problems) > 0 {
		return paths, fmt.Errorf("migration paths not ready: %s", strings.Join(problems, "; "))
	}
	return paths, nil
}
