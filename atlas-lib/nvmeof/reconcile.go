package nvmeof

import (
	"context"
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
)

// PathState is what a node-side guardian needs to know about one volume's
// fabric after a reconcile: how many paths there should be, how many there are,
// what happened to each, and which attached paths the control plane no longer
// publishes.
type PathState struct {
	// NQN is the subsystem the paths front.
	NQN string
	// Expected is how many paths the control plane published.
	Expected int
	// Live is how many are live after the reconcile.
	Live int
	// Results is the per-path outcome, in priority order (primary first).
	Results []PathResult
	// Stale are attached controllers whose endpoint is not in the control
	// plane's current answer — typically the old primary after a migration.
	// They are reported, never removed; see ReconcilePaths.
	Stale []nvme.Controller
}

// Complete reports whether every published path is live.
func (s PathState) Complete() bool { return s.Expected > 0 && s.Live >= s.Expected }

// Degraded reports whether the volume is usable but short of paths — I/O still
// flows, with less redundancy than the control plane published.
func (s PathState) Degraded() bool { return s.Live > 0 && s.Live < s.Expected }

// Down reports whether no path is live, i.e., the volume cannot serve I/O.
func (s PathState) Down() bool { return s.Live == 0 }

// ReconcilePaths makes the attached fabric paths of a volume match the control
// plane's current answer, and reports the resulting state.
//
// This is the loop a node-side guardian runs: the control plane's Connection is
// the desired state, the kernel's controllers are the actual state, and the two
// drift — a path drops when its storage node restarts, and the published set
// itself changes after a migration or a node replacement. Reconciling means
// establishing the published paths that are not up (in priority order, skipping
// the ones that cannot be reached) and reporting what is left over.
//
// It is safe to call on every tick: connecting is idempotent per path, so a
// volume already attached over all its paths costs one controller-state lookup
// per path and changes nothing.
//
// Stale paths are reported, not removed. A path missing from the control plane's
// answer right now is not necessarily gone for good — a node in restart is the
// obvious case — and tearing down a controller is a change to a live data path,
// so that decision stays with the caller. Pass subs as nil to skip the check.
//
// The error is non-nil only when no path could be established at all: a volume
// with one live path is attached and usable, and everything else is in the
// returned PathState.
func ReconcilePaths(
	ctx context.Context,
	c Connector,
	subs nvme.SubsystemResolver,
	conn lvol.Connection,
	opts ...TargetOption,
) (PathState, error) {
	if conn.NQN == "" {
		return PathState{}, fmt.Errorf("reconcile: connection has no NQN: %w", errs.ErrUnsupported)
	}
	targets := Targets(conn, opts...)
	if len(targets) == 0 {
		return PathState{NQN: conn.NQN}, fmt.Errorf("reconcile %s: control plane published no endpoints: %w",
			conn.NQN, errs.ErrNotConnected)
	}

	state := PathState{NQN: conn.NQN, Expected: len(targets)}
	results, err := c.ConnectPaths(ctx, targets)
	state.Results = results
	for _, r := range results {
		if r.Live {
			state.Live++
		}
	}
	if err != nil {
		// Nothing is up; which attached paths are stale is noise next to that.
		return state, err
	}

	if subs != nil {
		stale, err := stalePaths(ctx, subs, targets)
		if err != nil {
			return state, fmt.Errorf("reconcile %s: %w", conn.NQN, err)
		}
		state.Stale = stale
	}
	return state, nil
}

// stalePaths returns the attached controllers of the subsystem whose endpoint
// matches none of the desired targets. Controllers whose address the kernel does
// not report (PCIe) are skipped: they describe no fabric endpoint, so they
// cannot be compared against one.
func stalePaths(ctx context.Context, subs nvme.SubsystemResolver, targets []Target) ([]nvme.Controller, error) {
	s, err := subs.ByNQN(ctx, targets[0].NQN)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var stale []nvme.Controller
	for _, ctrl := range s.Controllers {
		if ctrl.Address.TrAddr == "" {
			continue
		}
		if !matchesAny(ctrl, targets) {
			stale = append(stale, ctrl)
		}
	}
	return stale, nil
}

func matchesAny(ctrl nvme.Controller, targets []Target) bool {
	for _, t := range targets {
		if matchesTarget(ctrl, t) {
			return true
		}
	}
	return false
}
