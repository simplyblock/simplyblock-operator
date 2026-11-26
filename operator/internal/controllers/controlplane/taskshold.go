// The tasks hold: what keeps the task runner's pod where it is while it is
// adding a storage node.
//
// The runner drives a node add as one long call. A pod replaced in the middle of
// it abandons the add after the node's SPDK pod has been started and before the
// control plane has recorded the node, and the add is started again from the
// beginning. Two things replace the pod without anybody asking for it: the
// cluster-file stamp, which rolls every workload mounting the file each time the
// FoundationDB coordinators move, and a node drain, which evicts it. A
// MachineConfig rollout does both to every worker in turn, so an add running
// through one was restarted once per worker (OpenShift, 2026-11-25: five
// restarts in twenty-one minutes, and the deployment outlived its deadline).
//
// While an add is in flight the stamp the runner already carries is kept, and
// its PodDisruptionBudget allows no disruption. Both are released when the add
// has settled.
//
// A hold that only an add can end is a drain refused for as long as adds keep
// coming, and a fleet's adds follow one another without a gap. So the hold runs
// in turns: tasksHoldLimit held, then tasksHoldRelease open, for as long as an
// add is in flight. A drain waits one turn at most, and the add it interrupts
// in the open window is retried.

package controlplane

import (
	"context"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// tasksHoldSinceAnnotation is on the runner's budget while a node add is in
	// flight, and says when the current run of adds began. The turns are counted
	// from it, which is what bounds the hold without the operator keeping
	// anything in memory.
	tasksHoldSinceAnnotation = "storage.simplyblock.io/node-add-hold-since"

	// tasksHoldLimit is how long the runner is held in one turn. An add takes
	// minutes, and a drain refused for an hour is a MachineConfigPool reporting
	// itself degraded.
	tasksHoldLimit = 20 * time.Minute

	// tasksHoldRelease is how long the hold then stays open before the next
	// turn. A drain that has been failing retries every five minutes and gives
	// each attempt a minute and a half, so the window has to be longer than
	// both together for the drain to be sure of landing in it.
	tasksHoldRelease = 7 * time.Minute
)

// tasksHold is the state of the hold on one pass.
type tasksHold struct {
	// since is when the current run of node adds began holding, and nil when no
	// add is in flight.
	since *time.Time
	// held is whether the runner is to be left alone on this pass.
	held bool
}

// tasksHoldNow reads whether a node add is in flight and which turn of the hold
// the runner in the given namespace is in.
func (r *ControlPlaneReconciler) tasksHoldNow(ctx context.Context, namespace string) (tasksHold, error) {
	inFlight, err := r.nodeAddInFlight(ctx)
	if err != nil || !inFlight {
		return tasksHold{}, err
	}

	now := r.clock()
	since := now
	var live policyv1.PodDisruptionBudget
	err = r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ComponentTasks}, &live)
	if client.IgnoreNotFound(err) != nil {
		return tasksHold{}, err
	}
	if err == nil {
		// An annotation that does not parse is one nobody can have written on
		// purpose, and starting the hold again is the reading that releases it.
		if began, perr := time.Parse(time.RFC3339, live.Annotations[tasksHoldSinceAnnotation]); perr == nil {
			since = began
		}
	}
	return tasksHold{since: &since, held: heldAt(now.Sub(since))}, nil
}

// heldAt reports whether the hold is on, that long into a run of adds. A clock
// that went backwards reads as the start of the run.
func heldAt(into time.Duration) bool {
	if into < 0 {
		return true
	}
	return into%(tasksHoldLimit+tasksHoldRelease) < tasksHoldLimit
}

// applyTasksBudget writes the task runner's budget by itself, ahead of
// everything else a pass applies. Releasing the hold must not depend on the rest
// of the pass going through: a budget left at no disruption by a pass that
// failed further on is a drain refused with nothing counting its turn.
func (r *ControlPlaneReconciler) applyTasksBudget(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	hold, err := r.tasksHoldNow(ctx, cp.Namespace)
	if err != nil {
		return err
	}
	return applyOne(ctx, r.Client, cp, r.Scheme, tasksDisruptionBudget(cp.Namespace, hold))
}

// nodeAddInFlight reports whether the control plane is adding a storage node
// (StorageNode.AddInFlight). The nodes of every namespace count: a
// StorageCluster in any namespace belongs to this installation, and its adds
// are driven by the one task runner.
func (r *ControlPlaneReconciler) nodeAddInFlight(ctx context.Context) (bool, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return false, err
	}
	for i := range nodes.Items {
		if nodes.Items[i].AddInFlight() {
			return true, nil
		}
	}
	return false, nil
}

// tasksDisruptionBudget is the task runner's budget. It always exists, so that
// raising the hold is a change to one field rather than a create racing the
// eviction it is there to refuse: no disruption while the hold is on, and one
// otherwise, which for a single replica is no budget at all.
func tasksDisruptionBudget(namespace string, hold tasksHold) *policyv1.PodDisruptionBudget {
	budget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: ComponentTasks, Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: ComponentTasks}},
			// A runner that is not ready is adding nothing, so the hold has
			// nothing to protect and must not keep it on a host being drained.
			UnhealthyPodEvictionPolicy: ptr.To(policyv1.AlwaysAllow),
		},
	}
	if hold.since != nil {
		budget.Annotations = map[string]string{
			tasksHoldSinceAnnotation: hold.since.UTC().Format(time.RFC3339),
		}
	}
	if hold.held {
		budget.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(0))
	}
	return budget
}

// clock is the time the hold is measured against.
func (r *ControlPlaneReconciler) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// storageNodeRequests maps a StorageNode, in whichever namespace, to the
// ControlPlane that holds the install, so the hold is raised when an add is
// asked for and released when it settles rather than a steady-state pass later.
func (r *ControlPlaneReconciler) storageNodeRequests(ctx context.Context, _ client.Object) []reconcile.Request {
	holder, err := r.deploymentHolder(ctx)
	if err != nil || holder.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: holder}}
}

// nodeAddStepChanged passes the StorageNode events that can change the hold:
// a node's add beginning or ending. A node's status is written on every pass of
// its own controller, and none of those writes is a reason to reconcile the
// control plane.
func nodeAddStepChanged() predicate.Predicate {
	inStep := func(obj client.Object) bool {
		node, ok := obj.(*simplyblockv1alpha2.StorageNode)
		return ok && node.AddInFlight()
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return inStep(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return inStep(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return inStep(e.ObjectOld) != inStep(e.ObjectNew) },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
