// The operator protecting itself from the drain it is arranging.
//
// The case this is about is narrow and total: the manager pod runs on the
// worker being drained. `kubectl drain` evicts pods in no particular order, so
// it can take the manager out before the manager has created the storage node's
// own budget — and then nothing is left that would have created it, the SPDK
// process is killed under a running backend node, and the control plane sees a
// node that vanished. That is the failure the whole HostMaintenance action
// exists to prevent, reached by evicting the thing preventing it.
//
// design-storagenode.md §10.

package node

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"k8s.io/client-go/tools/events"

	atlaskube "github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	budgetNamespace = "simplyblock"
	budgetCluster   = "a-cluster"
	// managerWorker is where the manager pod runs, and drainedWorker is the one
	// a window is opened on. The interesting case is the two being equal.
	managerWorker = "worker-1"
	otherWorker   = "worker-2"
)

// aWorkload builds a Workload that believes it runs on managerNode.
func aWorkload(t *testing.T, managerNode string, objs ...client.Object) (*Workload, client.Client) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, policyv1.AddToScheme)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Workload{Client: apiClient, ManagerNode: managerNode}, apiClient
}

// managerPod is the operator's own pod, on the worker every case here is
// about, carrying the label its Deployment selects it by.
func managerPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-operator-abc",
			Namespace: budgetNamespace,
			Labels:    map[string]string{"app": managerAppLabel},
		},
		Spec: corev1.PodSpec{NodeName: managerWorker},
	}
}

func selfBudgetOf(t *testing.T, c client.Client) (*policyv1.PodDisruptionBudget, bool) {
	t.Helper()
	var budget policyv1.PodDisruptionBudget
	key := types.NamespacedName{Namespace: budgetNamespace, Name: selfBudgetName}
	err := c.Get(context.Background(), key, &budget)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("reading the self-budget: %v", err)
	}
	return &budget, true
}

// TestTheManagerHoldsItselfOnTheWorkerItIsDraining. Without it, the drain can
// evict the manager before the storage node's budget exists, and the budget is
// then never created by anything.
func TestTheManagerHoldsItselfOnTheWorkerItIsDraining(t *testing.T) {
	w, c := aWorkload(t, managerWorker, managerPod())

	if err := w.ProtectSelf(context.Background(), budgetNamespace, managerWorker); err != nil {
		t.Fatalf("protecting the manager: %v", err)
	}

	budget, found := selfBudgetOf(t, c)
	if !found {
		t.Fatal("no self-budget was created for the worker the manager runs on")
	}
	if budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntVal != 0 {
		t.Errorf("maxUnavailable = %v, want 0: the budget's job is to allow no eviction at all",
			budget.Spec.MaxUnavailable)
	}
	if got := budget.Spec.Selector.MatchLabels["app"]; got != managerAppLabel {
		t.Errorf("the budget selects %q, want the manager's own pods", got)
	}
}

// TestTheManagerDoesNotHoldItselfOnSomebodyElsesWorker. A budget that blocked
// the manager's eviction while an unrelated worker drained would make the
// manager's own node undrainable for the length of every maintenance window in
// the cluster.
func TestTheManagerDoesNotHoldItselfOnSomebodyElsesWorker(t *testing.T) {
	w, c := aWorkload(t, managerWorker, managerPod())

	if err := w.ProtectSelf(context.Background(), budgetNamespace, otherWorker); err != nil {
		t.Fatalf("protecting the manager: %v", err)
	}

	if _, found := selfBudgetOf(t, c); found {
		t.Error("a self-budget was created for a worker the manager does not run on")
	}
}

// TestAManagerThatDoesNotKnowWhereItRunsHoldsNothing. NODE_NAME is set by the
// chart from spec.nodeName, and an operator started without it cannot tell
// whether the worker being drained is its own. Creating the budget anyway would
// block the manager's eviction on every window in the cluster; skipping it
// leaves the case this protects against no worse than it was.
func TestAManagerThatDoesNotKnowWhereItRunsHoldsNothing(t *testing.T) {
	w, c := aWorkload(t, "", managerPod())

	if err := w.ProtectSelf(context.Background(), budgetNamespace, managerWorker); err != nil {
		t.Fatalf("protecting the manager: %v", err)
	}

	if _, found := selfBudgetOf(t, c); found {
		t.Error("a self-budget was created by a manager that does not know which node it is on")
	}
}

// TestReleasingTheManagerIsWhatLetsTheDrainFinish. The manager is on the node
// being drained, so a budget that outlived the window would leave the drain
// blocked forever on the very pod that arranged it.
func TestReleasingTheManagerIsWhatLetsTheDrainFinish(t *testing.T) {
	w, c := aWorkload(t, managerWorker, managerPod())
	ctx := context.Background()

	if err := w.ProtectSelf(ctx, budgetNamespace, managerWorker); err != nil {
		t.Fatal(err)
	}
	if _, found := selfBudgetOf(t, c); !found {
		t.Fatal("the self-budget was not created, so there is nothing to release")
	}

	if err := w.ReleaseSelf(ctx, budgetNamespace); err != nil {
		t.Fatalf("releasing the manager: %v", err)
	}
	if _, found := selfBudgetOf(t, c); found {
		t.Error("the self-budget survived the release, so the drain stays blocked on the manager")
	}

	// Releasing one that is already gone is the state being asked for: the
	// window's terminal step runs it again, and a crash can leave it having run
	// once already.
	if err := w.ReleaseSelf(ctx, budgetNamespace); err != nil {
		t.Errorf("releasing an absent self-budget reported an error: %v", err)
	}
}

// TestAStaleSelfBudgetIsCleanedUp. A manager that crashed between creating the
// budget and releasing it leaves a worker no drain can ever finish, and the
// object that would have removed it is the one that died. The replacement
// manager clears it on the way up.
func TestAStaleSelfBudgetIsCleanedUp(t *testing.T) {
	stale := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: selfBudgetName, Namespace: budgetNamespace},
	}
	w, c := aWorkload(t, managerWorker, managerPod(), stale)

	if err := w.ReleaseSelf(context.Background(), budgetNamespace); err != nil {
		t.Fatalf("clearing the stale self-budget: %v", err)
	}
	if _, found := selfBudgetOf(t, c); found {
		t.Error("the stale self-budget survived, so its worker cannot be drained")
	}
}

// TestTheSelfBudgetCarriesTheGroupsLabels, so that what a `kubectl get pdb`
// shows is identifiable as the operator's and matchable by one rule.
func TestTheSelfBudgetCarriesTheGroupsLabels(t *testing.T) {
	w, c := aWorkload(t, managerWorker, managerPod())

	if err := w.ProtectSelf(context.Background(), budgetNamespace, managerWorker); err != nil {
		t.Fatal(err)
	}

	budget, found := selfBudgetOf(t, c)
	if !found {
		t.Fatal("no self-budget")
	}
	if budget.Labels[atlaskube.LabelApp] == "" {
		t.Errorf("labels = %v, want the group's own", budget.Labels)
	}
}

// unreachableControlPlane fails every read, which is the state a window has to
// hold its own eviction through: the budget's whole job is to exist before
// anything that can go wrong does.
type unreachableControlPlane struct {
	ControlPlane
}

func (unreachableControlPlane) StorageNode(
	context.Context, string, string,
) (NodeReading, bool, error) {
	return NodeReading{}, false, errUnreachableControlPlane
}

var errUnreachableControlPlane = errors.New("the control plane is unreachable")

// TestTheWindowHoldsTheManagerBeforeAnythingThatCanFail. The ordering is the
// mechanism, not a detail of it. A shutdown step that reached the control plane
// first, or created the storage node's budget first, would leave a window in
// which the drain can evict the manager — and the manager is the only thing
// that would have created either.
func TestTheWindowHoldsTheManagerBeforeAnythingThatCanFail(t *testing.T) {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "a-node", Namespace: budgetNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: budgetCluster,
			WorkerNode: managerWorker,
		},
	}
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, policyv1.AddToScheme)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, managerPod()).Build()

	r := &StorageNodeOpsReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(16),
		API:      unreachableControlPlane{},
		Workload: &Workload{Client: apiClient, ManagerNode: managerWorker},
	}

	ops := &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-window", Namespace: budgetNamespace},
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{
			Action:  simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance,
			NodeRef: "a-node",
		},
	}

	// The step cannot finish: the control plane is unreachable. What it must
	// have done first is hold the manager.
	if _, err := r.maintenanceShutDown(
		context.Background(), ops, node, "cluster-id", "node-id"); err == nil {
		t.Fatal("the step reported success against an unreachable control plane")
	}

	if _, found := selfBudgetOf(t, apiClient); !found {
		t.Error("the manager was left evictable while the window could not reach the control plane")
	}
}

// TestTheWindowLetsTheManagerGoWhenItLetsTheStoragePodGo. Both are on the node
// being drained, so holding the manager past the point where the storage pod is
// released would block the drain on the manager instead of on the pod — the
// same deadlock, one object over.
func TestTheWindowLetsTheManagerGoWhenItLetsTheStoragePodGo(t *testing.T) {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "a-node", Namespace: budgetNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: budgetCluster,
			WorkerNode: managerWorker,
		},
	}
	stale := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: selfBudgetName, Namespace: budgetNamespace},
	}
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, policyv1.AddToScheme)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, managerPod(), stale).Build()

	r := &StorageNodeOpsReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(16),
		Workload: &Workload{Client: apiClient, ManagerNode: managerWorker},
	}

	if _, err := r.maintenanceRelease(context.Background(), node); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if _, found := selfBudgetOf(t, apiClient); found {
		t.Error("the manager is still held, so the drain it arranged cannot finish")
	}
}
