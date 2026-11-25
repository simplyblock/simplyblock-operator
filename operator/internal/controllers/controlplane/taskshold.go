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
// has settled, and after tasksHoldLimit whether it has or not, so that an add
// that never settles does not hold a drain for good.

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
	// flight, and says when the hold began. It is what bounds the hold without
	// the operator keeping anything in memory.
	tasksHoldSinceAnnotation = "storage.simplyblock.io/node-add-hold-since"

	// tasksHoldLimit is how long one unbroken run of node adds may hold the
	// runner. An add takes minutes; a fleet's adds follow one another without a
	// gap, and a drain held for all of them is a MachineConfigPool reporting
	// itself degraded. Past the limit the drain wins, and the add it interrupts
	// is retried.
	tasksHoldLimit = 20 * time.Minute
)

// tasksHold is the state of the hold on one pass.
type tasksHold struct {
	// since is when the current run of node adds began holding, and nil when no
	// add is in flight.
	since *time.Time
	// held is whether the runner is to be left alone on this pass.
	held bool
}

// tasksHoldNow reads whether a node add is in flight in the namespace and how
// long the runner has been held for it.
func (r *ControlPlaneReconciler) tasksHoldNow(ctx context.Context, namespace string) (tasksHold, error) {
	inFlight, err := r.nodeAddInFlight(ctx, namespace)
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
	return tasksHold{since: &since, held: now.Sub(since) < tasksHoldLimit}, nil
}

// nodeAddInFlight reports whether the control plane has been asked to add a
// storage node and has not answered yet. Posting and Resolving are the two
// steps between the request and the node: a node waiting for its slot has asked
// for nothing, and one whose worker is away has left both for AwaitingWorker, so
// the hold never keeps a drain off the worker an add is waiting on.
func (r *ControlPlaneReconciler) nodeAddInFlight(ctx context.Context, namespace string) (bool, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range nodes.Items {
		if nodeAddStep(&nodes.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}

// nodeAddStep reports whether the node is in a step the task runner is working on.
func nodeAddStep(node *simplyblockv1alpha2.StorageNode) bool {
	if !node.DeletionTimestamp.IsZero() {
		return false
	}
	switch simplyblockv1alpha2.StorageNodeStep(node.Status.Step.State) {
	case simplyblockv1alpha2.StorageNodeStepPosting, simplyblockv1alpha2.StorageNodeStepResolving:
		return true
	}
	return false
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

// storageNodeRequests maps a StorageNode to the ControlPlanes in its namespace,
// so the hold is raised when an add is asked for and released when it settles
// rather than a steady-state pass later.
func (r *ControlPlaneReconciler) storageNodeRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	var planes simplyblockv1alpha2.ControlPlaneList
	if err := r.List(ctx, &planes, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(planes.Items))
	for i := range planes.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&planes.Items[i])})
	}
	return requests
}

// nodeAddStepChanged passes the StorageNode events that can change the hold:
// a node entering or leaving the steps the runner works on. A node's status is
// written on every pass of its own controller, and none of those writes is a
// reason to reconcile the control plane.
func nodeAddStepChanged() predicate.Predicate {
	inStep := func(obj client.Object) bool {
		node, ok := obj.(*simplyblockv1alpha2.StorageNode)
		return ok && nodeAddStep(node)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return inStep(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return inStep(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return inStep(e.ObjectOld) != inStep(e.ObjectNew) },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
