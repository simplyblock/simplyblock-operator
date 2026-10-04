// The operator protecting itself from the drain it is arranging.
//
// A maintenance window blocks the eviction of the storage-node pod before it
// takes the backend node down, which is what makes `kubectl drain` wait rather
// than kill an SPDK process out from under a running node (§10). That protects
// the storage pod and nothing else, and on a converged worker the manager runs
// on the same host.
//
// The drain evicts in no particular order. So it can take the manager out
// before the manager has created the storage node's budget, and then nothing is
// left that would ever create it: the storage pod goes next, the SPDK process
// dies under a running backend node, and the control plane sees a node that
// vanished. That is precisely the failure the window exists to prevent, reached
// by evicting the thing preventing it.
//
// The answer is a budget the manager holds over itself for the length of the
// arrangement and no longer. It is created before the storage node's, and it is
// released in the same step that releases the storage node's — because the
// manager is on the node being drained, and a budget that outlived the window
// would leave the drain blocked forever on the pod that arranged it.
//
// design-storagenode.md §10 is the specification.

package node

import (
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	atlaskube "github.com/simplyblock/atlas/kube"
)

const (
	// managerAppLabel is what the operator's Deployment labels its pods with,
	// and therefore what a budget over the manager selects.
	//
	// The label is used rather than a marker of this package's own, because
	// labeling the manager's own pod to protect it is a write that has to
	// succeed before the protection exists — and the window it protects is
	// exactly the one where the manager may be evicted mid-write. What the
	// Deployment already stamps is there before the operator starts.
	managerAppLabel = "simplyblock-operator"

	// selfBudgetName is fixed rather than derived, because there is one
	// manager: it holds the leader lease, so only one instance ever arranges a
	// window, and a second name could only ever be a second copy of the same
	// budget. A fixed name is also what lets a replacement manager find and
	// clear the one its predecessor left.
	selfBudgetName = "sb-maintenance-operator"
)

// ProtectSelf blocks the manager's own eviction, for the case where the worker
// being drained is the one it runs on.
//
// It is a no-op otherwise, and that is the important half: a budget that held
// the manager while an unrelated worker drained would make the manager's own
// node undrainable for the length of every maintenance window in the cluster.
//
// A manager that does not know which node it is on holds nothing. The chart
// sets NODE_NAME from spec.nodeName, and an operator started without it cannot
// tell whose worker this is — creating the budget anyway would block its own
// eviction on every window, and skipping it leaves the case no worse than it
// was before this existed.
func (w *Workload) ProtectSelf(ctx context.Context, namespace, worker string) error {
	if w.ManagerNode == "" || w.ManagerNode != worker {
		return nil
	}

	budget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      selfBudgetName,
			Namespace: namespace,
			Labels: map[string]string{
				atlaskube.LabelApp: managerAppLabel,
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			// Zero rather than one: Kubernetes reads a budget allowing no
			// disruption as a refusal of every voluntary eviction, which is
			// what holds the drain rather than merely slowing it.
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": managerAppLabel},
			},
		},
	}

	err := w.Create(ctx, budget)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("hold the manager's own eviction on worker %s: %w", worker, err)
	}
	if err == nil {
		logf.FromContext(ctx).Info("the manager is holding its own eviction while it arranges "+
			"the maintenance of the worker it runs on", "worker", worker)
	}
	return nil
}

// ReleaseSelf lets the manager be evicted again, and clears a budget a previous
// manager left behind.
//
// The two are one operation because they cannot be told apart from here: a
// budget that exists either belongs to the window this is ending or belongs to
// a manager that died holding it, and in both cases the answer is that it goes.
// Leaving one of the second kind would make its worker undrainable forever, by
// an object whose owner no longer exists.
func (w *Workload) ReleaseSelf(ctx context.Context, namespace string) error {
	budget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name:      selfBudgetName,
		Namespace: namespace,
	}}
	if err := w.Delete(ctx, budget); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release the manager's own eviction: %w", err)
	}
	return nil
}

// ClearStaleSelfBudget removes a self-budget left by a manager that crashed
// while holding one.
//
// It runs once, on the instance that won the leader election, and that is the
// only place it can safely run: only the leader arranges a window, so only the
// leader's budget is ever live, and a replica clearing one on the way up would
// be clearing the leader's.
func (w *Workload) ClearStaleSelfBudget(ctx context.Context, namespace string) error {
	var budget policyv1.PodDisruptionBudget
	key := client.ObjectKey{Namespace: namespace, Name: selfBudgetName}
	err := w.Get(ctx, key, &budget)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look for a self-budget left by a previous manager: %w", err)
	}

	logf.FromContext(ctx).Info("clearing a maintenance budget the previous manager left over "+
		"itself, which would otherwise make its worker undrainable", "budget", selfBudgetName)
	return w.ReleaseSelf(ctx, namespace)
}
