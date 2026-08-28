package nvmeof

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// A connect can succeed at one layer of the NVMe object tree and still leave
// the layer below it unusable, and the check that gates a retry sits at the
// higher layer — so the retry sees nothing missing and spins forever. Every
// incident this file diagnoses has that shape:
//
//   - the subsystem is attached and its controllers are live, but it exports no
//     namespace at all, so no block device ever appears (DefectNoNamespace);
//   - a controller for the wanted endpoint is live, but contributes no path to
//     the namespace, so the volume runs below its published redundancy while a
//     connect keeps answering "already connected"
//     (DefectControllerNotContributing);
//   - two kernel subsystem instances answer for one NQN, one of them the stale
//     leftover of an earlier connect, so a lookup by NQN can hand back the
//     wrong block device (DefectAmbiguousHead).
//
// Inspect names these positively, from state the kernel already publishes,
// rather than inferring them from a wait that ran out or from the text of an
// nvme-cli error. That distinction matters: a timeout cannot tell "slow" from
// "broken," so a diagnosis built on one is either too eager or too patient,
// whereas "live controller, zero namespaces" is decidable on the first look.
//
// Diagnosis is separate from repair on purpose. A migration controller wants to
// report that a volume is short of paths without healing it mid-migration, and
// the node-side attach wants the opposite; see Repairer for the policy layer and
// Repair for the mechanism.

// DefectKind names a way an attached subsystem can be inconsistent with what the
// caller asked for.
type DefectKind string

const (
	// DefectNoNamespace is a subsystem with at least one live controller that
	// exports no namespace at all — the leftover of a half-completed or broken
	// connection. Connect is satisfied by it, since the controllers really are
	// live, so every retry short-circuits and waits for a block device that
	// will never appear.
	DefectNoNamespace DefectKind = "no-namespace"

	// DefectNamespaceMissing is a subsystem that exports namespaces, but not
	// the one selected. Unlike DefectNoNamespace the connection works — the
	// target simply is not publishing this namespace to this host, which no
	// amount of local reconnecting changes. It is reported, never repaired
	// automatically: the other namespaces belong to other volumes.
	DefectNamespaceMissing DefectKind = "namespace-missing"

	// DefectControllerNotContributing is a live controller that provides no
	// path to the selected namespace, so it counts toward neither redundancy
	// nor I/O while looking connected from every angle a connect checks. A
	// fresh controller re-runs the namespace scan, which is what recovers it.
	DefectControllerNotContributing DefectKind = "controller-not-contributing"

	// DefectAmbiguousHead is more than one kernel subsystem instance answering
	// for a single NQN — a stale head the kernel has not reaped sitting beside
	// the fresh one. Returning either block device is a coin flip, and the
	// wrong side of it is silent corruption, so the stale instance has to go.
	DefectAmbiguousHead DefectKind = "ambiguous-head"

	// DefectStaleEndpoint is an attached controller whose endpoint the control
	// plane no longer publishes — typically the old primary after a migration.
	// It is reported, never repaired automatically: an endpoint absent from the
	// current answer is not necessarily gone for good (a node in restart is the
	// obvious case), and dropping a live data path is the caller's decision.
	DefectStaleEndpoint DefectKind = "stale-endpoint"
)

// Scope is how much of the fabric a defect's repair tears down. Ordered by
// blast radius, so the narrowest repair that can fix a thing is the smallest
// value, and Repairer walks defects in this order.
type Scope int

const (
	// ScopeNone marks a defect with no local remedy: nothing that can be torn
	// down and reconnected changes it.
	ScopeNone Scope = iota
	// ScopeController repairs by tearing down one controller — one path of a
	// multipath subsystem. The remaining paths, and every namespace they serve,
	// keep working.
	ScopeController
	// ScopeSubsystem repairs by tearing down every controller of one kernel
	// subsystem instance, which takes all of its namespaces down with it.
	ScopeSubsystem
)

func (s Scope) String() string {
	switch s {
	case ScopeController:
		return "controller"
	case ScopeSubsystem:
		return "subsystem"
	default:
		return "none"
	}
}

// Defect is one diagnosed inconsistency, together with everything a caller needs
// to decide whether repairing it is worth what it costs.
type Defect struct {
	// Kind is what is wrong.
	Kind DefectKind
	// NQN is the subsystem the defect concerns.
	NQN string
	// Scope is how much has to be torn down to repair it; ScopeNone means
	// nothing local repairs it.
	Scope Scope

	// Controllers are the controllers a repair tears down, already in teardown
	// order: paths that cannot serve I/O first and the optimized path last, so
	// I/O in flight keeps the best path it has for as long as possible. Empty
	// for a ScopeNone defect.
	Controllers []nvme.Controller

	// CoTenants are the namespaces of *other* volumes that would lose their
	// last usable path if this defect's repair went ahead. It is the blast
	// radius, computed from what is attached now: empty means no volume other
	// than the caller's loses its block device.
	//
	// This is the field that decides whether a repair is allowed to happen
	// unattended. Repairing a volume by ripping the block device out from under
	// an unrelated volume that is serving I/O — the pods on it see ext4 remount
	// read-only — is never an improvement.
	CoTenants []nvme.Namespace

	// Subsystem is the kernel subsystem instance the defect is about. It
	// matters for DefectAmbiguousHead, where two instances share one NQN and
	// only one of them may be torn down.
	Subsystem nvme.SubsystemID

	// Detail describes the specific observation, for logs and events.
	Detail string
}

// Disruptive reports whether repairing this defect takes another volume's block
// device down with it.
func (d Defect) Disruptive() bool {
	return len(d.CoTenants) > 0
}

// Repairable reports whether a local teardown-and-reconnect can fix this defect
// at all. It says nothing about whether doing so is a good idea — that is what
// Disruptive and the Repairer's policy are for.
func (d Defect) Repairable() bool {
	return d.Scope != ScopeNone && len(d.Controllers) > 0
}

func (d Defect) String() string {
	s := fmt.Sprintf("%s %s (scope %s", d.Kind, d.NQN, d.Scope)
	if d.Detail != "" {
		s += ", " + d.Detail
	}
	if n := len(d.CoTenants); n > 0 {
		s += fmt.Sprintf(", %d co-tenant volume(s) at risk", n)
	}
	return s + ")"
}

// Inspect diagnoses the subsystem sel names against the paths targets asks for,
// and returns what is inconsistent. It reads only: nothing is torn down,
// reconnected or otherwise changed, and no NVMe command is issued.
//
// sel must set the NQN, and should set NSID or UUID on a multi-namespace
// subsystem — that is what makes "the namespace I asked for has no path" a
// decidable question rather than a guess about which namespace was meant.
//
// targets is the control plane's current answer, used to tell an endpoint that
// is merely absent from it (DefectStaleEndpoint) from one that is wanted. Pass
// nil to skip that comparison; the namespace- and controller-level checks do not
// need it.
//
// A subsystem that is not attached at all yields no defects: there is nothing
// inconsistent about it, and an ordinary connect is what it needs. The same goes
// for a healthy one, so an empty result is the expected answer on the happy
// path.
//
// Callers that only want to know whether the fabric matches the control plane's
// answer want ReconcilePaths, which counts paths. Inspect answers the harder
// question of why a fabric that looks connected is not serving a device.
func Inspect(
	ctx context.Context,
	subs nvme.SubsystemResolver,
	devs nvme.DeviceResolver,
	sel nvme.DeviceSelector,
	targets []Target,
) ([]Defect, error) {
	if sel.NQN == "" {
		return nil, fmt.Errorf("inspect: selector must set the NQN: %w", errs.ErrUnsupported)
	}

	s, err := subs.ByNQN(ctx, sel.NQN)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", sel.NQN, err)
	}

	defects := inspectSubsystem(s, sel, targets)

	// The duplicate-head check needs a view wider than one subsystem, since
	// ByNQN answers with a single instance and the whole point is that two of
	// them exist. Pass devs as nil to skip it.
	if devs != nil {
		dup, err := inspectDuplicateHeads(ctx, devs, sel)
		if err != nil {
			return defects, fmt.Errorf("inspect %s: %w", sel.NQN, err)
		}
		defects = append(defects, dup...)
	}
	return defects, nil
}

// inspectSubsystem diagnoses one attached subsystem: what it exports, and
// whether its live controllers actually serve the selected namespace.
func inspectSubsystem(s nvme.Subsystem, sel nvme.DeviceSelector, targets []Target) []Defect {
	var defects []Defect

	live := liveControllers(s)
	order := disconnectOrder(s)

	switch matched := matchingNamespaces(s, sel); {
	case len(s.Namespaces) == 0:
		// Zero namespaces and a live controller: the connection came up and
		// exported nothing. Only a teardown makes the kernel re-enumerate.
		if len(live) > 0 {
			defects = append(defects, Defect{
				Kind:        DefectNoNamespace,
				NQN:         s.NQN,
				Scope:       ScopeSubsystem,
				Controllers: order,
				Subsystem:   s.ID,
				// No namespaces means no co-tenant block device exists to lose,
				// so CoTenants is empty by construction — which is exactly why
				// this defect is safe to repair unattended.
				Detail: fmt.Sprintf("%d live controller(s), no namespace exported", len(live)),
			})
		}
	case len(matched) == 0:
		// Namespaces, but not this one. The connection works; the target is not
		// publishing what was asked for. Every namespace present belongs to
		// something else, so a teardown here is pure collateral damage.
		defects = append(defects, Defect{
			Kind:        DefectNamespaceMissing,
			NQN:         s.NQN,
			Scope:       ScopeSubsystem,
			Controllers: order,
			CoTenants:   otherNamespaces(s, nil),
			Subsystem:   s.ID,
			Detail: fmt.Sprintf("selector %s matches none of the %d exported namespace(s)",
				sel, len(s.Namespaces)),
		})
	case len(matched) == 1:
		defects = append(defects, notContributing(s, matched[0], live, targets)...)
	default:
		// Several namespaces of one subsystem match: the selector is
		// under-specified and no reconnect fixes that (WaitForDevice fails on
		// it outright). Saying which controller serves "the" namespace would
		// require guessing which one was meant, so this check stands down.
	}

	defects = append(defects, staleEndpoints(s, targets)...)
	return defects
}

// notContributing reports the live controllers that serve no path to ns.
//
// This is the state a connect cannot see. The controller exists and is live, so
// nothing at the controller level is missing and a connect declines to act,
// while the namespace's own path list — the view that decides whether I/O can
// use this path — does not mention it. The two never reconcile on their own.
//
// A namespace with no path list at all is skipped rather than reported: under
// nvme_core.multipath=0 the kernel publishes no per-controller ANA view, and
// reading that absence as "no controller contributes" would condemn every path
// of a perfectly healthy subsystem.
func notContributing(s nvme.Subsystem, ns nvme.Namespace, live []nvme.Controller, targets []Target) []Defect {
	if len(ns.Paths) == 0 {
		return nil
	}

	serving := make(map[nvme.ControllerID]bool, len(ns.Paths))
	for _, p := range ns.Paths {
		serving[p.Controller] = true
	}

	var defects []Defect
	for _, ctrl := range live {
		if serving[ctrl.ID] {
			continue
		}
		// A controller the control plane no longer publishes is stale, not
		// broken; that is a different defect with a different verdict, and
		// reporting both for one controller would invite two repairs of it.
		if len(targets) > 0 && !matchesAny(ctrl, targets) {
			continue
		}
		defects = append(defects, Defect{
			Kind:        DefectControllerNotContributing,
			NQN:         s.NQN,
			Scope:       ScopeController,
			Controllers: []nvme.Controller{ctrl},
			CoTenants:   losingLastPath(s, ns.ID, map[nvme.ControllerID]bool{ctrl.ID: true}),
			Subsystem:   s.ID,
			Detail: fmt.Sprintf("controller %s at %s is live but serves no path to namespace %d",
				ctrl.ID, endpointOf(ctrl), ns.ID),
		})
	}
	return defects
}

// staleEndpoints reports attached controllers whose endpoint is absent from the
// control plane's current answer. Controllers whose address the kernel does not
// report (PCIe) describe no fabric endpoint and cannot be compared against one.
func staleEndpoints(s nvme.Subsystem, targets []Target) []Defect {
	if len(targets) == 0 {
		return nil
	}
	var defects []Defect
	for _, ctrl := range s.Controllers {
		if ctrl.Address.TrAddr == "" || matchesAny(ctrl, targets) {
			continue
		}
		defects = append(defects, Defect{
			Kind:        DefectStaleEndpoint,
			NQN:         s.NQN,
			Scope:       ScopeController,
			Controllers: []nvme.Controller{ctrl},
			CoTenants:   losingLastPath(s, 0, map[nvme.ControllerID]bool{ctrl.ID: true}),
			Subsystem:   s.ID,
			Detail: fmt.Sprintf("controller %s at %s fronts an endpoint the control plane no longer publishes",
				ctrl.ID, endpointOf(ctrl)),
		})
	}
	return defects
}

// inspectDuplicateHeads reports a selector answered by more than one kernel
// subsystem instance, and names the stale one.
//
// Two instances for one NQN is the kernel not yet having reaped an earlier
// connect. It is dangerous rather than merely untidy: a By* lookup returns
// whichever came first, and handing back the stale namespace means writing to
// the wrong block device. The repairable case is the one where reachability
// separates them — a head whose paths have all gone inaccessible is the stale
// one. When every instance still looks usable the defect is reported at
// ScopeNone: reachability says which devices are serviceable, not which one this
// connect created, and tearing down a guess is how a healthy volume loses its
// data path.
func inspectDuplicateHeads(
	ctx context.Context,
	devs nvme.DeviceResolver,
	sel nvme.DeviceSelector,
) ([]Defect, error) {
	matched, err := devs.ListWithSelector(ctx, sel)
	if err != nil {
		return nil, err
	}

	byInstance := make(map[nvme.SubsystemID][]nvme.Device)
	var instances []nvme.SubsystemID
	for _, d := range matched {
		id := d.Subsystem.ID
		if _, seen := byInstance[id]; !seen {
			instances = append(instances, id)
		}
		byInstance[id] = append(byInstance[id], d)
	}
	if len(instances) < 2 {
		return nil, nil
	}

	var defects []Defect
	for _, id := range instances {
		devices := byInstance[id]
		if slices.ContainsFunc(devices, nvme.Device.Accessible) {
			continue
		}
		defects = append(defects, Defect{
			Kind:        DefectAmbiguousHead,
			NQN:         sel.NQN,
			Scope:       ScopeSubsystem,
			Controllers: disconnectOrder(devices[0].Subsystem),
			// Every namespace of a stale instance is by definition unable to
			// serve I/O, so nothing loses a usable path when it goes.
			Subsystem: id,
			Detail: fmt.Sprintf("%d subsystem instances answer %s; %s can serve no I/O",
				len(instances), sel.NQN, id),
		})
	}
	if len(defects) == 0 {
		// Several instances, none of them demonstrably stale.
		defects = append(defects, Defect{
			Kind:      DefectAmbiguousHead,
			NQN:       sel.NQN,
			Scope:     ScopeNone,
			Subsystem: instances[0],
			Detail: fmt.Sprintf("%d subsystem instances answer %s and all of them look usable; "+
				"which one this connect created cannot be told from reachability", len(instances), sel.NQN),
		})
	}
	return defects, nil
}

// liveControllers narrows s's controllers to the ones in the kernel live state.
func liveControllers(s nvme.Subsystem) []nvme.Controller {
	out := make([]nvme.Controller, 0, len(s.Controllers))
	for _, ctrl := range s.Controllers {
		if ctrl.IsLive() {
			out = append(out, ctrl)
		}
	}
	return out
}

// matchingNamespaces returns the distinct namespaces of s that sel selects.
// Namespaces are deduplicated by NSID: without a multipath head the same
// namespace can appear once per controller, and those repeats are one volume on
// several paths rather than several volumes.
func matchingNamespaces(s nvme.Subsystem, sel nvme.DeviceSelector) []nvme.Namespace {
	var out []nvme.Namespace
	seen := make(map[nvme.NamespaceID]bool, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		if seen[ns.ID] || !sel.Matches(nvme.Device{Namespace: ns, Subsystem: s}) {
			continue
		}
		seen[ns.ID] = true
		out = append(out, ns)
	}
	return out
}

// otherNamespaces returns s's distinct namespaces except the one with nsid.
// Passing 0 excludes nothing, since no namespace has that id.
func otherNamespaces(s nvme.Subsystem, exclude *nvme.NamespaceID) []nvme.Namespace {
	var out []nvme.Namespace
	seen := make(map[nvme.NamespaceID]bool, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		if seen[ns.ID] || (exclude != nil && ns.ID == *exclude) {
			continue
		}
		seen[ns.ID] = true
		out = append(out, ns)
	}
	return out
}

// losingLastPath returns the namespaces other than own that would be left with
// no usable path if every controller in victims were torn down — the blast
// radius of a repair, in the only terms that matter to another volume's pods:
// whether its block device stops serving I/O.
//
// A namespace that still has an accessible path through a surviving controller
// is not counted, which is what makes tearing down one leg of a multipath
// subsystem an ordinary, safe operation even when the subsystem is shared. A
// namespace the kernel publishes no path list for cannot be judged this way, so
// it counts as at risk: on a host without native multipath every namespace of
// the subsystem depends on the controller it hangs off.
//
// Pass own as 0 to count every namespace, which is what a defect about the
// subsystem rather than about one namespace wants.
func losingLastPath(s nvme.Subsystem, own nvme.NamespaceID, victims map[nvme.ControllerID]bool) []nvme.Namespace {
	var exclude *nvme.NamespaceID
	if own != 0 {
		exclude = &own
	}

	var out []nvme.Namespace
	for _, ns := range otherNamespaces(s, exclude) {
		if len(ns.Paths) == 0 || !survivesTeardown(ns, victims) {
			out = append(out, ns)
		}
	}
	return out
}

// survivesTeardown reports whether ns keeps an accessible path through a
// controller that is not being torn down.
func survivesTeardown(ns nvme.Namespace, victims map[nvme.ControllerID]bool) bool {
	for _, p := range ns.Paths {
		if !victims[p.Controller] && p.ANAState.Accessible() {
			return true
		}
	}
	return false
}

// endpointOf renders a controller's fabric endpoint for a message.
func endpointOf(ctrl nvme.Controller) string {
	if ctrl.Address.TrSvcID == "" {
		return ctrl.Address.TrAddr
	}
	return ctrl.Address.TrAddr + ":" + ctrl.Address.TrSvcID
}
