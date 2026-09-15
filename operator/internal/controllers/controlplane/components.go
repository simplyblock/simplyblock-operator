// The components a managed control plane consists of, and the phase derived
// from them.
//
// design-controlplane.md §4.3 keeps this table in the operator rather than in
// the API, because a user cannot add a component to a managed control plane and
// so has nothing to configure. A spec field here would exist only to let
// somebody mark the management API non-essential, which turns an outage into a
// warning without changing the outage.
//
// # Essential means the work stops, not that it slows down
//
// A component is essential when its absence loses work or stops the control
// plane answering. The task runner is the case that draws the line: its work is
// queued, so a runner at zero defers what is waiting rather than dropping it,
// and the queue is still there when it comes back. That is a control plane doing
// less than it should while remaining correct, which is what Degraded is for.
//
// Only a component marked essential can produce Unavailable, and that asymmetry
// is the safety property: Unavailable holds every controller in the operator, so
// the set of things able to cause it has to be a closed list somebody reviewed.
// A component added to the install without a decision about it lands in the
// default, which is that it can reach Degraded and cannot reach Unavailable. The
// failure that costs is halting a fleet over an exporter, not reporting a
// warning about one.

package controlplane

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// componentKind is how a component's readiness is read.
type componentKind int

const (
	// kindDeployment and kindStatefulSet are a ready count against a desired
	// count, read off the workload's own status.
	kindDeployment componentKind = iota
	kindStatefulSet

	// kindFoundationDB is the resource's own report. A FoundationDBCluster at
	// two of three coordinators is serving, and a replica count cannot say so.
	kindFoundationDB
)

// component is one workload of a managed control plane.
type component struct {
	// name is the object's name, which is also what status.components publishes
	// and what a Restart operation names.
	name string

	// kind is how its readiness is read.
	kind componentKind

	// essential is whether this component at zero ready makes the control plane
	// Unavailable rather than Degraded.
	essential bool

	// why records the decision behind essential, for the component where it is
	// not self-evident. It is read by nothing and exists so the table explains
	// itself where it is edited.
	why string
}

// componentTable is every workload the managed install applies, in the order
// status.components reports them: the database first, then what serves, then
// what supports it.
//
// It is exactly the set the install applies, which is what makes it watchable:
// a component the operator does not own is one it cannot report a count for, and
// the observability half the chart still renders is therefore absent from here
// rather than reported as missing.
var componentTable = []component{
	{
		name: ComponentFDBCluster, kind: kindFoundationDB, essential: true,
		why: "the control plane's entire state is in it; a database that is not " +
			"serving is a control plane that answers nothing",
	},
	{
		name: ComponentWebAPI, kind: kindDeployment, essential: true,
		why: "it is the endpoint every controller in this operator calls",
	},
	{
		name: ComponentFDBOperator, kind: kindDeployment, essential: false,
		why: "a database already running keeps running without its operator; " +
			"what stops is reconfiguring it and replacing a failed process group",
	},
	{
		name: ComponentTasks, kind: kindDeployment, essential: false,
		why: "its work is queued, so a runner at zero defers what is waiting " +
			"rather than dropping it",
	},
	{
		name: ComponentMonitoring, kind: kindDeployment, essential: false,
		why: "the fleet keeps serving while nothing is watching it, and what is " +
			"lost is the record of what happened rather than the work",
	},
	{
		name: ComponentAdminControl, kind: kindDeployment, essential: false,
		why: "it runs nothing; it exists to be exec'd into",
	},
	{
		name: ComponentMinio, kind: kindStatefulSet, essential: false,
		why: "design-controlplane.md §12 Q5 leaves this open, and non-essential " +
			"is the default a component takes until somebody decides: nothing in " +
			"the install path or the data path reads the store",
	},
	{
		name: ComponentFDBExporter, kind: kindDeployment, essential: false,
		why: "it publishes metrics about a database that is serving either way",
	},
}

// observe reads every component's counts back. A workload that is not there yet
// is not an error: the apply created it and the cache has not caught up, which
// reads as zero ready against zero desired.
func observe(
	ctx context.Context, c client.Reader, namespace string,
) ([]simplyblockv1alpha2.ControlPlaneComponentStatus, error) {
	out := make([]simplyblockv1alpha2.ControlPlaneComponentStatus, 0, len(componentTable))

	for _, comp := range componentTable {
		status := simplyblockv1alpha2.ControlPlaneComponentStatus{
			Name:      comp.name,
			Essential: comp.essential,
		}

		switch comp.kind {
		case kindDeployment:
			var d appsv1.Deployment
			err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: comp.name}, &d)
			switch {
			case errors.IsNotFound(err):
			case err != nil:
				return nil, fmt.Errorf("read Deployment %s: %w", comp.name, err)
			default:
				status.Desired = desiredReplicas(d.Spec.Replicas)
				status.Ready = d.Status.ReadyReplicas
			}

		case kindStatefulSet:
			var s appsv1.StatefulSet
			err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: comp.name}, &s)
			switch {
			case errors.IsNotFound(err):
			case err != nil:
				return nil, fmt.Errorf("read StatefulSet %s: %w", comp.name, err)
			default:
				status.Desired = desiredReplicas(s.Spec.Replicas)
				status.Ready = s.Status.ReadyReplicas
			}

		case kindFoundationDB:
			health, err := readFoundationDB(ctx, c, namespace)
			if err != nil {
				return nil, err
			}
			// The database reports whether it is serving, not how many of its
			// processes are. Publishing its process groups as the two counts
			// keeps the field meaning the same thing for every component, and
			// the health is folded in by reporting zero ready when the cluster
			// is not available at all — which is the state that makes this
			// component's verdict Unavailable.
			status.Desired = health.desired
			status.Ready = health.reconciled
			if !health.available {
				status.Ready = 0
			}
		}

		out = append(out, status)
	}
	return out, nil
}

// desiredReplicas is a workload's replica count. A nil pointer is Kubernetes's
// default of one rather than zero, which matters because zero desired is what
// this package reads as not there yet.
func desiredReplicas(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// verdict is what one signal says about the control plane. The values are
// ordered by severity so that the phase is the maximum across every signal.
type verdict int

const (
	verdictAvailable verdict = iota
	verdictDegraded
	verdictUnavailable
)

// componentVerdict is what one component's counts say.
//
// A component below its desired count while above zero is Degraded whether or
// not it is essential: the management API at one of two replicas is still
// answering every request, which is exactly the window Degraded exists to name.
// Zero ready is where the two diverge.
func componentVerdict(status simplyblockv1alpha2.ControlPlaneComponentStatus) verdict {
	switch {
	case status.Desired == 0:
		// Nothing is asked for, so nothing is missing. This is the workload the
		// apply just created and the cache has not caught up with.
		return verdictAvailable
	case status.Ready == 0 && status.Essential:
		return verdictUnavailable
	case status.Ready == 0:
		return verdictDegraded
	case status.Ready < status.Desired:
		return verdictDegraded
	default:
		return verdictAvailable
	}
}

// derivePhase is the worst verdict among the readiness probe and every
// component, together with the sentence explaining it.
//
// The probe answers whether the control plane responds. The components answer
// whether it is one restart away from not responding. A probe that fails settles
// the phase on its own, because a control plane that does not answer is
// Unavailable whatever its pod counts say.
func derivePhase(
	probeOK bool, probeError string,
	components []simplyblockv1alpha2.ControlPlaneComponentStatus,
) (simplyblockv1alpha2.ControlPlanePhase, string) {
	if !probeOK {
		return simplyblockv1alpha2.ControlPlanePhaseUnavailable, probeError
	}

	worst := verdictAvailable
	reason := ""
	for _, status := range components {
		v := componentVerdict(status)
		if v > worst {
			worst = v
			reason = fmt.Sprintf("%s has %d of %d replicas ready",
				status.Name, status.Ready, status.Desired)
		}
	}

	switch worst {
	case verdictUnavailable:
		return simplyblockv1alpha2.ControlPlanePhaseUnavailable, reason
	case verdictDegraded:
		return simplyblockv1alpha2.ControlPlanePhaseDegraded, reason
	default:
		return simplyblockv1alpha2.ControlPlanePhaseAvailable, ""
	}
}

// essentialComponents is the set a scoped Restart has to drain before recycling.
// It is derived from the table rather than restated, so a component whose
// classification changes moves both at once.
func essentialComponents() map[string]bool {
	out := make(map[string]bool, len(componentTable))
	for _, comp := range componentTable {
		if comp.essential {
			out[comp.name] = true
		}
	}
	return out
}

// knownComponent reports whether a name is one this table has, which is what a
// Restart naming a workload that does not exist is refused on.
func knownComponent(name string) bool {
	for _, comp := range componentTable {
		if comp.name == name {
			return true
		}
	}
	return false
}

// restartableComponents are the workloads a Restart can roll: everything in the
// table that is a Deployment or a StatefulSet. The FoundationDBCluster is not
// one of them, because recycling a database is the FoundationDB operator's
// mechanism and not a pod-template annotation.
func restartableComponents() []component {
	out := make([]component, 0, len(componentTable))
	for _, comp := range componentTable {
		if comp.kind != kindFoundationDB {
			out = append(out, comp)
		}
	}
	return out
}
