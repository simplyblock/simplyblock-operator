// The StorageNode reconciler: it owns one backend node's own lifecycle, and
// nothing else. Every imperative operation belongs to StorageNodeOps, and the
// Kubernetes workload the node runs in belongs to the cluster.
//
// Four paths leave this reconcile, and status.uuid is what chooses between them:
// empty means the provisioning machine of §4.2 is running, and non-empty means
// steady-state synchronization against the storage-node stream. Deletion and
// adoption are the other two.
//
// Provisioning is a persisted machine rather than a sequence of nullable fields
// because adding a backend node is not idempotent, and the call adds every socket
// of the worker at once rather than one node. Two StorageNode objects for the same
// worker that both observe an empty status.uuid would add the worker twice. The
// claim is therefore made in Kubernetes before the control plane is touched: the
// transition into Posting is an optimistic-lock patch, which succeeds for exactly
// one reconciler at a given resourceVersion and returns 409 to the rest. That
// replaces both status.postedAt and the List over sibling objects that stood in
// for it.
//
// design-storagenode.md §4 is the specification.

package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// NodeFinalizer holds the object while its backend node is drained. A node
	// that is online has data on it, and deleting the object must not delete the
	// node underneath without moving that data first (§4.5).
	NodeFinalizer = "storage.simplyblock.io/storagenode-finalizer"

	// nodeRetry is the slow backstop. Steady state arrives on the storage-node
	// stream, so this is what covers a stream nothing has yet noticed is dead,
	// and the interval at which a held provisioning step looks again.
	nodeRetry = 30 * time.Second

	// nodeAdvance is how long a pass that moved the machine forward waits before
	// the next one. The status write this pass made is itself a change the
	// controller watches, so this is the backstop for the event rather than the
	// path the next step normally arrives on.
	nodeAdvance = time.Second

	// clusterRefField indexes nodes by the cluster they belong to, so a cluster
	// event wakes its nodes rather than every node in the deployment.
	clusterRefField = "spec.clusterRef"
)

// StorageNodeReconciler reconciles a StorageNode.
type StorageNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      ControlPlane

	// Nodes is the storage-node stream's cache, which steady state is read from
	// and which adoption matches against. It is optional: a deployment without the
	// control-plane informer falls back to reading the control plane directly.
	Nodes NodeCache

	// Registries are told which Kubernetes object a backend node id belongs to,
	// once this reconcile has resolved one. The control plane knows nothing of
	// object names, so a stream cannot enqueue a reconcile for a node it has never
	// been told about.
	Registries []NodeObjectRegistry

	// DeviceScopes is the device stream's scope set. A device stream is per node
	// rather than per cluster, so a scope is opened when a node resolves its UUID
	// and closed when the node goes: nothing else knows a node exists to stream
	// the devices of.
	DeviceScopes *cpinformer.ScopeSet

	// Capacity is where a node's occupancy is read from. Neither the node list nor
	// the node stream carries how full a node is; the numbers exist only in the
	// metrics the control plane exports (§12).
	Capacity NodeCapacitySource

	// Workload is the storage-plane side: the worker labels, the per-node
	// configuration, and the probe that asks whether a worker's storage-node API
	// answers.
	Workload *Workload
}

// NodeObjectRegistry is told the Kubernetes object behind a backend node id.
type NodeObjectRegistry interface {
	RegisterNode(nodeID string, object types.NamespacedName)
	UnregisterNode(nodeID string)
}

// NodeCapacitySource supplies a node's occupancy, satisfied by atlas-lib's
// prometheus.Provider.
type NodeCapacitySource interface {
	NodeCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// SetupWithManager registers the controller.
//
// It watches three things beside its own kind. A StorageCluster event wakes its
// nodes, because the cluster's UUID is what provisioning waits for. A Kubernetes
// Node event is how a cordon is seen, which is what raises a maintenance window
// (§10). And the storage-node stream's triggers are how steady state arrives at
// all.
func (r *StorageNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&simplyblockv1alpha2.StorageNode{}, clusterRefField,
		func(object client.Object) []string {
			node, ok := object.(*simplyblockv1alpha2.StorageNode)
			if !ok {
				return nil
			}
			return []string{node.Spec.ClusterRef}
		})
	if err != nil {
		return fmt.Errorf("index nodes by their cluster: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageNode{}).
		Named("storagenode").
		Watches(&simplyblockv1alpha2.StorageCluster{},
			handler.EnqueueRequestsFromMapFunc(r.nodesOf)).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodesOn)).
		Complete(r)
}

// nodesOf enqueues every node of a cluster.
func (r *StorageNodeReconciler) nodesOf(
	ctx context.Context, cluster client.Object,
) []reconcile.Request {
	var nodes simplyblockv1alpha2.StorageNodeList
	err := r.List(ctx, &nodes,
		client.InNamespace(cluster.GetNamespace()),
		client.MatchingFields{clusterRefField: cluster.GetName()})
	if err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(nodes.Items))
	for i := range nodes.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&nodes.Items[i]),
		})
	}
	return requests
}

// nodesOn enqueues every storage node running on one Kubernetes worker, which is
// how a cordon reaches the nodes it is about.
func (r *StorageNodeReconciler) nodesOn(
	ctx context.Context, worker client.Object,
) []reconcile.Request {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range nodes.Items {
		if nodes.Items[i].Spec.WorkerNode != worker.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&nodes.Items[i]),
		})
	}
	return requests
}

func (r *StorageNodeReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var node simplyblockv1alpha2.StorageNode
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cluster, err := r.cluster(ctx, &node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !node.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &node)
	}

	if !controllerutil.ContainsFinalizer(&node, NodeFinalizer) {
		controllerutil.AddFinalizer(&node, NodeFinalizer)
		return ctrl.Result{}, r.Update(ctx, &node)
	}

	// The cluster owns the node, which is what makes a cluster delete cascade to
	// its nodes (§3.1). It is established here rather than by whatever created the
	// object, so a node written by hand joins the spine too.
	if err := r.adopt(ctx, &node, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// A pod the scheduler has refused holds a provisioning node in CheckingHost
	// and takes a running one offline, and neither path can name the cause from
	// what the control plane reports. It is announced before the branch because
	// it is the same answer on both sides of it.
	r.reportPodScheduling(ctx, &node)

	if node.Status.UUID == "" {
		return r.provision(ctx, &node, cluster)
	}

	// A cordoned worker takes its storage-node pod with it, so the node is taken
	// down deliberately rather than killed underneath a running SPDK process. The
	// window is raised before the status sync, because a node whose host is going
	// away should not first be reported healthy.
	if raised, err := r.raiseMaintenance(ctx, &node); err != nil || raised {
		return ctrl.Result{RequeueAfter: nodeAdvance}, err
	}

	return r.syncStatus(ctx, &node, cluster)
}

// cluster resolves the node's parent. A node whose cluster does not exist is held
// rather than failed: admission refuses such a node on create (§3.4), so one seen
// here is a cluster deleted out from under a node that outlived it.
func (r *StorageNodeReconciler) cluster(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Name: node.Spec.ClusterRef, Namespace: node.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &cluster, nil
}

// adopt establishes the cluster as the node's controller owner, which is both the
// ownership spine and what the conversion reads spec.clusterRef back from.
func (r *StorageNodeReconciler) adopt(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) error {
	if cluster == nil || metav1.IsControlledBy(node, cluster) {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if err := controllerutil.SetControllerReference(cluster, node, r.Scheme); err != nil {
		return fmt.Errorf("own node %s by cluster %s: %w", node.Name, cluster.Name, err)
	}
	return r.Patch(ctx, node, patch)
}

// provision runs the machine of §4.2 forward by at most one step.
func (r *StorageNodeReconciler) provision(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	if cluster == nil || cluster.Status.UUID == "" {
		r.emit(node, corev1.EventTypeNormal, ClusterNotReady,
			"The cluster has no UUID yet, so there is nothing to add this node to")
		return ctrl.Result{RequeueAfter: nodeRetry}, r.hold(ctx, node,
			"waiting for the cluster to be created in the control plane")
	}

	machine, err := statemachine.NewFromSnapshot(ctx, provisioningGraph(),
		statemachine.FromKube[nodeStep](node.Status.Step))
	if err != nil {
		// An unrecognized step is a downgrade, a hand-edited object, or a rename
		// that shipped without a conversion, and none of them resolve by
		// reconciling again.
		return ctrl.Result{}, r.fail(ctx, node,
			fmt.Sprintf("provisioning cannot be resumed: %v", err))
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry hook
	// never runs and no deadline is set for it. Setting one on the first pass is
	// what stops the first step being the one step that cannot time out.
	if node.Status.Step.State == "" {
		deadline := metav1.NewTime(time.Now().Add(checkingHostDeadline))
		return ctrl.Result{RequeueAfter: nodeAdvance},
			r.recordStep(ctx, node, machine.CurrentState(), &deadline)
	}

	current := machine.CurrentState()

	// A worker that is not there is checked before the deadline, because a step
	// waiting on it cannot make progress and spending its budget on a machine
	// that is rebooting would fail the node for the reboot.
	//
	// Only the two steps that have claimed the worker divert. The claim is what
	// AwaitingWorker has to carry: it counts as claimed (claimedWorker), so the
	// cluster's node-add cap keeps holding while the machine is away and no
	// second worker is handed a configuration change on top of a reboot already
	// running. A step that had not claimed anything cannot be held the same way,
	// because a claimed sibling on the same worker is read as an add that already
	// happened and a reason to skip Posting, and a node diverted out of
	// AwaitingSlot never posted. Those steps wait where they are instead: AwaitingSlot declines to
	// take a slot while the worker is away, and the host check simply does not
	// answer.
	if current == stepPosting || current == stepResolving {
		away, reason, err := r.workerAway(ctx, node.Spec.WorkerNode)
		if err != nil {
			return ctrl.Result{RequeueAfter: nodeRetry}, err
		}
		if away {
			r.emit(node, corev1.EventTypeWarning, WorkerAway, fmt.Sprintf(
				"Worker %s %s, so %s is held until it is back", node.Spec.WorkerNode, reason, current))
			if err := machine.TransitionTo(ctx, stepAwaitingWorker); err != nil {
				return ctrl.Result{}, fmt.Errorf("enter step %s: %w", stepAwaitingWorker, err)
			}
			snapshot := statemachine.ToKube(machine.Snapshot())
			return ctrl.Result{RequeueAfter: nodeRetry},
				r.recordStep(ctx, node, stepAwaitingWorker, snapshot.Deadline)
		}
	}

	if machine.TimeoutReached() {
		return ctrl.Result{}, r.fail(ctx, node,
			fmt.Sprintf("step %s outlived its deadline", current))
	}

	next, done, err := r.performNodeStep(ctx, node, cluster, current)
	if err != nil {
		var blocked *blockedStepError
		if errors.As(err, &blocked) {
			r.emit(node, corev1.EventTypeWarning, blocked.reason, blocked.message)
			return ctrl.Result{RequeueAfter: nodeRetry}, r.hold(ctx, node, blocked.message)
		}
		logf.FromContext(ctx).Error(err, "the provisioning step could not be advanced",
			"node", node.Name, "step", current)
		return ctrl.Result{RequeueAfter: nodeRetry}, r.hold(ctx, node, err.Error())
	}
	if !done {
		return ctrl.Result{RequeueAfter: nodeRetry}, r.hold(ctx, node,
			fmt.Sprintf("waiting on %s", current))
	}

	if next == current {
		// Resolving and Adopting both end with a UUID on the object, which the
		// step that reached them has already written, and both report themselves
		// as their own successor. The next pass is steady state.
		//
		// The test is the step rather than machine.IsTerminal, which answers
		// whether the graph gives the state an exit: Resolving has one now, to
		// the step that holds while the worker is away, and a finished Resolving
		// is still the end of the path.
		//
		// The add is over, so the slot it was taken for goes back. This is the
		// successful one of the three ends, and the other two are the deadline in
		// fail and the object going away in teardown.
		return ctrl.Result{RequeueAfter: nodeAdvance}, r.releaseSlot(ctx, node)
	}

	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: nodeAdvance},
		r.recordStep(ctx, node, next, snapshot.Deadline)
}

// performNodeStep runs one step and reports the step that follows it and whether
// this one has finished.
//
// The next step is returned rather than read off the graph's single edge, because
// two of the six branch: CheckingHost and CheckingConfig both divert to Adopting,
// and AwaitingSlot skips Posting when a sibling socket has already claimed the
// worker.
func (r *StorageNodeReconciler) performNodeStep(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
	current nodeStep,
) (nodeStep, bool, error) {
	switch current {
	case stepCheckingHost:
		return r.checkHost(ctx, node, cluster)
	case stepCheckingConfig:
		return r.checkConfig(ctx, node, cluster)
	case stepAwaitingSlot:
		return r.awaitSlot(ctx, node, cluster)
	case stepPosting:
		return stepResolving, true, r.postNode(ctx, node, cluster)
	case stepResolving:
		return r.resolve(ctx, node, cluster)
	case stepAwaitingWorker:
		return r.awaitWorker(ctx, node)
	case stepAdopting:
		done, err := r.resolveUUID(ctx, node, cluster)
		return stepAdopting, done, err
	default:
		return current, false, fmt.Errorf("step %s belongs to no provisioning path", current)
	}
}

// awaitWorker holds while the worker is away and starts the path again when it
// is back.
//
// It returns to CheckingHost rather than to the step it was diverted from,
// because what a reboot interrupts is not resumable in the middle: a Posting
// resumed after the fact would ask for a second node, and the add it was waiting
// on may well have landed. CheckingHost is where a backend node that already
// exists is found, and its Adopting edge is what takes the node over instead of
// adding it twice.
func (r *StorageNodeReconciler) awaitWorker(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (nodeStep, bool, error) {
	away, reason, err := r.workerAway(ctx, node.Spec.WorkerNode)
	if err != nil {
		return stepAwaitingWorker, false, err
	}
	if away {
		return stepAwaitingWorker, false, blockedf(WorkerAway,
			"worker %s %s", node.Spec.WorkerNode, reason)
	}
	r.emit(node, corev1.EventTypeNormal, WorkerReturned, fmt.Sprintf(
		"Worker %s is back, and provisioning starts again from the host check",
		node.Spec.WorkerNode))
	return stepCheckingHost, true, nil
}

// workerAway reports whether the machine is unavailable, and says which of the
// two conditions it is so the event names the cause rather than the outcome.
//
// The cordon counts as well as the readiness, because a drain begins with one:
// catching it there is what stops an add being asked for against a machine that
// is about to go down.
//
// A worker the API server does not have is not reported away. It is a different
// condition with its own handling, and answering it here would make every step
// of this path depend on a Node object that a node being torn down no longer
// has.
func (r *StorageNodeReconciler) workerAway(
	ctx context.Context, worker string,
) (bool, string, error) {
	var object corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: worker}, &object); err != nil {
		return false, "", client.IgnoreNotFound(err)
	}
	switch {
	case !workerReady(&object):
		return true, "is not Ready", nil
	case object.Spec.Unschedulable:
		return true, "is cordoned", nil
	default:
		return false, "", nil
	}
}

// checkHost holds until the worker's storage-node API answers, and diverts to
// adoption when this deployment is being taken over wholesale or when a backend
// node is already at the worker's address.
func (r *StorageNodeReconciler) checkHost(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (nodeStep, bool, error) {
	// An upgrade Secret declares that this deployment is being adopted wholesale,
	// which is the migration route off a Helm deployment. It diverts before the
	// host check, because an adopted node is already running and its API answering
	// is not this operator's precondition to establish.
	if r.upgradeAdoption(ctx, node.Namespace, cluster.Name) {
		return stepAdopting, true, nil
	}

	// A backend node already at the worker's address covers a POST whose response
	// was lost after the control plane committed, as well as a node this operator
	// never added.
	if _, found, err := r.matchBackendNode(ctx, node, cluster); err != nil {
		return stepCheckingHost, false, err
	} else if found {
		return stepAdopting, true, nil
	}

	answers, err := r.Workload.HostAnswers(ctx, node.Namespace, node.Spec.WorkerNode)
	if err != nil {
		return stepCheckingHost, false, err
	}
	if !answers {
		return stepCheckingHost, false, blockedf(HostUnreachable,
			"the storage-node API on worker %s does not answer yet", node.Spec.WorkerNode)
	}
	return stepCheckingConfig, true, nil
}

// checkConfig is a gate rather than a validation. A cluster with
// enableFailureDomains set requires every node to declare a fault group, and a
// node that does not is held rather than rejected: the value can arrive later, and
// holding is what makes filling it in sufficient (§4.2).
func (r *StorageNodeReconciler) checkConfig(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (nodeStep, bool, error) {
	if _, found, err := r.matchBackendNode(ctx, node, cluster); err != nil {
		return stepCheckingConfig, false, err
	} else if found {
		return stepAdopting, true, nil
	}

	if ptr.BoolFromOrFalse(cluster.Spec.EnableFailureDomains) &&
		node.Spec.Config.FailureDomain == "" {
		return stepCheckingConfig, false, blockedf(FailureDomainMissing,
			"cluster %s requires a fault group and this node declares none; "+
				"set spec.config.failureDomain", cluster.Name)
	}
	return stepAwaitingSlot, true, nil
}

// awaitSlot is where two independent serialization rules live, and the last place
// a node that needs no add at all is recognized.
//
// maxParallelNodeAdds caps how many workers may be in flight at once, counted by
// distinct worker rather than by object so that a two-socket host consumes one
// slot. Workers hosting a FoundationDB pod are always sequential regardless of
// that cap, because a node add reboots the host and two simultaneous FoundationDB
// reboots reduce the control plane's own fault tolerance. Both are predicates over
// the current state of the cluster's other nodes, so the step re-evaluates them on
// every pass and holds rather than failing.
//
// The sibling check is the third thing here and it is not a serialization rule: a
// second socket of a worker some other object has already claimed must not post
// again, and enters Resolving directly.
func (r *StorageNodeReconciler) awaitSlot(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (nodeStep, bool, error) {
	// A backend node the worker already has is the answer to the question this
	// queue exists to ask, so it is asked before the queue rather than only in
	// front of it. The two gates before this one check the same thing and a node
	// passes them once: one that reached the queue before its backend node
	// existed — a previous operator's add, a POST whose response was lost after
	// the control plane committed, a rebuilt object — would otherwise wait for a
	// slot it has no use for, and hold the cap against the rest of the fleet
	// while it did.
	//
	// It is checked before the worker is, for the reason CheckingHost checks it
	// first: an adopted node is already running, and the machine being reachable
	// is not this operator's precondition to establish.
	if _, found, err := r.matchBackendNode(ctx, node, cluster); err != nil {
		return stepAwaitingSlot, false, err
	} else if found {
		return stepAdopting, true, nil
	}

	// A slot taken for a machine that is not there is an add posted against a
	// worker that cannot answer it. The wait costs nothing, because the slot is
	// still free when the machine comes back, and taking it would put a second
	// worker's configuration change alongside a reboot already under way.
	away, reason, err := r.workerAway(ctx, node.Spec.WorkerNode)
	if err != nil {
		return stepAwaitingSlot, false, err
	}
	if away {
		return stepAwaitingSlot, false, blockedf(WorkerAway,
			"worker %s %s, so no node-add slot is taken for it", node.Spec.WorkerNode, reason)
	}

	siblings, err := r.clusterNodeObjects(ctx, node)
	if err != nil {
		return stepAwaitingSlot, false, err
	}

	// One POST adds every socket of the worker, so a sibling at Posting or beyond
	// means the worker has been claimed.
	for i := range siblings {
		sibling := &siblings[i]
		if sibling.Name == node.Name || sibling.Spec.WorkerNode != node.Spec.WorkerNode {
			continue
		}
		if claimedWorker(sibling) {
			return stepResolving, true, nil
		}
	}

	limit := int32(1)
	if spec := cluster.Spec.StorageNodes; spec != nil && spec.MaxParallelNodeAdds != nil {
		limit = *spec.MaxParallelNodeAdds
	}

	// Who is in flight is read from the cluster's own record rather than counted
	// off the siblings, because the siblings are read from a cache and the record
	// is read from the object this node is about to patch. A cache that has just
	// been filled holds some of the nodes and a cap counted from it is a cap each
	// node applies to a different cluster.
	held, err := r.heldSlots(ctx, cluster)
	if err != nil {
		return stepAwaitingSlot, false, err
	}
	// A slot held for this worker settles this node's answer, whichever object
	// took it. Its own slot means it may post, and a slot its other socket took
	// means the add has been asked for and this object resolves against it
	// instead of asking again.
	//
	// The sibling check above answers the second case a pass later than this
	// does, because a node that has taken a slot is still recorded at
	// AwaitingSlot until its transition is written. With more than one slot free
	// that pass is long enough for the second socket to post an add of its own.
	if i := slices.IndexFunc(held, func(slot simplyblockv1alpha2.ProvisioningSlot) bool {
		return slot.Worker == node.Spec.WorkerNode
	}); i >= 0 {
		if held[i].Node == node.Name {
			return stepPosting, true, nil
		}
		return stepResolving, true, nil
	}

	if int32(len(held)) >= limit {
		return stepAwaitingSlot, false, blockedf(AwaitingSlot,
			"waiting for a node-add slot, %d of %d in flight: %s",
			len(held), limit, describeSlots(held))
	}
	available := limit - int32(len(held))

	// Which of the waiting workers should try for the free slots is decided from
	// the set that is waiting, and it is a tie-break rather than the cap.
	//
	// It settles who goes first among nodes that can see each other, so a node
	// told to wait is not overtaken by one told to wait beside it, and it needs
	// nobody to have written anything: two nodes reading one set reach one
	// answer. What it cannot do is hold the cap, because nodes reading a cold
	// cache read different sets and each is alone in its own. The patch below is
	// what holds the cap.
	holders := slotHolders(held)
	contenders := []string{node.Spec.WorkerNode}
	for i := range siblings {
		sibling := &siblings[i]
		if sibling.Spec.WorkerNode == node.Spec.WorkerNode || claimedWorker(sibling) {
			continue
		}
		if nodeStep(sibling.Status.Step.State) != stepAwaitingSlot {
			// Not waiting for a slot yet, so not competing for this one.
			continue
		}
		if slices.Contains(holders, sibling.Spec.WorkerNode) {
			// Already holding one, and the free slots are counted net of it, so
			// ranking against it would charge this node for the same add twice.
			// A worker is still at this step for the pass between taking its slot
			// and the transition out being written.
			continue
		}
		contenders = append(contenders, sibling.Spec.WorkerNode)
	}
	slices.Sort(contenders)
	contenders = slices.Compact(contenders)

	if rank := slices.Index(contenders, node.Spec.WorkerNode); int32(rank) >= available {
		return stepAwaitingSlot, false, blockedf(AwaitingSlot,
			"waiting for a node-add slot, %d of %d in flight and %d worker(s) ahead",
			len(held), limit, rank)
	}

	// A FoundationDB worker waits for every other FoundationDB worker, whatever
	// the cap says.
	if r.hostsFoundationDB(ctx, node.Namespace, node.Spec.WorkerNode) {
		for _, worker := range holders {
			if r.hostsFoundationDB(ctx, node.Namespace, worker) {
				return stepAwaitingSlot, false, blockedf(AwaitingSlot,
					"worker %s hosts FoundationDB and worker %s is already being added",
					node.Spec.WorkerNode, worker)
			}
		}
	}

	took, err := r.takeSlot(ctx, node, cluster, held, limit)
	if err != nil {
		return stepAwaitingSlot, false, err
	}
	if !took {
		return stepAwaitingSlot, false, blockedf(AwaitingSlot,
			"another node took the last free node-add slot first")
	}
	return stepPosting, true, nil
}

// claimedWorker reports// claimedWorker reports whether an object has claimed its worker, which is the
// transition into Posting or anything past it.
func claimedWorker(node *simplyblockv1alpha2.StorageNode) bool {
	switch nodeStep(node.Status.Step.State) {
	case stepPosting, stepResolving, stepAdopting, stepAwaitingWorker:
		// AwaitingWorker is only reachable from Posting and Resolving, so a node
		// in it has claimed its worker and its add is still outstanding. Holding
		// the claim across the wait is what keeps the cluster's node-add cap
		// closed while a machine reboots.
		return true
	default:
		return node.Status.UUID != ""
	}
}

// postNode adds the worker's nodes. The claim was made by the transition into
// this step, so the call is made once and the step that follows polls for the UUID
// it produces.
func (r *StorageNodeReconciler) postNode(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) error {
	params := r.addParams(node, cluster)
	if err := r.API.AddNode(ctx, cluster.Status.UUID, params); err != nil {
		return fmt.Errorf("add node %s on worker %s: %w",
			node.Name, node.Spec.WorkerNode, err)
	}
	return nil
}

// resolveUUID matches this node's slot against the cluster's node list and writes
// the UUID when it appears.
//
// The match is positional rather than by identity: the control plane's nodes for
// one worker are sorted by RPC port ascending, and position in that list is the
// socket ordinal, because the ports are assigned in socket order at node-add time
// (§4.3).
// nodeAddTask is the control plane's own name for the job Posting starts.
const nodeAddTask = "node_add"

// resolve waits for the backend node the add was asked to produce, and asks
// again when nothing is still working on producing one.
//
// The add can fail after it has started — a management interface the control
// plane cannot find an address on is the case this was written for — and what it
// leaves behind is a task that finished and no node. Nothing about that is
// visible from the node list, which is empty either way, so a step that only
// matched the list waited out its deadline against an add that had already given
// up. It held the cluster's one node-add slot while it waited, so every other
// node of the cluster waited behind a node that was never coming.
//
// The task window is the evidence. A node_add still in it is an add worth
// waiting for; no node_add in it at all means the add this step is waiting on is
// over, and an add that is over without a node is one to ask for again. The
// window is capped, so a task that has scrolled out of it has finished too.
//
// Asking again is unbounded here and bounded by the step's own deadline, which
// is what makes a permanently failing add fail the node rather than spin on it
// forever.
func (r *StorageNodeReconciler) resolve(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (nodeStep, bool, error) {
	done, err := r.resolveUUID(ctx, node, cluster)
	if err != nil || done {
		return stepResolving, done, err
	}

	if addInFlight(cluster) {
		return stepResolving, false, nil
	}

	r.emit(node, corev1.EventTypeWarning, NodeAddGaveUp, fmt.Sprintf(
		"the node_add for worker %s finished without producing a node, so it is being asked for again",
		node.Spec.WorkerNode))
	return stepPosting, true, nil
}

// addInFlight reports whether the control plane is still working on a node_add
// for this cluster.
//
// It does not ask which node the task is for, because the window does not say
// and because the cap is one add at a time: a node_add in flight is this node's
// add or the add of the node holding the slot ahead of it, and waiting is right
// either way.
func addInFlight(cluster *simplyblockv1alpha2.StorageCluster) bool {
	for _, task := range cluster.Status.Tasks {
		if task.Type == nodeAddTask && task.Status != taskDone {
			return true
		}
	}
	return false
}

// taskDone is the control plane's terminal task status. Everything else it
// publishes — new, running, suspended — is a task still being worked through.
const taskDone = "done"

func (r *StorageNodeReconciler) resolveUUID(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (bool, error) {
	reading, found, err := r.matchBackendNode(ctx, node, cluster)
	if err != nil || !found {
		return false, err
	}

	err = r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		status.UUID = reading.UUID
		applyReading(status, reading)
		status.Phase = phaseOf(reading)
		status.Step = statemachine.KubeSnapshot{}
		status.Message = ""
	})
	if err != nil {
		return false, err
	}

	r.register(node, cluster.Status.UUID, reading.UUID)
	if nodeStep(node.Status.Step.State) == stepAdopting {
		r.emit(node, corev1.EventTypeNormal, NodeAdopted,
			fmt.Sprintf("Backend node %s was adopted rather than added", reading.UUID))
	}
	provisioningDurationSeconds.WithLabelValues(node.Spec.ClusterRef).
		Observe(time.Since(node.CreationTimestamp.Time).Seconds())
	return true, nil
}

// matchBackendNode finds the backend node filling this object's slot, by the
// worker's internal IP and the slot's position in the port-sorted list.
func (r *StorageNodeReconciler) matchBackendNode(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (NodeReading, bool, error) {
	worker, err := r.workerIdentity(ctx, node.Spec.WorkerNode)
	if err != nil || (worker.address == "" && worker.systemUUID == "") {
		return NodeReading{}, false, err
	}

	readings, err := r.clusterNodes(ctx, cluster.Status.UUID)
	if err != nil {
		return NodeReading{}, false, err
	}

	var onWorker []NodeReading
	for _, reading := range readings {
		if reading.UUID != "" && worker.owns(reading) {
			onWorker = append(onWorker, reading)
		}
	}
	if len(onWorker) == 0 {
		return NodeReading{}, false, nil
	}
	slices.SortFunc(onWorker, func(a, b NodeReading) int {
		return int(a.RPCPort) - int(b.RPCPort)
	})

	slot := 0
	if node.Spec.Slot != nil {
		slot = int(*node.Spec.Slot)
	}
	if slot >= len(onWorker) {
		// The socket is not online yet. The other sockets of the worker may be,
		// which is why this is "not found" rather than an error.
		return NodeReading{}, false, nil
	}
	return onWorker[slot], true, nil
}

// syncStatus writes what the control plane reports, and returns without patching
// when nothing has moved.
func (r *StorageNodeReconciler) syncStatus(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) (ctrl.Result, error) {
	if cluster == nil {
		return ctrl.Result{RequeueAfter: nodeRetry}, nil
	}
	r.register(node, cluster.Status.UUID, node.Status.UUID)

	reading, found, err := r.nodeReading(ctx, cluster.Status.UUID, node.Status.UUID)
	if err != nil {
		return ctrl.Result{RequeueAfter: nodeRetry}, nil
	}
	if !found {
		// The stored UUID no longer exists. The cluster may have been reset and
		// its nodes recreated, so the object goes back to provisioning rather than
		// reporting a node that is not there.
		return ctrl.Result{RequeueAfter: nodeAdvance},
			r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
				status.UUID = ""
				status.Status = ""
				status.Health = false
				status.Phase = simplyblockv1alpha2.StorageNodePhasePending
				status.Message = "the control plane no longer reports this node"
			})
	}

	wasOnline := node.Status.Phase == simplyblockv1alpha2.StorageNodePhaseOnline
	sample, sampled := r.capacitySample(ctx, cluster.Status.UUID, node.Status.UUID)

	err = r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		applyReading(status, reading)
		status.Phase = phaseOf(reading)
		if sampled && worthWriting(status.Resources.Capacity, sample) {
			status.Resources.Capacity = &simplyblockv1alpha2.StorageNodeCapacity{
				TotalBytes: ptr.To(sample.Total),
				UsedBytes:  ptr.To(sample.Used),
				SampledAt:  ptr.To(metav1.NewTime(sample.SampledAt)),
			}
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if !wasOnline && node.Status.Phase == simplyblockv1alpha2.StorageNodePhaseOnline {
		r.emit(node, corev1.EventTypeNormal, NodeOnline,
			fmt.Sprintf("Node %s is online and carrying its share", node.Status.UUID))
	}
	r.observePhase(node)
	return ctrl.Result{RequeueAfter: nodeRetry}, nil
}

// applyReading copies what the control plane says into the status, carrying the
// previous capacity forward: it is measured elsewhere and on its own schedule.
func applyReading(status *simplyblockv1alpha2.StorageNodeStatus, reading NodeReading) {
	previous := (*simplyblockv1alpha2.StorageNodeCapacity)(nil)
	if status.Resources != nil {
		previous = status.Resources.Capacity
	}

	status.Status = reading.Status
	status.Health = reading.Health
	status.Hostname = reading.Hostname
	status.Uptime = reading.Uptime
	status.FailureDomain = fmt.Sprintf("%d", reading.FailureDomain)
	status.Resources = &simplyblockv1alpha2.StorageNodeResources{
		CPU:      ptr.To(reading.CPUCount),
		Volumes:  ptr.To(reading.Volumes),
		Capacity: previous,
	}
	if reading.Memory > 0 {
		status.Resources.Memory = fmt.Sprintf("%d", reading.Memory)
	}
	// The device summary is absent until the control plane has reported, which is
	// what tells a node that has not reported from one that genuinely has no
	// devices. The stream carries neither count, so a streamed reading leaves
	// whatever the last listed one said.
	if reading.DevicesCount > 0 || reading.OnlineDevicesCount > 0 {
		status.Resources.Devices = &simplyblockv1alpha2.StorageNodeDevices{
			Online: reading.OnlineDevicesCount,
			Total:  reading.DevicesCount,
		}
	}
	status.Ports = &simplyblockv1alpha2.StorageNodePorts{
		Management: reading.ManagementIP,
		NvmeOf:     ptr.To(reading.NVMeOFPort),
		Lvol:       ptr.To(reading.LvolPort),
		Rpc:        ptr.To(reading.RPCPort),
	}
}

// phaseOf is the operator's reading of the lifecycle the control plane reports.
//
// The two are deliberately separate: one says how far the operator has got, and
// the other says what the control plane reports, in its own spelling (§3.3).
func phaseOf(reading NodeReading) simplyblockv1alpha2.StorageNodePhase {
	switch reading.Status {
	case nodeStatusOnline, nodeStatusActive:
		if reading.Resources().degraded() {
			return simplyblockv1alpha2.StorageNodePhaseDegraded
		}
		return simplyblockv1alpha2.StorageNodePhaseOnline
	case nodeStatusSuspended, nodeStatusOffline:
		return simplyblockv1alpha2.StorageNodePhaseOffline
	case nodeStatusInCreation, nodeStatusInRestart:
		return simplyblockv1alpha2.StorageNodePhaseProvisioning
	default:
		// unreachable and timeout, plus anything the control plane adds later. A
		// value this operator does not know is a node it cannot vouch for.
		return simplyblockv1alpha2.StorageNodePhaseFailed
	}
}

// deviceHealth is the node-level half of what StorageDevice reports per device.
type deviceHealth struct{ online, total int32 }

func (d deviceHealth) degraded() bool { return d.total > 0 && d.online < d.total }

// Resources is the device pair a phase is decided from.
func (n NodeReading) Resources() deviceHealth {
	return deviceHealth{online: n.OnlineDevicesCount, total: n.DevicesCount}
}

// capacityWriteThresholdPercent is how much a node's used size has to move before
// the new reading is worth recording: one percent of the node's own total, so a
// larger node tolerates a larger absolute drift.
//
// Some threshold is required rather than merely economical. The reconciler watches
// its own objects, so every status write schedules another reconcile; writing a
// freshly sampled number every time would make the node reconcile itself in a loop
// for as long as any I/O was happening.
const capacityWriteThresholdPercent = 1

// worthWriting reports whether a sample says something the object does not already
// say. A first reading always does; after that the used size has to have moved by
// at least one percent of the total, or the total itself has to have changed,
// which is what a device joining or leaving looks like.
func worthWriting(
	current *simplyblockv1alpha2.StorageNodeCapacity, sample prometheus.Capacity,
) bool {
	if !sample.Sampled() {
		return false // nothing has measured this node; say nothing about it
	}
	if current == nil || current.UsedBytes == nil || current.TotalBytes == nil {
		return true
	}
	if *current.TotalBytes != sample.Total {
		return true
	}
	drift := *current.UsedBytes - sample.Used
	if drift < 0 {
		drift = -drift
	}
	return drift*100 >= sample.Total*capacityWriteThresholdPercent
}

// capacitySample reads one node's occupancy, or reports that there is none.
//
// A failure is not an error the caller has to handle: the rest of the status is
// correct without it, and a node whose capacity is momentarily unknown is worth
// publishing with the figure absent rather than not published at all (§12).
func (r *StorageNodeReconciler) capacitySample(
	ctx context.Context, clusterID, nodeID string,
) (prometheus.Capacity, bool) {
	if r.Capacity == nil || clusterID == "" || nodeID == "" {
		return prometheus.Capacity{}, false
	}
	samples, err := r.Capacity.NodeCapacity(ctx, clusterID)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("no capacity sample for this node",
			"cluster", clusterID, "node", nodeID, "err", err.Error())
		return prometheus.Capacity{}, false
	}
	sample, ok := samples[nodeID]
	return sample, ok
}

// raiseMaintenance raises a HostMaintenance operation when the node's worker has
// been cordoned, and reports whether it did.
//
// The operator raises this and a user does not, which is what §10 means by the
// trigger being the cordon. A user creating one by hand is accepted and behaves
// identically, which is what makes the flow testable without cordoning anything.
func (r *StorageNodeReconciler) raiseMaintenance(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	var worker corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.WorkerNode}, &worker); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !worker.Spec.Unschedulable {
		return false, nil
	}
	return r.ensureOps(ctx, node, simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance,
		node.Name+"-maintenance")
}

// teardown drains the node before the object goes.
//
// A node with no status.uuid has no backend node behind it, so its finalizer is
// removed immediately. One that has data on it gets a Remove operation, owned by
// the node through a controller reference, and the finalizer is held until the
// node's lock is clear (§4.5).
func (r *StorageNodeReconciler) teardown(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(node, NodeFinalizer) {
		return ctrl.Result{}, nil
	}

	if node.Status.UUID == "" {
		r.unregister(node)
		// A node deleted while its add was outstanding is the one case where
		// nothing is left to release the slot afterward, and a slot no reconcile
		// will ever give back is a cap that never reopens.
		if err := r.releaseSlot(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(node, NodeFinalizer)
		return ctrl.Result{}, r.Update(ctx, node)
	}

	if _, err := r.ensureOps(ctx, node,
		simplyblockv1alpha2.StorageNodeOpsActionRemove, node.Name+"-remove"); err != nil {
		return ctrl.Result{}, err
	}

	// The drain reaching a terminal phase is what says it has finished, whatever
	// its outcome. A failed removal leaves the operation as the record of why, so
	// the object is not held forever by a drain nobody is going to retry.
	//
	// The node's lock is not the signal, and the difference is not cosmetic: a
	// drain that has just been raised holds no lock yet, so a teardown reading the
	// empty field would take "not started" for "finished" and drop the finalizer
	// on the same pass that asked for the drain. The object then goes, and with it
	// the operation it owns, and the backend node is left running with its data on
	// it and nothing in Kubernetes tracking it.
	if finished, err := r.drainFinished(ctx, node); err != nil {
		return ctrl.Result{}, err
	} else if !finished {
		return ctrl.Result{RequeueAfter: nodeRetry}, nil
	}

	r.unregister(node)
	controllerutil.RemoveFinalizer(node, NodeFinalizer)
	return ctrl.Result{}, r.Update(ctx, node)
}

// drainFinished reports whether the removal this node raised for itself has
// reached a terminal phase.
//
// An operation that is not there is a drain that has not started rather than one
// that is over: the pass before this one raises it, and one deleted out of band
// is raised again. Treating a missing record as a finished drain is the same
// mistake as treating an unheld lock as one (§4.5).
func (r *StorageNodeReconciler) drainFinished(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	var ops simplyblockv1alpha2.StorageNodeOps
	key := types.NamespacedName{Name: node.Name + "-remove", Namespace: node.Namespace}
	if err := r.Get(ctx, key, &ops); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read the removal of node %s: %w", node.Name, err)
	}
	return terminalOps(ops.Status.Phase), nil
}

// ensureOps raises one operation the entity created for itself, idempotently by
// name, and reports whether it created one.
//
// The entity owns the operation it raised for itself. That is the one direction
// ownership runs between the two categories: an operation never owns its target,
// and one an entity created for itself is a subordinate of it (§4.5).
func (r *StorageNodeReconciler) ensureOps(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	act simplyblockv1alpha2.StorageNodeOpsAction,
	name string,
) (bool, error) {
	var existing simplyblockv1alpha2.StorageNodeOps
	key := types.NamespacedName{Name: name, Namespace: node.Namespace}
	err := r.Get(ctx, key, &existing)
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}

	ops := &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: node.Namespace},
		Spec: simplyblockv1alpha2.StorageNodeOpsSpec{
			NodeRef: node.Name,
			Action:  act,
		},
	}
	if err := controllerutil.SetControllerReference(node, ops, r.Scheme); err != nil {
		return false, fmt.Errorf("own the %s operation on node %s: %w", act, node.Name, err)
	}
	if err := r.Create(ctx, ops); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("raise the %s operation on node %s: %w", act, node.Name, err)
	}
	return true, nil
}

// register tells every stream which object a backend node id belongs to, and opens
// the node's device stream.
func (r *StorageNodeReconciler) register(
	node *simplyblockv1alpha2.StorageNode, clusterID, nodeID string,
) {
	if nodeID == "" {
		return
	}
	for _, registry := range r.Registries {
		registry.RegisterNode(nodeID, client.ObjectKeyFromObject(node))
	}
	if r.DeviceScopes != nil && clusterID != "" {
		r.DeviceScopes.Add(cpinformer.Scope{clusterID, nodeID})
	}
}

// unregister closes the node's device stream and stops naming events after it.
//
// The scope goes first: no further device event can arrive once the stream is
// closed, so the name mappings are dropped second and nothing is left naming
// objects after a node on its way out.
//
// It closes by the node's own id rather than by the cluster-and-node key it was
// opened under, because the two happen at different moments with different
// things readable. The open has the node freshly placed in its cluster, so the
// cluster's id is there; the teardown may run with the cluster already gone,
// which is the ordinary case when a cluster is deleted with its nodes. Requiring
// the cluster's id here left the scope in the set, and the stream behind it
// reconnecting against a 404 for the life of the process.
func (r *StorageNodeReconciler) unregister(node *simplyblockv1alpha2.StorageNode) {
	if node.Status.UUID == "" {
		return
	}
	if r.DeviceScopes != nil {
		r.DeviceScopes.RemoveLeaf(node.Status.UUID)
	}
	for _, registry := range r.Registries {
		registry.UnregisterNode(node.Status.UUID)
	}
}

// nodeReading is what the control plane currently says about the node, from the
// stream's cache once it has delivered its snapshot and from the control plane
// until then.
func (r *StorageNodeReconciler) nodeReading(
	ctx context.Context, clusterID, nodeID string,
) (NodeReading, bool, error) {
	if r.Nodes != nil && r.Nodes.Synced(scopeOf(clusterID)) {
		if _, dto, ok := r.Nodes.Lookup(nodeID); ok {
			return readingFromDTO(dto), true, nil
		}
		return NodeReading{}, false, nil
	}
	return r.API.StorageNode(ctx, clusterID, nodeID)
}

// clusterNodes returns every backend node of the cluster, preferring the stream's
// cache once its snapshot has arrived. The gate is the snapshot rather than a
// preference: an empty unsynced cache and a cluster with no nodes look identical,
// and adoption would read the first as the second and keep waiting for a node that
// is already there.
func (r *StorageNodeReconciler) clusterNodes(
	ctx context.Context, clusterID string,
) ([]NodeReading, error) {
	if r.Nodes != nil && r.Nodes.Synced(scopeOf(clusterID)) {
		cached := r.Nodes.List(scopeOf(clusterID))
		out := make([]NodeReading, 0, len(cached))
		for _, dto := range cached {
			out = append(out, readingFromDTO(dto))
		}
		return out, nil
	}
	return r.API.StorageNodes(ctx, clusterID)
}

// clusterNodeObjects are this node's siblings: every StorageNode of the same
// cluster.
func (r *StorageNodeReconciler) clusterNodeObjects(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) ([]simplyblockv1alpha2.StorageNode, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	err := r.List(ctx, &nodes,
		client.InNamespace(node.Namespace),
		client.MatchingFields{clusterRefField: node.Spec.ClusterRef})
	if err != nil {
		return nil, fmt.Errorf("list the cluster's nodes: %w", err)
	}
	return nodes.Items, nil
}

// workerIdentity is what the two sides of a node both know about one machine.
//
// The firmware UUID is the identity: Kubernetes reads it from the host and
// reports it as Node.status.nodeInfo.systemUUID, and the control plane reads it
// from the same place and reports it as a node's system_uuid. Neither invents
// it and no network carries it.
//
// The address is kept for the control plane that reports no UUID. It was the
// only match there was, and it works wherever the storage plane and Kubernetes
// share a network, which is every deployment that has been matched this way so
// far.
type workerIdentity struct {
	address    string
	systemUUID string
}

// owns reports whether a reading is a node of this worker.
//
// The UUID decides it whenever both sides have one, because it is the answer
// that does not depend on which network the reading's address is on. Falling
// back to the address where the reading carries no UUID keeps the older control
// plane working; falling back where it carries a different one would undo the
// whole point, so a UUID that disagrees is a node of another machine.
func (w workerIdentity) owns(reading NodeReading) bool {
	if reading.SystemUUID != "" && w.systemUUID != "" {
		return strings.EqualFold(reading.SystemUUID, w.systemUUID)
	}
	return w.address != "" && reading.ManagementIP == w.address
}

// workerIdentity reads both from the worker's Node object.
func (r *StorageNodeReconciler) workerIdentity(
	ctx context.Context, worker string,
) (workerIdentity, error) {
	var object corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: worker}, &object); err != nil {
		return workerIdentity{}, client.IgnoreNotFound(err)
	}
	identity := workerIdentity{systemUUID: object.Status.NodeInfo.SystemUUID}
	for _, address := range object.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			identity.address = address.Address
			break
		}
	}
	return identity, nil
}

// hostsFoundationDB reports whether a worker runs a FoundationDB pod, which is
// what makes its node add sequential regardless of the parallel-add cap.
func (r *StorageNodeReconciler) hostsFoundationDB(
	ctx context.Context, namespace, worker string,
) bool {
	var pods corev1.PodList
	err := r.List(ctx, &pods,
		client.InNamespace(namespace), client.HasLabels{utils.LabelFDBClusterName})
	if err != nil {
		// An unreadable list is treated as a yes, which serializes rather than
		// parallelizes. The
		// cost of being wrong that way is a slower expansion; the other way it is
		// two simultaneous reboots of the control plane's own store.
		return true
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == worker {
			return true
		}
	}
	return false
}

// upgradeAdoption reports whether this deployment is being adopted wholesale,
// which is the same signal the cluster's own creation path reads.
func (r *StorageNodeReconciler) upgradeAdoption(
	ctx context.Context, namespace, cluster string,
) bool {
	var secret corev1.Secret
	key := types.NamespacedName{
		Name:      fmt.Sprintf("simplyblock-%s-upgrade", cluster),
		Namespace: namespace,
	}
	return r.Get(ctx, key, &secret) == nil
}

// addParams is what the node-add call carries. The node describes itself, so every
// value but the subsystem cap comes from its own spec.config (§3.1).
func (r *StorageNodeReconciler) addParams(
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
) utils.StorageNodeSetAddParams {
	config := node.Spec.Config
	workload := cluster.Spec.StorageNodes
	if workload == nil {
		workload = &simplyblockv1alpha2.StorageNodesSpec{}
	}

	params := utils.StorageNodeSetAddParams{
		NodeAddress:      r.Workload.NodeAddress(node.Spec.WorkerNode, node.Namespace),
		InterfaceName:    workload.MgmtInterface,
		SPDKImage:        config.SpdkImage,
		SPDKProxyImage:   config.SpdkProxyImage,
		DataNics:         workload.DataInterfaces,
		Namespace:        node.Namespace,
		JMPercent:        journalPercent(config.JournalManager),
		Partitions:       partitionsPerDevice(workload),
		HaJMCount:        journalCount(config.JournalManager),
		CRName:           cluster.Name,
		CRNameSpace:      cluster.Namespace,
		CRPlural:         "storageclusters",
		Format4K:         ptr.BoolFromOrFalse(workload.EnableFormat4K),
		SpdkSystemMemory: config.SpdkSystemMemory,
		Expand:           ptr.BoolFromOrFalse(config.Expand),
	}

	// The control plane's failure domain is an integer, and this API's is a label.
	// Only a label that is a number can be sent, which is what a domain seeded
	// from an index looks like; anything else is a name the control plane has no
	// field for and is left to it to assign.
	if index := domainIndex(config.FailureDomain); index != nil {
		params.FailureDomain = index
	}
	return params
}

// domainIndex reads a failure-domain label as the integer the control plane's own
// field takes, and reports nil for a label that is not one.
//
// The two vocabularies genuinely differ: this API names a fault group after the
// rack, the zone, or the power feed somebody would say out loud, and the control
// plane indexes one. A label seeded from an index sends its number, and a name
// the control plane has no field for is left to it to assign — which is why
// status.failureDomain reports what was assigned rather than what was asked for
// (§3.3).
func domainIndex(domain string) *int {
	if domain == "" {
		return nil
	}
	index, err := strconv.Atoi(domain)
	if err != nil {
		return nil
	}
	return &index
}

// partitionsPerDevice is how many partitions each device is carved into, which is
// one when the journal has a device of its own and two when it shares.
// partitionsPerDevice translates spec.enableJournalDevice into the count the
// control plane wants: 0 gives a whole device to the journal manager, and 1
// carves a journal partition out of each storage device. Unset is 1, which is
// what the retired spec.partitions field defaulted to.
//
// The numbers are not arbitrary and are not a preference. The backend compares
// what a device already carries against 1 + this count and repartitions when
// they differ, and it cannot repartition a device whose table SPDK's gpt module
// has claimed — which is every device of a machine that has run this product
// before. A count one higher than the fleet was built with therefore does not
// lay the disks out differently; it makes the node impossible to add.
func partitionsPerDevice(workload *simplyblockv1alpha2.StorageNodesSpec) int {
	if ptr.BoolFromOrFalse(workload.EnableJournalDevice) {
		return 0
	}
	return 1
}

func journalPercent(spec *simplyblockv1alpha2.JournalManagerSpec) int {
	if spec == nil {
		return 3
	}
	return ptr.IntFrom(spec.PercentPerDevice, 3)
}

func journalCount(spec *simplyblockv1alpha2.JournalManagerSpec) int {
	if spec == nil {
		return 3
	}
	return ptr.IntFrom(spec.Count, 3)
}

// recordStep persists the step the machine is about to be in, with the instant it
// expires. Both travel together, because a step persisted without its deadline
// restores as a step that can never time out.
func (r *StorageNodeReconciler) recordStep(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	next nodeStep,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		status.Phase = simplyblockv1alpha2.StorageNodePhaseProvisioning
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: deadline}
	})
}

// hold reports a provisioning step that is waiting on something outside this
// process. It is not a failure, so the deadline keeps running.
func (r *StorageNodeReconciler) hold(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, message string,
) error {
	return r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
		if status.Phase == "" {
			status.Phase = simplyblockv1alpha2.StorageNodePhasePending
		}
		status.Message = message
	})
}

// fail ends provisioning. A node whose machine cannot be resumed, or whose step
// outlived its budget, is Failed with the reason in its message rather than
// retrying forever.
func (r *StorageNodeReconciler) fail(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode, message string,
) error {
	r.emit(node, corev1.EventTypeWarning, HostUnreachable, message)
	// A node that has given up holds nothing. Releasing here rather than leaving
	// it to the next node to reap is what keeps a cap from reading as full for as
	// long as nobody happens to ask for a slot.
	return errors.Join(
		r.releaseSlot(ctx, node),
		r.writeStatus(ctx, node, func(status *simplyblockv1alpha2.StorageNodeStatus) {
			status.Phase = simplyblockv1alpha2.StorageNodePhaseFailed
			status.Message = message
		}))
}

// writeStatus applies the mutation and patches only when something changed, which
// is what keeps a node serving I/O from reconciling itself in a loop.
func (r *StorageNodeReconciler) writeStatus(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	mutate func(*simplyblockv1alpha2.StorageNodeStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageNode
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if equalNodeStatus(fresh.Status, desired) {
			node.Status = desired
			node.ResourceVersion = fresh.ResourceVersion
			return nil
		}

		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		fresh.Status = desired
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		node.Status = fresh.Status
		node.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// emit raises an event about the node's own lifecycle, which is what an
// administrator looking at a worker has open.
func (r *StorageNodeReconciler) emit(
	node *simplyblockv1alpha2.StorageNode, eventType, reason, message string,
) {
	r.Recorder.Eventf(node, nil, eventType, reason, reason, "%s", message)
}

// observePhase publishes the node's phase as a gauge, so a node stuck in
// Provisioning is alertable rather than merely visible.
func (r *StorageNodeReconciler) observePhase(node *simplyblockv1alpha2.StorageNode) {
	for _, phase := range []simplyblockv1alpha2.StorageNodePhase{
		simplyblockv1alpha2.StorageNodePhasePending,
		simplyblockv1alpha2.StorageNodePhaseProvisioning,
		simplyblockv1alpha2.StorageNodePhaseOnline,
		simplyblockv1alpha2.StorageNodePhaseRemoving,
		simplyblockv1alpha2.StorageNodePhaseOffline,
		simplyblockv1alpha2.StorageNodePhaseDegraded,
		simplyblockv1alpha2.StorageNodePhaseFailed,
	} {
		value := 0.0
		if node.Status.Phase == phase {
			value = 1
		}
		nodePhaseState.WithLabelValues(node.Spec.ClusterRef, node.Name, string(phase)).Set(value)
	}
}

// equalNodeStatus compares two statuses for the purpose of deciding whether to
// write. It is spelled out rather than reflect.DeepEqual because the status
// carries pointers, and two equal values behind two pointers are not deeply equal.
func equalNodeStatus(a, b simplyblockv1alpha2.StorageNodeStatus) bool {
	if a.Phase != b.Phase || a.UUID != b.UUID || a.Status != b.Status ||
		a.Health != b.Health || a.Hostname != b.Hostname || a.Uptime != b.Uptime ||
		a.FailureDomain != b.FailureDomain || a.ActiveOpsRef != b.ActiveOpsRef ||
		a.Message != b.Message || a.ObservedGeneration != b.ObservedGeneration ||
		a.Step.State != b.Step.State || !equalTime(a.Step.Deadline, b.Step.Deadline) {
		return false
	}
	return equalResources(a.Resources, b.Resources) && equalPorts(a.Ports, b.Ports)
}

func equalResources(a, b *simplyblockv1alpha2.StorageNodeResources) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !equalInt32(a.CPU, b.CPU) || a.Memory != b.Memory ||
		!equalInt32(a.Volumes, b.Volumes) {
		return false
	}
	if (a.Devices == nil) != (b.Devices == nil) {
		return false
	}
	if a.Devices != nil && *a.Devices != *b.Devices {
		return false
	}
	return equalCapacity(a.Capacity, b.Capacity)
}

func equalCapacity(a, b *simplyblockv1alpha2.StorageNodeCapacity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalInt64(a.TotalBytes, b.TotalBytes) &&
		equalInt64(a.UsedBytes, b.UsedBytes) &&
		equalTime(a.SampledAt, b.SampledAt)
}

func equalPorts(a, b *simplyblockv1alpha2.StorageNodePorts) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Management == b.Management && equalInt32(a.NvmeOf, b.NvmeOf) &&
		equalInt32(a.Lvol, b.Lvol) && equalInt32(a.Rpc, b.Rpc)
}

func equalInt32(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
