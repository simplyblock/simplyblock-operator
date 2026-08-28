package nvmeof

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// PathResult is the outcome of one target in an ordered multipath connect.
// ConnectPaths returns one per target it attempted, in the order attempted.
type PathResult struct {
	Target Target

	// Live reports whether a live controller for this path exists after the
	// attempt. A path that is not live carries the reason in Err.
	Live bool
	// AlreadyPresent is true when a controller for this exact path existed
	// before the attempt, so no new connect was issued.
	AlreadyPresent bool
	// Err is why the path did not come up; nil when Live is true.
	Err error
}

// ConnectPaths attaches a subsystem over an ordered list of fabric paths.
//
// The control plane returns a volume's paths in descending priority —
// primary, secondary, tertiary — and that order is significant: the first
// path to come up is the one carrying I/O until the kernel has the full ANA
// picture. ConnectPaths therefore attaches them strictly in the order given,
// one at a time, waiting for each to reach a live state before starting the
// next. It never runs them concurrently and never reorders them.
//
// A path that cannot be established (its storage node is restarting, say, so
// the primary is unreachable while the secondary holds leadership) does not
// stop the ones behind it: the attempt is recorded in its PathResult and the
// next path in priority order follows. Waiting for a single path is bounded
// by WithPathTimeout so one unreachable node cannot consume the whole
// context. Re-establishing a path that failed here is the caller's job (a
// reconcile loop); retrying must re-issue that path alone, so the paths that
// did come up keep their relative order.
//
// It is idempotent: a path whose controller already exists is left alone
// rather than connected a second time, which would create a duplicate
// controller for the same endpoint.
//
// The returned error is non-nil only when no path at all could be
// established — with one live path the subsystem is attached and usable, and
// per-path failures are reported through the results. All targets must name
// the same subsystem NQN.
func (c *connector) ConnectPaths(ctx context.Context, targets []Target) ([]PathResult, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("connect: no targets")
	}
	nqn := targets[0].NQN
	for _, t := range targets {
		if t.NQN == "" {
			return nil, fmt.Errorf("connect: empty NQN")
		}
		if t.NQN != nqn {
			return nil, fmt.Errorf("connect %s: targets span more than one subsystem (%s)", nqn, t.NQN)
		}
	}

	results := make([]PathResult, 0, len(targets))
	live := 0
	for _, t := range targets {
		r := c.connectPath(ctx, t)
		results = append(results, r)
		if r.Live {
			live++
		}
		// A canceled or expired caller context stops the walk: the paths
		// behind this one are left unattempted rather than run against a
		// context that can only fail.
		if ctx.Err() != nil {
			break
		}
	}

	if live == 0 {
		reasons := make([]error, 0, len(results))
		for _, r := range results {
			if r.Err != nil {
				reasons = append(reasons, r.Err)
			}
		}
		return results, fmt.Errorf("connect %s: no path could be established: %w", nqn, errors.Join(reasons...))
	}
	return results, nil
}

// connectPath establishes a single path and waits for it to go live. The
// per-path timeout covers the whole attempt — the controller-state lookups,
// the connect write and the wait for live — so no single step can hold up the
// paths behind this one, whichever one is slow.
func (c *connector) connectPath(parent context.Context, t Target) PathResult {
	r := PathResult{Target: t}

	ctx := parent
	if c.pathTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, c.pathTimeout)
		defer cancel()
	}

	ctrl, found, err := c.findPath(ctx, t)
	if err != nil {
		r.Err = fmt.Errorf("connect %s via %s: %w", t.NQN, endpoint(t), err)
		return r
	}
	switch {
	case found && ctrl.IsLive():
		r.AlreadyPresent, r.Live = true, true
		return r
	case found:
		// The controller exists but is not live yet — the kernel is still
		// connecting or reconnecting it. Writing the fabrics device again
		// would add a second controller for the same endpoint, so only wait.
		r.AlreadyPresent = true
	default:
		if _, err := c.attach(ctx, t); err != nil {
			r.Err = fmt.Errorf("connect %s via %s: %w", t.NQN, endpoint(t), err)
			return r
		}
	}

	if err := c.waitPathLive(ctx, t); err != nil {
		r.Err = err
		return r
	}
	r.Live = true
	return r
}

// waitPathLive polls until the controller for t is live. ctx carries the
// per-path deadline set by connectPath, and every lookup runs under it, so a
// resolver that blocks is bounded by the same deadline as the polling.
func (c *connector) waitPathLive(ctx context.Context, t Target) error {
	ticker := time.NewTicker(c.pollInterval())
	defer ticker.Stop()
	for {
		ctrl, found, err := c.findPath(ctx, t)
		if err != nil {
			return fmt.Errorf("waiting for %s via %s: %w", t.NQN, endpoint(t), err)
		}
		if found && ctrl.IsLive() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s via %s to become live: %w", t.NQN, endpoint(t), ctx.Err())
		case <-ticker.C:
		}
	}
}

// findPath returns the controller fronting t's subsystem over t's endpoint.
// A subsystem that is not attached at all is not an error.
func (c *connector) findPath(ctx context.Context, t Target) (nvme.Controller, bool, error) {
	s, err := c.subs.ByNQN(ctx, t.NQN)
	if errors.Is(err, errs.ErrNotFound) {
		return nvme.Controller{}, false, nil
	}
	if err != nil {
		return nvme.Controller{}, false, err
	}
	for _, ctrl := range s.Controllers {
		if matchesTarget(ctrl, t) {
			return ctrl, true, nil
		}
	}
	return nvme.Controller{}, false, nil
}

// matchesTarget reports whether ctrl is the fabric path t describes: same
// transport, same target address and service id. Controllers whose address
// the kernel does not report (PCIe) never match.
func matchesTarget(ctrl nvme.Controller, t Target) bool {
	if ctrl.Address.TrAddr == "" {
		return false
	}
	return strings.EqualFold(ctrl.Transport, string(transport(t))) &&
		ctrl.Address.TrAddr == t.Address &&
		ctrl.Address.TrSvcID == strconv.Itoa(port(t))
}

// Teardown ranks, lowest first. A path that cannot serve I/O is released
// first and the optimized path last, so I/O in flight keeps the best path it
// has for as long as possible: releasing the optimized path first would make
// the kernel fail I/O over to a path we are about to remove as well.
const (
	rankUnusable     = iota // inaccessible, persistent-loss, change
	rankUnknown             // no ANA information reported for this controller
	rankNonOptimized        // non-optimized — accessible, not preferred
	rankOptimized           // the path the kernel prefers for I/O
)

// anaRank maps an ANA state to its teardown rank.
func anaRank(s nvme.ANAState) int {
	switch s {
	case nvme.ANAOptimized:
		return rankOptimized
	case nvme.ANANonOptimized:
		return rankNonOptimized
	case "":
		return rankUnknown
	default: // inaccessible, persistent-loss, change
		return rankUnusable
	}
}

// disconnectOrder returns s's controllers in the order they must be torn
// down: by ANA state (unusable and non-optimized paths first, the optimized
// one last) and, among paths of equal rank, in reverse of the order they
// were connected — the kernel hands out controller instance numbers in
// creation order, so the path attached first is released last.
//
// A controller serving several namespaces is ranked by its best ANA state
// across them: optimized for any namespace means it is still carrying I/O.
// A controller with no ANA information (a single-path subsystem, or legs the
// kernel has not published yet) is treated as not-known-to-be-optimized and
// released before the optimized path.
func disconnectOrder(s nvme.Subsystem) []nvme.Controller {
	ranked := make(map[nvme.ControllerID]int, len(s.Controllers))
	for _, ns := range s.Namespaces {
		for _, p := range ns.Paths {
			r := anaRank(p.ANAState)
			if cur, ok := ranked[p.Controller]; !ok || r > cur {
				ranked[p.Controller] = r
			}
		}
	}
	rankOf := func(ctrl nvme.Controller) int {
		if r, ok := ranked[ctrl.ID]; ok {
			return r
		}
		return rankUnknown
	}

	out := slices.Clone(s.Controllers)
	slices.SortStableFunc(out, func(a, b nvme.Controller) int {
		if r := cmp.Compare(rankOf(a), rankOf(b)); r != 0 {
			return r
		}
		return cmp.Compare(instanceOf(b.ID), instanceOf(a.ID))
	})
	return out
}

// instanceOf extracts the kernel instance number from a controller id
// ("nvme12" -> 12). Ids that carry none rank as -1, which puts them after
// numbered controllers in the reverse-order tie-break.
func instanceOf(id nvme.ControllerID) int {
	n, err := strconv.Atoi(strings.TrimPrefix(string(id), "nvme"))
	if err != nil {
		return -1
	}
	return n
}
