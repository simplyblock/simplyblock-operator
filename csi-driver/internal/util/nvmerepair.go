package util

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
)

// Repair of the fabric states a plain connect cannot see.
//
// A connect can succeed at one layer of the NVMe object tree while the layer
// below it is unusable, and the check that gates the retry sits at the higher
// layer — so nothing looks missing and the retry spins. Both incidents this file
// exists for have that shape:
//
//   - A subsystem whose controllers are live and which exports no namespace at
//     all. `nvme connect` is satisfied, `nvme list-subsys` shows the NQN, and no
//     block device ever appears, so every NodeStageVolume attempt skips the
//     connect and times out on device discovery — which kubelet retries forever.
//     That is the multi-hour FIO pod hang.
//
//   - A controller that is connected while contributing NO path to the namespace
//     head. Its namespace never joined, so the device-scoped view does not list
//     it and the volume runs a path short — but `nvme connect` refuses with
//     "already connected", because at the controller level nothing is missing.
//     K8sNativeResilientFailoverTest iteration 28 (2026-08-09): 106 such retries
//     over 11 minutes for volume 638be965, not one of which produced a single
//     packet to the target. The volume ran at 2 of 3 paths the whole time and
//     lost all I/O when the outage removed the other two — ext4 went read-only
//     with "no available path - failing I/O". Across that 42-hour run many
//     volumes were affected, so the cluster was routinely below its configured
//     redundancy with no operator-visible signal.
//
// # Where the logic lives
//
// The diagnosis is atlas-lib/nvmeof's, not this file's. nvmeof.Inspect reads the
// sysfs this node already exposes and returns typed defects carrying the scope a
// repair would need and the co-tenant volumes it would disturb. That code has
// unit tests and snapshot tests replaying real kernel state captured from a live
// cluster — including states forced with two nvmet targets that cannot be
// reproduced by hand. A second implementation here would have none of that
// behind it, and the two would drift.
//
// The teardown is atlas's too, mechanism and all. nvmeof.Repair takes a
// ControllerDetacher — the one operation a repair performs — and nvmeof's
// CLIConnector supplies it over nvme-cli, which is how this driver talks to the
// fabric. So the ordering, the "a controller that fails to release must not stop
// the ones behind it" rule, and the disconnect itself are all shared.
//
// What is genuinely local, then, is only policy: which diagnosed defect to act
// on, when to refuse, and how often the same repair may run. atlas's Repairer
// would own that as well, but it drives connect too, and connect is still this
// driver's own — which is also why CLIConnector is taken here as a
// ControllerDetacher and not as a Connector.
//
// Reading sysfs rather than shelling out is also strictly better than what this
// file did before: no process per monitor tick, a real per-namespace ANA view
// instead of the difference between two `nvme list-subsys` invocations, and
// namespace UUIDs the co-tenant check can name in a log line.

const (
	// defaultRepairCooldown bounds how often the same repair may be applied to
	// the same endpoint, so a repair that does not stick degrades into periodic
	// retries instead of a teardown/reconnect loop at monitor cadence — which,
	// at a 3s tick, is what the difference amounts to.
	defaultRepairCooldown = 5 * time.Minute
)

// autoRepairKinds are the defects healSubsystem repairs unattended.
//
// The two omissions are deliberate and match atlas's own defaults. A missing
// namespace is the target not publishing it, which reconnecting cannot change,
// and every namespace that is there belongs to another volume. A stale endpoint
// is indistinguishable from a storage node that is merely restarting, and
// dropping a live data path on that evidence is not the node plugin's call.
// Both are still diagnosed and reported.
var autoRepairKinds = []nvmeof.DefectKind{
	nvmeof.DefectNoNamespace,
	nvmeof.DefectControllerNotContributing,
	nvmeof.DefectAmbiguousHead,
}

// repairAction records one defect considered and what became of it. A defect
// deliberately left alone is as much a part of the outcome as one repaired —
// more so, when a volume stays degraded because repairing it would have taken
// another volume down.
type repairAction struct {
	defect   nvmeof.Defect
	repaired bool
	skipped  string
	err      error
}

func (a repairAction) String() string {
	switch {
	case a.err != nil:
		return fmt.Sprintf("%s: repair failed: %v", a.defect, a.err)
	case a.repaired:
		return fmt.Sprintf("%s: repaired", a.defect)
	default:
		return fmt.Sprintf("%s: not repaired: %s", a.defect, a.skipped)
	}
}

// nvmeRepairer applies policy to the defects atlas diagnoses: narrowest repair
// first, never at the cost of another volume's block device, and never more
// often than the cooldown allows.
//
// The cooldown state lives on the value rather than in package-level maps so it
// is bounded and pruned; a node plugin repairing paths for months must not
// accumulate one entry per repair it ever made.
//
// It is safe for concurrent use.
type nvmeRepairer struct {
	subs nvme.SubsystemResolver
	devs nvme.DeviceResolver

	// detacher performs the teardown. A field so tests need no nvme-cli.
	detacher nvmeof.ControllerDetacher

	cooldown        time.Duration
	maxScope        nvmeof.Scope
	allowDisruptive bool
	kinds           []nvmeof.DefectKind
	now             func() time.Time

	mu   sync.Mutex
	last map[repairCooldownKey]time.Time
}

// repairCooldownKey identifies a repair across attempts.
//
// What may go in subject is constrained from both sides: narrow enough that two
// repairs outstanding at once do not collide — applying one would otherwise mask
// the other — and stable enough to survive a repair that did not stick, since a
// key the teardown itself changes would see a brand-new repair every time. See
// cooldownKeyOf for what each scope uses.
type repairCooldownKey struct {
	nqn     string
	kind    nvmeof.DefectKind
	scope   nvmeof.Scope
	subject string
}

// newNVMeRepairer returns a repairer reading the local sysfs and tearing
// controllers down through nvme-cli.
func newNVMeRepairer() *nvmeRepairer {
	cfg := nvme.SysfsConfig{}
	subs := nvme.NewSysfsSubsystemResolver(cfg)
	return &nvmeRepairer{
		subs: subs,
		devs: nvme.NewSysfsDeviceResolver(cfg),
		// atlas's nvme-cli connector, used for the one operation a repair
		// performs. Only the teardown is wanted here — attaching still goes
		// through this driver's own connect path — and ControllerDetacher is
		// exactly that much of a Connector.
		detacher: nvmeof.NewCLIConnector(subs),
		cooldown: defaultRepairCooldown,
		maxScope: nvmeof.ScopeSubsystem,
		kinds:    slices.Clone(autoRepairKinds),
		now:      time.Now,
		last:     make(map[repairCooldownKey]time.Time),
	}
}

// defaultRepairer is the process-wide repairer, so its cooldowns are shared by
// NodeStageVolume and the connection monitor — the two callers that would
// otherwise fight over the same controller.
var defaultRepairer = newNVMeRepairer()

// healSubsystem diagnoses the volume sel names and repairs the narrowest defect
// policy permits, returning everything diagnosed and everything done about it.
//
// It repairs at most one defect per call. A repair invalidates the diagnosis
// that produced it, and every caller is already a loop — kubelet retrying
// NodeStageVolume, or the monitor's next tick — so the next diagnosis belongs to
// the next call rather than to a loop in here.
//
// targets is the control plane's current answer. It is what separates a
// controller that is broken from one that is merely no longer published, so
// passing it matters: without it an obsolete endpoint and a dead path look the
// same. Pass nil when the caller has no answer to hand.
func (r *nvmeRepairer) healSubsystem(
	ctx context.Context,
	sel nvme.DeviceSelector,
	targets []nvmeof.Target,
) ([]nvmeof.Defect, []repairAction) {
	defects, err := nvmeof.Inspect(ctx, r.subs, r.devs, sel, targets)
	if err != nil {
		klog.Errorf("nvme repair: cannot diagnose %s: %v", sel.NQN, err)
		return nil, nil
	}
	if len(defects) == 0 {
		return nil, nil
	}
	for _, d := range defects {
		klog.V(4).Infof("nvme repair: %s", d)
	}

	action, ok := r.choose(defects)
	if !ok {
		return defects, nil
	}
	if action.skipped != "" {
		klog.Warningf("nvme repair: %s", action)
		return defects, []repairAction{action}
	}

	r.mark(action.defect)
	klog.Warningf("nvme repair: repairing %s by tearing down %s",
		action.defect, strings.Join(controllerNames(action.defect), ", "))
	if err := r.repair(ctx, action.defect); err != nil {
		action.err = err
		klog.Errorf("nvme repair: %s", action)
	} else {
		action.repaired = true
	}
	return defects, []repairAction{action}
}

// choose picks the defect to act on — the narrowest repairable one policy
// permits — and returns it as a pending action. A defect barred by policy is
// returned with skipped set so the reason is reported rather than dropped; ok is
// false only when there is nothing to say at all.
func (r *nvmeRepairer) choose(defects []nvmeof.Defect) (repairAction, bool) {
	candidates := make([]nvmeof.Defect, 0, len(defects))
	for _, d := range defects {
		if d.Repairable() && slices.Contains(r.kinds, d.Kind) {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return repairAction{}, false
	}
	// Narrowest blast radius first, and among equals the one disturbing no other
	// volume, so a permitted repair is preferred over one policy will turn away.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Scope != candidates[j].Scope {
			return candidates[i].Scope < candidates[j].Scope
		}
		return len(candidates[i].CoTenants) < len(candidates[j].CoTenants)
	})

	var barred repairAction
	for _, d := range candidates {
		reason := r.barrier(d)
		if reason == "" {
			return repairAction{defect: d}, true
		}
		if barred.defect.Kind == "" {
			barred = repairAction{defect: d, skipped: reason}
		}
	}
	return barred, true
}

// barrier returns why policy forbids repairing d, or "" when it does not.
func (r *nvmeRepairer) barrier(d nvmeof.Defect) string {
	if d.Scope > r.maxScope {
		return fmt.Sprintf("a %s-scope repair is needed but repairs are capped at %s scope",
			d.Scope, r.maxScope)
	}
	if d.Disruptive() && !r.allowDisruptive {
		return fmt.Sprintf("the repair would leave %s with no usable path", describeCoTenants(d.CoTenants))
	}
	if left, ok := r.cooling(d); ok {
		return fmt.Sprintf("the same repair ran %s ago; %s of cooldown left",
			(r.cooldown - left).Truncate(time.Second), left.Truncate(time.Second))
	}
	return ""
}

// repair carries out the teardown atlas's Repair defines: the controllers the
// defect names, in the order it names them — paths that cannot serve I/O first
// and the optimized path last — and a controller that fails to release does not
// stop the ones behind it.
//
// Reconnecting is the caller's: `nvme connect` is what the caller was doing
// anyway, and a repair only has to clear the way for it. Policy is choose's job;
// this only acts.
func (r *nvmeRepairer) repair(ctx context.Context, d nvmeof.Defect) error {
	return nvmeof.Repair(ctx, r.detacher, d)
}

// cooling reports whether d's repair is still inside its cooldown, and how much
// of it is left.
func (r *nvmeRepairer) cooling(d nvmeof.Defect) (left time.Duration, cooling bool) {
	if r.cooldown <= 0 {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	last, seen := r.last[cooldownKeyOf(d)]
	if !seen {
		return 0, false
	}
	if elapsed := r.now().Sub(last); elapsed < r.cooldown {
		return r.cooldown - elapsed, true
	}
	return 0, false
}

// mark starts d's cooldown, pruning entries whose own cooldown expired long ago
// so a long-running node plugin does not accumulate one per repair ever made.
func (r *nvmeRepairer) mark(d nvmeof.Defect) {
	if r.cooldown <= 0 {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.last {
		if now.Sub(t) > 2*r.cooldown {
			delete(r.last, k)
		}
	}
	r.last[cooldownKeyOf(d)] = now
}

// cooldownKeyOf is d's identity across attempts, and mirrors atlas's own keyOf.
//
// A controller-scope repair is keyed by its fabric endpoint: the controller name
// is exactly what the repair changes — tearing down nvme3 and reconnecting
// yields nvme7 at the same address — while the endpoint survives, and it keeps
// three broken paths of one subsystem as three repairs rather than one.
//
// A subsystem-scope repair is keyed by nothing further. It concerns the
// subsystem behind this NQN whatever instance the kernel hands out after the
// teardown, so keying on the instance id would lose the cooldown as soon as a
// reconnect renumbered it — reopening the loop the cooldown exists to close.
//
// DefectAmbiguousHead is the exception and keys on the instance, because it is
// the one defect Inspect can report several times for a single NQN, one per
// stale head. Sharing a key would let repairing the first mark the rest as
// handled and leave them attached.
func cooldownKeyOf(d nvmeof.Defect) repairCooldownKey {
	k := repairCooldownKey{nqn: d.NQN, kind: d.Kind, scope: d.Scope}
	switch {
	case d.Scope == nvmeof.ScopeController && len(d.Controllers) > 0:
		k.subject = controllerEndpoint(d.Controllers[0])
	case d.Kind == nvmeof.DefectAmbiguousHead:
		k.subject = string(d.Subsystem)
	}
	return k
}

func controllerEndpoint(ctrl nvme.Controller) string {
	if ctrl.Address.TrSvcID == "" {
		return ctrl.Address.TrAddr
	}
	return ctrl.Address.TrAddr + ":" + ctrl.Address.TrSvcID
}

func controllerNames(d nvmeof.Defect) []string {
	names := make([]string, 0, len(d.Controllers))
	for _, ctrl := range d.Controllers {
		names = append(names, string(ctrl.ID))
	}
	return names
}

// describeCoTenants names the volumes a repair would strand the way an operator
// would recognise them: by lvol UUID, which is what the namespace carries.
func describeCoTenants(nss []nvme.Namespace) string {
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

// targetsFromConnections turns the control plane's answer into the target list
// Inspect compares the attached controllers against.
func targetsFromConnections(nqn string, conns []*LvolConnectResp) []nvmeof.Target {
	targets := make([]nvmeof.Target, 0, len(conns))
	for _, c := range conns {
		if c == nil {
			continue
		}
		transport := nvmeof.Transport(strings.ToLower(c.TargetType))
		if transport == "" {
			transport = nvmeof.TransportTCP
		}
		targets = append(targets, nvmeof.Target{
			NQN:       nqn,
			Transport: transport,
			Address:   c.IP,
			Port:      c.Port,
		})
	}
	return targets
}

// repairFabric diagnoses this volume's subsystem and repairs what policy allows,
// reporting whether anything was actually torn down — the only case where
// retrying the attach can produce a different answer.
//
// The control plane's endpoints are not passed. A NodeStageVolume that could not
// produce a device has no business deciding an endpoint is stale, and the two
// defects that matter at attach time — a subsystem exporting no namespace, and a
// duplicate head — need no endpoint comparison at all.
func (nvmf *initiatorNVMf) repairFabric(ctx context.Context) bool {
	sel := nvme.DeviceSelector{NQN: nvmf.nqn, NSID: nvme.NamespaceID(nvmf.nsId)}
	_, actions := defaultRepairer.healSubsystem(ctx, sel, nil)
	for _, a := range actions {
		if a.repaired {
			return true
		}
	}
	return false
}

// healMonitoredVolume is the connection monitor's repair hook, for the state its
// path counting can see but its reconnect cannot fix: a controller that is
// connected while contributing no path to the namespace head. `nvme connect`
// refuses it with "already connected", so the monitor re-issues a connect that
// never reaches the target and the volume stays a path short indefinitely.
//
// The volume is selected by lvol UUID rather than by namespace id: the monitor
// already resolved it from /sys/block/<dev>/uuid, and on a shared subsystem it
// is what tells this volume from its co-tenants without deriving anything.
func healMonitoredVolume(ctx context.Context, nqn, lvolID string, conns []*LvolConnectResp) {
	sel := nvme.DeviceSelector{NQN: nqn, UUID: lvolID}
	_, actions := defaultRepairer.healSubsystem(ctx, sel, targetsFromConnections(nqn, conns))
	for _, a := range actions {
		if a.repaired {
			// The next monitor tick reconnects the torn-down path: reconnecting
			// here would race the reconcile that is about to run anyway.
			klog.Infof("nvme repair: %s repaired for lvol %s; the next reconcile will reconnect the path",
				a.defect.Kind, lvolID)
		}
	}
}
