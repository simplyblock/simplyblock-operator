package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/locks"
	"github.com/simplyblock/atlas/nvme"
)

const (
	// defaultSettleWindow bounds how long Attach waits for the block device
	// after every path it could establish is live. It is a debounce, not a
	// patience budget: the namespace shows up in sysfs a moment after the
	// controller goes live, with no udev in the way, so a few hundred
	// milliseconds is the normal case and anything beyond this window is a
	// defect worth diagnosing rather than waiting out. Diagnosis is what decides
	// whether to act — the window only says when to look.
	defaultSettleWindow = 3 * time.Second

	// defaultRepairCooldown bounds how often the same repair may be applied to
	// the same target, so a repair that does not stick degrades into periodic
	// retries instead of a teardown/reconnect loop at monitor cadence. A node
	// plugin that reconnects a controller every three seconds forever is worse
	// than one that leaves it alone.
	defaultRepairCooldown = 5 * time.Minute

	// defaultRepairRounds bounds how many repairs one Attach may apply. Each
	// round re-diagnoses, because a repair invalidates the previous diagnosis,
	// and each acts on at most one defect. Three is one per auto-repairable
	// kind: past that, the fabric is being churned rather than fixed.
	defaultRepairRounds = 3
)

// autoRepairKinds are the defects Attach repairs unattended.
//
// The two omissions are deliberate. DefectNamespaceMissing means the target is
// not publishing the namespace: the connection works, so reconnecting cannot
// change the answer, and every namespace that *is* there belongs to another
// volume. DefectStaleEndpoint means the control plane stopped publishing an
// endpoint, which a node in restart also looks like — dropping a live data path
// on that evidence is the caller's call, and has been since ReconcilePaths.
// Both are reported so a caller can act, and Repair will act on either when
// explicitly asked.
var autoRepairKinds = []DefectKind{
	DefectNoNamespace,
	DefectControllerNotContributing,
	DefectAmbiguousHead,
}

// RepairAction records one defect Attach considered and what became of it.
// A defect that was diagnosed and deliberately left alone is as much a part of
// the outcome as one that was repaired — more, when a volume stays degraded
// because repairing it would have taken someone else's volume down.
type RepairAction struct {
	// Defect is what was considered.
	Defect Defect
	// Repaired reports whether the teardown was carried out.
	Repaired bool
	// Skipped is why it was not, empty when it was.
	Skipped string
	// Err is why the teardown failed, when it was attempted and did not work.
	Err error
}

func (a RepairAction) String() string {
	switch {
	case a.Err != nil:
		return fmt.Sprintf("%s: repair failed: %v", a.Defect, a.Err)
	case a.Repaired:
		return fmt.Sprintf("%s: repaired", a.Defect)
	default:
		return fmt.Sprintf("%s: not repaired: %s", a.Defect, a.Skipped)
	}
}

// AttachResult is the whole outcome of an Attach: the device if one came up, the
// per-path state, and everything that was diagnosed and done along the way.
//
// The diagnostics are returned rather than only logged because their absence was
// itself an incident: a cluster ran for 42 hours with volumes routinely below
// their configured redundancy — hundreds of degraded reports per volume — and
// nothing above the node plugin could see it. A caller that surfaces Defects as
// events or status conditions turns that into something operable.
type AttachResult struct {
	// Device is the namespace block device, zero when none came up.
	Device nvme.Device
	// Paths is the per-path outcome of the last connect attempt, in priority
	// order (primary first).
	Paths []PathResult
	// Defects is everything diagnosed, across all rounds, oldest first. Empty
	// on the happy path.
	Defects []Defect
	// Repairs is what was done, or deliberately not done, about them.
	Repairs []RepairAction
}

// Degraded reports whether the device came up on fewer paths than were asked
// for — usable, with less redundancy than the control plane published.
func (r AttachResult) Degraded() bool {
	return r.live() > 0 && r.live() < len(r.Paths)
}

func (r AttachResult) live() int {
	n := 0
	for _, p := range r.Paths {
		if p.Live {
			n++
		}
	}
	return n
}

// Repairer attaches volumes and heals the fabric states that a plain connect
// cannot, applying a policy Inspect deliberately does not have.
//
// It sits above Connector rather than inside it: the connector stays mechanical,
// and the decisions that need judgment — how narrow a repair to prefer, whether
// a repair may disturb another volume, how often the same repair may be
// retried — live here, on a value, together with the state they need. That state
// is why this is a type and not a function: cooldowns have to outlive a single
// attach to do anything, and a package-level map of them would be shared by
// every caller in the process and never pruned.
//
// A Repairer is safe for concurrent use.
type Repairer struct {
	c    Connector
	subs nvme.SubsystemResolver
	devs nvme.DeviceResolver

	settle          time.Duration
	cooldown        time.Duration
	rounds          int
	maxScope        Scope
	allowDisruptive bool
	kinds           []DefectKind

	// now is time.Now, a field so tests need not sit out a real cooldown.
	now func() time.Time

	mu   sync.Mutex
	last map[repairKey]time.Time
}

// repairKey identifies a repair for cooldown purposes: the same defect on the
// same target, recognized again on a later attempt.
//
// What may go in subject is constrained from both sides. It has to be narrow
// enough that two repairs outstanding at once do not collide — or applying one
// would silently mask the other — and stable enough to survive a repair that did
// not stick, since a key the teardown itself changes would see a brand-new repair
// every time, which is the disconnect/reconnect loop the cooldown exists to
// prevent. Neither the controller id nor the kernel subsystem id satisfies both
// in general: a repair re-creates them. See keyOf for what each scope uses.
type repairKey struct {
	nqn     string
	kind    DefectKind
	scope   Scope
	subject string
}

// RepairOption configures a Repairer.
type RepairOption func(*Repairer)

// WithSettleWindow bounds how long Attach waits for the block device before
// diagnosing why it is not there. Too short and a healthy connect gets
// diagnosed while the kernel is still publishing the namespace; too long and a
// broken one stays broken for exactly that much longer. Zero or less restores
// the default.
func WithSettleWindow(d time.Duration) RepairOption {
	return func(r *Repairer) {
		if d > 0 {
			r.settle = d
		}
	}
}

// WithRepairCooldown bounds how often the same repair may be applied to the same
// target. Zero disables the cooldown, which lets a repair that does not stick
// run at whatever cadence the caller attaches — appropriate for a one-shot
// NodeStage, not for a monitor loop.
func WithRepairCooldown(d time.Duration) RepairOption {
	return func(r *Repairer) { r.cooldown = max(d, 0) }
}

// WithRepairRounds bounds how many repairs a single Attach may apply. Zero or
// less disables repair entirely, leaving Attach a connect that diagnoses but
// never acts — the shape a controller wants when it needs to report a fabric
// state without changing it.
func WithRepairRounds(n int) RepairOption {
	return func(r *Repairer) { r.rounds = max(n, 0) }
}

// WithMaxRepairScope caps how much of the fabric a repair may tear down.
// ScopeController allows single-path repairs only, which can never remove a
// namespace device that another path still serves; ScopeNone disables repair as
// WithRepairRounds(0) does.
func WithMaxRepairScope(s Scope) RepairOption {
	return func(r *Repairer) { r.maxScope = s }
}

// WithDisruptiveRepairs allows repairs that leave another volume's namespace
// with no usable path — that is, ones which take a block device away from a
// volume that is not the caller's.
//
// It is off by default, and turning it on should be a deliberate answer to a
// specific situation. The failure it permits is not subtle: co-tenant volumes
// lose I/O, and the pods on them see ext4 remount read-only. A volume of one's
// own that stays degraded is the better outcome nearly every time.
func WithDisruptiveRepairs(allow bool) RepairOption {
	return func(r *Repairer) { r.allowDisruptive = allow }
}

// WithAutoRepairKinds replaces the set of defects Attach repairs unattended.
// Defects outside it are still diagnosed and reported. Passing none disables
// repair while keeping the diagnosis.
func WithAutoRepairKinds(kinds ...DefectKind) RepairOption {
	return func(r *Repairer) { r.kinds = slices.Clone(kinds) }
}

// NewRepairer returns a Repairer attaching through c and reading kernel state
// through subs and devs, both required. Defaults: a 3s settle window, a 5m
// per-repair cooldown, at most 3 repairs per attach, subsystem-wide repairs
// allowed but never at the cost of another volume's block device.
func NewRepairer(
	c Connector,
	subs nvme.SubsystemResolver,
	devs nvme.DeviceResolver,
	opts ...RepairOption,
) *Repairer {
	r := &Repairer{
		c:        c,
		subs:     subs,
		devs:     devs,
		settle:   defaultSettleWindow,
		cooldown: defaultRepairCooldown,
		rounds:   defaultRepairRounds,
		maxScope: ScopeSubsystem,
		kinds:    slices.Clone(autoRepairKinds),
		now:      time.Now,
		last:     make(map[repairKey]time.Time),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Attach establishes a volume over all of its paths, repairs the fabric states a
// plain connect cannot see, and returns the namespace block device.
//
// It is ConnectMultipathDevice plus a diagnosis: connect every path, wait out
// the settle window, then ask Inspect what is inconsistent and act on the
// narrowest defect it is allowed to act on. Each repair is followed by a fresh
// connect and a fresh diagnosis, since a teardown invalidates the previous one,
// and repairs stop at the round limit or as soon as nothing is left to do.
//
// The diagnosis runs even when the block device is already there, which is the
// difference between this and waiting for a device. The worst of these states is
// not the volume with no device — that one at least fails visibly. It is the
// volume that has a device and silently runs a path short: a live controller
// contributing nothing, redundancy quietly below what the control plane
// published, and every connect answering "already connected." Nothing about the
// device's presence says the fabric is right, so Attach is safe and useful to
// call on every tick of a monitor loop, not just at NodeStage.
//
// Repairs are narrowest-first: a single controller before a whole subsystem, so
// the fabric is disturbed as little as the fix allows. A repair is not carried
// out when it would leave another volume without a usable path, when it would
// take the caller's own device down, or when the same repair ran recently — all
// three overridable, and every one of them reported in AttachResult.Repairs. A
// volume deliberately left degraded is something the caller needs to be told.
//
// nsid picks the namespace on a multi-namespace subsystem — pass
// lvol.Connection.NSID; 0 means "the subsystem's only namespace," which is
// enough for a plain lvol but ambiguous for a shared subsystem.
//
// The returned error is non-nil only when no device came up. AttachResult is
// populated either way — including its Defects on the success path, which is
// where a caller that reports fabric health gets it from.
func (r *Repairer) Attach(ctx context.Context, targets []Target, nsid nvme.NamespaceID) (AttachResult, error) {
	if len(targets) == 0 {
		return AttachResult{}, fmt.Errorf("attach: no targets")
	}
	sel := nvme.DeviceSelector{NQN: targets[0].NQN, NSID: nsid}

	var res AttachResult
	// acted keeps one Attach from applying the same repair twice: a repair that
	// did not help will be diagnosed again next round, and re-applying it there
	// is how a teardown/reconnect loop starts.
	acted := make(map[repairKey]bool)

	for round := 0; ; round++ {
		paths, err := r.c.ConnectPaths(ctx, targets)
		if len(paths) > 0 {
			res.Paths = paths
		}
		if err != nil {
			return res, err
		}

		dev, waitErr := r.probe(ctx, sel)
		res.Device = dev
		// A caller whose own deadline ran out gets its error unchanged: what is
		// wrong is that there was no time left, not the fabric.
		if waitErr != nil && ctx.Err() != nil {
			return res, waitErr
		}

		defects, ierr := Inspect(ctx, r.subs, r.devs, sel, targets)
		res.Defects = append(res.Defects, defects...)
		if ierr != nil {
			return res, errors.Join(waitErr, ierr)
		}

		if round >= r.rounds {
			return res, r.outcome(sel, waitErr, res, "repair rounds exhausted")
		}
		action, ok := r.choose(defects, dev, waitErr == nil, acted)
		if !ok {
			return res, r.outcome(sel, waitErr, res, "nothing left to repair")
		}

		if action.Skipped == "" {
			acted[keyOf(action.Defect)] = true
			r.mark(action.Defect)
			if err := Repair(ctx, r.c, action.Defect); err != nil {
				action.Err = err
			} else {
				action.Repaired = true
			}
		}
		res.Repairs = append(res.Repairs, action)

		// Nothing was torn down, so another round would connect the same fabric,
		// wait out the same window and reach the same verdict.
		if !action.Repaired {
			return res, r.outcome(sel, waitErr, res, "no repair could be applied")
		}
	}
}

// probe waits for the selected device under the earlier of the settle window and
// the caller's own deadline, so a caller with a shorter one (kubelet's CSI
// operation timeout, say) still wins.
func (r *Repairer) probe(ctx context.Context, sel nvme.DeviceSelector) (nvme.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, r.settle)
	defer cancel()
	return WaitForDevice(ctx, r.devs, sel)
}

// choose picks the defect to act on this round — the narrowest repairable one
// the policy permits — and returns it as a pending action. A defect that is
// repairable in principle but barred by policy is returned with Skipped set, so
// the reason reaches the caller instead of being dropped; ok is false only when
// there is nothing to say at all.
//
// dev is the caller's own device when one is attached (attached says whether it
// is), so a repair can be barred from tearing down the very paths serving it.
func (r *Repairer) choose(
	defects []Defect,
	dev nvme.Device,
	attached bool,
	acted map[repairKey]bool,
) (RepairAction, bool) {
	candidates := make([]Defect, 0, len(defects))
	for _, d := range defects {
		if d.Repairable() && slices.Contains(r.kinds, d.Kind) && !acted[keyOf(d)] {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return RepairAction{}, false
	}
	// Narrowest blast radius first, and among equals the one that disturbs no
	// other volume, so a permitted repair is preferred over one that policy will
	// only turn away.
	slices.SortStableFunc(candidates, func(a, b Defect) int {
		if a.Scope != b.Scope {
			return int(a.Scope) - int(b.Scope)
		}
		return len(a.CoTenants) - len(b.CoTenants)
	})

	var barred RepairAction
	for _, d := range candidates {
		reason := r.barrier(d, dev, attached)
		if reason == "" {
			return RepairAction{Defect: d}, true
		}
		if barred.Defect.Kind == "" {
			barred = RepairAction{Defect: d, Skipped: reason}
		}
	}
	return barred, true
}

// barrier returns why policy forbids repairing d, or "" when it does not.
func (r *Repairer) barrier(d Defect, dev nvme.Device, attached bool) string {
	if d.Scope > r.maxScope {
		return fmt.Sprintf("a %s-scope repair is needed but repairs are capped at %s scope",
			d.Scope, r.maxScope)
	}
	if d.Disruptive() && !r.allowDisruptive {
		return fmt.Sprintf("the repair would leave %s with no usable path", describeNamespaces(d.CoTenants))
	}
	// Repairing a volume by disconnecting the paths that are serving it is not a
	// repair. This is the case that makes it safe to diagnose an already-attached
	// volume: a path it does not use may be torn down, the last one it does may
	// not.
	if attached && ownDeviceAtRisk(d, dev) {
		return fmt.Sprintf("the repair would leave this volume's own device %s with no usable path",
			dev.Namespace.DevicePath)
	}
	if left, ok := r.cooling(d); ok {
		return fmt.Sprintf("the same repair ran %s ago; %s of cooldown left",
			(r.cooldown - left).Truncate(time.Second), left.Truncate(time.Second))
	}
	return ""
}

// ownDeviceAtRisk reports whether repairing d would leave the caller's own
// device with no path that can serve I/O.
//
// Two states are not at risk and must not be treated as such. A defect about a
// different kernel subsystem instance cannot touch this device however the
// controller ids read — that is the whole point of cleaning up a stale head next
// to a live one. And a device that is already unable to serve I/O has nothing
// left to lose, which is what lets the stale head be torn down when it is the
// only thing a lookup found.
func ownDeviceAtRisk(d Defect, dev nvme.Device) bool {
	if d.Subsystem != "" && dev.Subsystem.ID != "" && d.Subsystem != dev.Subsystem.ID {
		return false
	}
	if !dev.Accessible() {
		return false
	}
	victims := make(map[nvme.ControllerID]bool, len(d.Controllers))
	for _, ctrl := range d.Controllers {
		victims[ctrl.ID] = true
	}
	return !survivesTeardown(dev.Namespace, victims)
}

// cooling reports whether d's repair is still inside its cooldown, and how much
// of it is left.
func (r *Repairer) cooling(d Defect) (left time.Duration, cooling bool) {
	if r.cooldown <= 0 {
		return 0, false
	}
	// ViaLock rather than WithLockValue: "not cooling" is an ordinary answer, not
	// a failure, and routing it through an error would mean inventing one to carry
	// a bool.
	locks.ViaLock(&r.mu, func() {
		last, seen := r.last[keyOf(d)]
		if !seen {
			return
		}
		if elapsed := r.now().Sub(last); elapsed < r.cooldown {
			left, cooling = r.cooldown-elapsed, true
		}
	})
	return left, cooling
}

// mark records that d's repair is being applied now, starting its cooldown. It
// also prunes entries whose cooldown has long expired, so a node plugin that
// attaches volumes for months does not accumulate one entry per repair it ever
// made.
func (r *Repairer) mark(d Defect) {
	if r.cooldown <= 0 {
		return
	}
	now := r.now()
	locks.ViaLock(&r.mu, func() {
		for k, t := range r.last {
			if now.Sub(t) > 2*r.cooldown {
				delete(r.last, k)
			}
		}
		r.last[keyOf(d)] = now
	})
}

// keyOf is d's cooldown identity.
//
// A controller-scope repair is keyed by its fabric endpoint. The controller id
// is exactly what the repair changes — tearing down nvme3 and reconnecting
// yields nvme7 at the same address — while the endpoint survives, and it keeps
// the paths distinct: three broken paths of one subsystem are three repairs, not
// one.
//
// A subsystem-scope repair is keyed by nothing further: it concerns the
// subsystem behind this NQN, whatever instance the kernel hands out after the
// teardown, and keying on the instance id would lose the cooldown the moment a
// reconnect produced a different one.
//
// DefectAmbiguousHead is the exception and does key on the instance, because it
// is the one defect Inspect can report several times for a single NQN — one per
// stale head. Sharing a key there would let repairing the first mark the rest as
// already handled and leave them attached. The instance is stable in the way
// that matters here: a repair that fails leaves that same head in place, and one
// that succeeds removes the head the key names.
func keyOf(d Defect) repairKey {
	k := repairKey{nqn: d.NQN, kind: d.Kind, scope: d.Scope}
	switch {
	case d.Scope == ScopeController && len(d.Controllers) > 0:
		k.subject = endpointOf(d.Controllers[0])
	case d.Kind == DefectAmbiguousHead:
		k.subject = string(d.Subsystem)
	}
	return k
}

// outcome turns the end of the repair loop into a result. A device that came up
// is a success however much was diagnosed on the way — a volume attached over two
// of its three paths is degraded, not failed, and failing it would take away the
// two paths it has.
//
// Otherwise, it builds the error, naming what was diagnosed and what was or was
// not done about it. The wait error alone — "no device turned up" — is what made
// the original incidents so hard to read; the diagnosis is the part that says
// whether anyone can do anything about it.
func (r *Repairer) outcome(sel nvme.DeviceSelector, waitErr error, res AttachResult, why string) error {
	if waitErr == nil {
		return nil
	}
	err := fmt.Errorf("attach %s: no namespace device: %s: %w", sel, why, waitErr)
	if len(res.Defects) == 0 {
		return err
	}
	detail := make([]string, 0, len(res.Defects)+len(res.Repairs))
	for _, d := range res.Defects {
		detail = append(detail, d.String())
	}
	for _, a := range res.Repairs {
		detail = append(detail, a.String())
	}
	return fmt.Errorf("%w; diagnosed: %s", err, strings.Join(detail, "; "))
}

// Repair carries out d's remedy: it tears down the controllers d names, in the
// teardown order Inspect put them in, so that the next connect re-runs the
// controller and namespace scan the kernel got wrong.
//
// It applies no policy whatsoever — not the cooldown, not the blast-radius
// check, not the scope cap. It will take down a whole subsystem, and every
// co-tenant volume on it, if that is what the defect it is handed says. Repairer
// is what decides; this is what acts, and it is exported for callers that make
// that decision themselves: a migration controller repairing a stale endpoint it
// knows is genuinely gone, say, which Attach will never do on its own.
//
// Reconnecting is not part of it. A repair leaves the fabric torn down, and what
// to reconnect — one path, every path, in which order — is the caller's; go
// through Attach to have both done together.
//
// A defect with no local remedy (ScopeNone) is refused with errs.ErrUnsupported
// rather than silently doing nothing.
func Repair(ctx context.Context, d ControllerDetacher, defect Defect) error {
	return repairWith(ctx, d, defect)
}

// ControllerDetacher is the single operation a repair performs. Repair takes
// this rather than a whole Connector because tearing one controller down is all
// it does, and demanding the rest would make every caller that cannot attach —
// a driver whose connect path lives elsewhere, a diagnostic tool — supply four
// methods it has no implementation for. Connector satisfies it.
type ControllerDetacher interface {
	// DisconnectController detaches a single controller, leaving the
	// subsystem's other paths in place. It must be idempotent: a controller
	// already gone is not an error.
	DisconnectController(ctx context.Context, ctrl nvme.Controller) error
}

func repairWith(ctx context.Context, c ControllerDetacher, d Defect) error {
	if !d.Repairable() {
		return fmt.Errorf("repair %s: %s has no local remedy: %w", d.NQN, d.Kind, errs.ErrUnsupported)
	}
	var firstErr error
	for _, ctrl := range d.Controllers {
		// A controller that fails to release does not stop the ones behind it:
		// the optimized path must still be torn down last rather than left
		// behind, which is the whole point of the order.
		if err := c.DisconnectController(ctx, ctrl); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("repair %s (%s): %w", d.NQN, ctrl.ID, err)
		}
	}
	return firstErr
}

// describeNamespaces names namespaces the way an operator would recognize them:
// by the volume UUID where there is one, since that is the lvol id, and by
// device path otherwise.
func describeNamespaces(nss []nvme.Namespace) string {
	if len(nss) == 0 {
		return "no volume"
	}
	parts := make([]string, len(nss))
	for i, ns := range nss {
		switch {
		case ns.UUID != "":
			parts[i] = fmt.Sprintf("volume %s (nsid %d)", ns.UUID, ns.ID)
		case ns.DevicePath != "":
			parts[i] = fmt.Sprintf("%s (nsid %d)", ns.DevicePath, ns.ID)
		default:
			parts[i] = fmt.Sprintf("nsid %d", ns.ID)
		}
	}
	return strings.Join(parts, "; ")
}
