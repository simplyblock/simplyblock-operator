// The Migrate action: relocating a node onto another worker.
//
// A migration moves a storage node to a different worker host without draining
// it. The node keeps its backend UUID, its partitions, and its logical-volume
// assignments, and what changes is the machine the SPDK process runs on. It is
// therefore not a removal followed by an add, and no volume migration is created.
//
//	Preparing ──► Relocating ──► AwaitingNode ──► Promoting
//
// The mechanism is a control-plane restart pointed at a different node_address,
// which is the same primitive a host maintenance uses aimed somewhere else.
//
// Relocating and AwaitingNode are two steps because one would race. The restart is
// asynchronous, so a node still reporting online immediately after the call may be
// reporting the state from before it. This is the one place in the design where a
// step's completion condition is a negative predicate, and it is worth naming as a
// weakness: a stream that coalesces can deliver online before and online after
// without ever delivering what is in between, so the departure can be missed. What
// makes it tolerable is that Promoting re-reads the node before issuing, so a
// promote that would race is refused there. §16 Q4 is the control-plane request
// that would make the observation positive instead.
//
// design-storagenode.md §9 is the specification.

package node

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// performMigrateStep runs one step of the relocation.
func (r *StorageNodeOpsReconciler) performMigrateStep(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
) (bool, error) {
	target := ops.Spec.MigrateParams().TargetWorkerNode
	if target == "" {
		return false, fatalf("spec.migrate.targetWorkerNode is empty; there is nowhere to relocate to")
	}

	node, err := r.node(ctx, ops)
	if err != nil {
		return false, err
	}
	if node.Spec.WorkerNode == target && current == stepPreparing {
		// The node is already where the operation would move it, which is what a
		// re-run of a finished migration looks like. There is nothing to prepare
		// and nothing to relocate, and saying so beats issuing a restart that
		// moves a node onto the host it is on.
		return true, nil
	}

	clusterID, nodeID, err := r.target(ctx, ops)
	if err != nil {
		return false, err
	}

	switch current {
	case stepPreparing:
		return r.migratePrepare(ctx, ops, node, target)
	case stepRelocating:
		return r.migrateRelocate(ctx, ops, node, target, clusterID, nodeID)
	case stepAwaitingNode:
		return r.migrateAwaitNode(ctx, clusterID, nodeID)
	case stepPromoting:
		return r.migratePromote(ctx, ops, node, target, clusterID, nodeID)
	default:
		return false, fatalf("step %s does not belong to the Migrate action", current)
	}
}

// migratePrepare puts the target host into the storage plane and holds until the
// control plane will be able to resolve it.
//
// It blocks on DNS, not on readiness. The control plane resolves node_address
// itself, and a name that does not resolve makes the restart fail inside the
// control plane, whose response is to reset the node to offline. Pod readiness
// happens before the EndpointSlice is published, so waiting on readiness alone
// leaves a window in which the restart is issued against a name that does not yet
// exist (§9).
func (r *StorageNodeOpsReconciler) migratePrepare(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
	target string,
) (bool, error) {
	var worker corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: target}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fatalf("target worker %s is not a node of this Kubernetes cluster", target)
		}
		return false, fmt.Errorf("read target worker %s: %w", target, err)
	}
	if !workerReady(&worker) {
		return false, fatalf("target worker %s is not Ready", target)
	}

	// The target's per-node configuration is written before it is labeled, so the
	// entry exists by the time the pod's init container sources it. Any drives
	// spec.migrate.newSsdPcie names are merged into the cloned allow list, so the
	// target host binds them on start and they survive a later rebuild (§3.2).
	err := r.Workload.CloneWorkerConfig(ctx, node.Namespace, node.Spec.ClusterRef,
		node.Spec.WorkerNode, target, ops.Spec.MigrateParams().NewSsdPcie)
	if err != nil {
		return false, fmt.Errorf("clone the node's configuration onto worker %s: %w", target, err)
	}

	if err := r.Workload.LabelWorker(ctx, node.Namespace, node.Spec.ClusterRef, target); err != nil {
		return false, fmt.Errorf("label worker %s into the storage plane: %w", target, err)
	}

	ready, err := r.Workload.PodReady(ctx, node.Namespace, node.Spec.ClusterRef, target)
	if err != nil {
		return false, err
	}
	if !ready {
		return false, nil
	}

	// The EndpointSlice is read through an uncached reader. A stale informer cache
	// can miss a freshly published endpoint, and the consequence is a migration
	// that waits on DNS forever while the name has in fact resolved for minutes
	// (§5.4).
	return r.Workload.PublishedInDNS(ctx, node.Namespace, node.Spec.ClusterRef, target)
}

// migrateRelocate issues the restart pointed at the target and completes when the
// node has left online, which is the observation that the restart has actually
// started.
//
// The restart is forced by default. A migration relocates a node that is still
// online, and the control plane rejects a non-forced restart of a node that is not
// already offline. spec.force is honored when it is set explicitly, which is the
// one place a default of true is the right one (§9).
func (r *StorageNodeOpsReconciler) migrateRelocate(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
	target, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if reading.Status != nodeStatusOnline {
		return true, nil
	}

	force := true
	if ops.Spec.Force != nil {
		force = *ops.Spec.Force
	}
	params := RestartParams{
		NodeAddress:    r.Workload.NodeAddress(ctx, target, node.Namespace),
		Force:          force,
		ReattachVolume: boolValue(ops.Spec.ReattachVolume),
		NewSsdPcie:     ops.Spec.MigrateParams().NewSsdPcie,
	}
	if err := r.API.RestartNode(ctx, clusterID, nodeID, params); err != nil {
		return false, fmt.Errorf("relocate node %s onto worker %s: %w",
			ops.Spec.NodeRef, target, err)
	}
	return false, nil
}

// migrateAwaitNode waits for the node to be online again, on the host it was
// relocated onto. It performs nothing: the restart is running and this step is the
// half of the pair that observes it finish.
func (r *StorageNodeOpsReconciler) migrateAwaitNode(
	ctx context.Context, clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	return reading.Status == nodeStatusOnline, nil
}

// migratePromote activates the relocated node and then re-points the Kubernetes
// view of where it runs.
//
// The promote is the step with no way back: it activates the target host's
// devices, fails and migrates the origin host's, starts a rebalance, and re-homes
// the logical volumes. The graph declares no edge from Promoting to an aborted
// state for that reason.
//
// The topology re-point happens after the promote and never before. Doing it first
// would leave the Kubernetes view describing a relocation the control plane had
// not performed.
func (r *StorageNodeOpsReconciler) migratePromote(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
	target, clusterID, nodeID string,
) (bool, error) {
	// The promote is separately guarded, which is what makes the negative
	// predicate in Relocating tolerable: a node that is not online has not
	// finished its restart, and promoting into an in-flight one leaves the
	// relocated devices stuck in `new` (§9).
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if reading.Status != nodeStatusOnline {
		return false, nil
	}

	// A node whose object already names the target has been promoted by an
	// earlier pass: the re-point below is the last thing this step does, so its
	// presence is the record that the promote landed.
	if node.Spec.WorkerNode != target {
		if err := r.API.Promote(ctx, clusterID, nodeID); err != nil {
			return false, fmt.Errorf("promote node %s on worker %s: %w",
				ops.Spec.NodeRef, target, err)
		}
		if err := r.repointTopology(ctx, node, target, ops.Spec.MigrateParams().NewSsdPcie); err != nil {
			return false, err
		}
	}

	// The source worker keeps its storage-plane labels while any other node still
	// runs there, and loses them when none does. Removing them from a worker that
	// still hosts a node would unschedule it.
	err = r.Workload.ReleaseWorker(ctx, node.Namespace, node.Spec.ClusterRef, node.Name)
	return err == nil, err
}

// repointTopology moves the Kubernetes half of the relocation: the node's worker,
// the drives the migration bound on the target, and the storage-plane labels that
// follow the worker.
//
// The allow-list merge is why spec.config.pcieAllowList is guarded by the webhook
// rather than by an immutability marker: this is the one legitimate writer of it,
// and the addresses have to survive a later rebuild of the node (§3.2).
func (r *StorageNodeOpsReconciler) repointTopology(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	target string,
	newSsdPcie []string,
) error {
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.WorkerNode = target
	node.Spec.Config.PcieAllowList = mergePCIAddresses(node.Spec.Config.PcieAllowList, newSsdPcie)
	if err := r.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("re-point node %s onto worker %s: %w", node.Name, target, err)
	}
	return r.Workload.LabelWorker(ctx, node.Namespace, node.Spec.ClusterRef, target)
}

// mergePCIAddresses adds the addresses a migration bound to the list the node
// already had, keeping the existing order and appending what is new.
//
// Order is preserved rather than sorted because the list is a user's, and a field
// the operator rewrites should come back recognizable to whoever wrote it.
func mergePCIAddresses(existing, added []string) []string {
	if len(added) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing))
	for _, address := range existing {
		seen[address] = struct{}{}
	}
	merged := existing
	for _, address := range added {
		if address == "" {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		merged = append(merged, address)
	}
	return merged
}

// workerReady reports the Kubernetes Ready condition of a worker.
func workerReady(worker *corev1.Node) bool {
	for _, condition := range worker.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ensurePtr keeps the atlas-lib pointer helper imported where the package uses it
// for the optional flags a restart carries.
var _ = ptr.To[bool]
