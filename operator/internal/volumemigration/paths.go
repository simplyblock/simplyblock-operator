package volumemigration

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/simplyblock/atlas/nvme"
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

// inspectPaths reports the state of each expected target address, as the host sees it.
// preExisting marks the addresses that already had a controller before connecting.
func inspectPaths(
	ctx context.Context,
	sysRoot, nqn string,
	conns []Connection,
	preExisting map[string]bool,
) ([]PathState, error) {
	subs, err := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{SysRoot: sysRoot}).List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host NVMe subsystems: %w", err)
	}

	// Controllers of this subsystem, by address, with the ANA states of their paths.
	states := map[string]*PathState{}
	for _, s := range subs {
		if s.NQN != nqn {
			continue
		}
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
	}

	out := make([]PathState, 0, len(conns))
	for _, c := range conns {
		addr := fmt.Sprintf("%s:%d", c.IP, c.Port)
		ps := PathState{Address: addr, PreExisting: preExisting[addr]}
		if found, ok := states[addr]; ok {
			ps.Present, ps.State, ps.ANAStates = true, found.State, found.ANAStates
		}
		out = append(out, ps)
	}
	return out, nil
}

// PresentAddresses returns the addresses of the subsystem's existing controllers, used
// to tell a path we created from one that was already there.
func PresentAddresses(ctx context.Context, sysRoot, nqn string) (map[string]bool, error) {
	subs, err := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{SysRoot: sysRoot}).List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host NVMe subsystems: %w", err)
	}
	addrs := map[string]bool{}
	for _, s := range subs {
		if s.NQN != nqn {
			continue
		}
		for _, c := range s.Controllers {
			addrs[c.Address.TrAddr+":"+c.Address.TrSvcID] = true
		}
	}
	return addrs, nil
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
	paths, err := inspectPaths(ctx, sysRoot, nqn, conns, preExisting)
	if err != nil {
		return nil, err
	}

	var problems []string
	for _, p := range paths {
		switch {
		case !p.Present:
			problems = append(problems, fmt.Sprintf(
				"%s: no controller for this address — the connect did not take effect", p.Address))
		case p.State != "live":
			problems = append(problems, fmt.Sprintf(
				"%s: controller state %q, want live — the path cannot carry I/O", p.Address, p.State))
		case len(p.ANAStates) == 0:
			problems = append(problems, fmt.Sprintf(
				"%s: controller has no namespace path — nothing to take over at cutover", p.Address))
		case p.Accessible():
			problems = append(problems, fmt.Sprintf(
				"%s: ANA state %s is accessible before cutover — reads may land on the target already",
				p.Address, strings.Join(p.ANAStates, ",")))
		}
	}
	if len(problems) > 0 {
		return paths, fmt.Errorf("migration paths not ready: %s", strings.Join(problems, "; "))
	}
	return paths, nil
}
