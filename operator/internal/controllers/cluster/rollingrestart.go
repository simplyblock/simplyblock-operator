// The rolling restart: the one action whose progress has to survive an
// operator restart, because it walks every storage node in the cluster in
// sequence.
//
// The position is split across two fields on purpose. status.step is where the
// machine has got to within the node being restarted, and
// status.rollingRestart is which node that is; neither is complete without the
// other. The node list is written once and an index moves through it, which is
// what makes advancing one increment rather than a removal and an append that
// have to agree, and what keeps the order the walk was planned in to the end
// instead of draining it away.
//
// A node added mid-walk is not restarted and one removed mid-walk is skipped
// when the walk reaches it. Both follow from a rolling restart being over the
// fleet it was started against, and neither is a failure.
//
// design-storagecluster.md §7 is the specification.

package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// storageNodePodLabels select the storage-node pod of one cluster. The pod the
// walk refreshes is found by intersecting them with the Kubernetes node the
// control plane says the storage node runs on.
const (
	storageNodePodAppLabel     = "app"
	storageNodePodApp          = "storage-node"
	storageNodePodClusterLabel = "simplyblock-cluster"
)

// performNodeStep runs one step of the rolling restart against the node the
// walk is currently on.
//
// Every step reads the node list first, because every predicate here is over
// the control plane's current answer. A node the control plane no longer lists
// is one that was removed mid-walk: the walk skips it rather than holding for
// a node that will never report anything again.
func (r *StorageClusterOpsReconciler) performNodeStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	current step,
) (bool, error) {
	if err := r.planWalk(ctx, ops, clusterID); err != nil {
		return false, err
	}
	walk := walkOf(ops)
	if int(walk.NodeIndex) >= len(walk.Nodes) {
		// Every node is done, and the caller's IsTerminal check will finish
		// the operation on the next pass. Reporting the step finished here is
		// what gets it there.
		return true, nil
	}
	nodeID := walk.Nodes[walk.NodeIndex]

	nodes, err := r.storageNodes(ctx, clusterID)
	if err != nil {
		return false, err
	}

	switch current {
	case stepCheckingPeers:
		return r.checkPeers(ops, nodes, nodeID)
	case stepShuttingDownNode:
		return r.shutDownNode(ctx, ops, clusterID, nodes, nodeID)
	case stepRefreshingPod:
		return r.refreshPod(ctx, ops, nodes, nodeID)
	case stepAwaitingPod:
		return r.awaitPod(ctx, ops, nodes, nodeID)
	case stepRestartingNode:
		return r.restartNode(ctx, ops, clusterID, nodes, nodeID)
	case stepRebalancing:
		return r.awaitRebalance(ctx, clusterID)
	default:
		return false, fatalf("step %s is not part of the rolling restart", current)
	}
}

// planWalk lists the cluster's nodes and writes the order they will be
// restarted in, once. A reconcile that finds the list populated does not
// re-list, which is what makes the walk cover the fleet it was started
// against.
func (r *StorageClusterOpsReconciler) planWalk(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, clusterID string,
) error {
	if ops.Status.RollingRestart != nil {
		return nil
	}
	nodes, err := r.storageNodes(ctx, clusterID)
	if err != nil {
		return err
	}
	planned := make([]string, 0, len(nodes))
	for _, node := range nodes {
		planned = append(planned, node.UUID)
	}
	rollingRestartNodeTotal.WithLabelValues(ops.Spec.ClusterRef).Set(float64(len(planned)))
	rollingRestartNodeIndex.WithLabelValues(ops.Spec.ClusterRef).Set(0)

	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.RollingRestart = &simplyblockv1alpha2.RollingRestartStatus{
			Nodes:     planned,
			NodeIndex: 0,
		}
	})
}

// storageNodes is every node of the cluster, from the stream's cache once it
// has delivered the cluster's snapshot and from the control plane until then.
//
// The cache is preferred because the stream has already delivered exactly this
// list, and a walk asks for it once per step per node: a twenty-node fleet is
// eighty reads that are all answers the informer is already holding. It is
// trusted only once the scope is synced, because an empty unsynced cache and a
// cluster with no nodes look identical, and reading the first as the second
// would plan a walk over nothing and call it a success.
//
// The scope is the cluster, and the object that opens it is the StorageCluster
// reconciler next door: a cluster in steady state has its node stream running,
// which is every cluster an operation can act on.
func (r *StorageClusterOpsReconciler) storageNodes(
	ctx context.Context, clusterID string,
) ([]utils.NodeStatusResponse, error) {
	scope := cpinformer.Scope{clusterID}
	if r.Nodes != nil && r.Nodes.Synced(scope) {
		cached := r.Nodes.List(scope)
		nodes := make([]utils.NodeStatusResponse, 0, len(cached))
		for _, dto := range cached {
			nodes = append(nodes, utils.NodeStatusResponse{
				UUID: dto.ID, Status: dto.Status, IP: dto.ManagementIP,
			})
		}
		return nodes, nil
	}

	nodes, err := r.API.StorageNodes(ctx, clusterID)
	if err != nil {
		return nil, fmt.Errorf("read the storage nodes of cluster %s: %w", clusterID, err)
	}
	return nodes, nil
}

// checkPeers is the safety property of the whole action. Taking a node down
// while another is already offline can exceed the cluster's fault tolerance and
// lose data, so every shutdown is gated on all peers being online and the walk
// holds here rather than proceeding.
//
// It performs no side effect. Its deadline is what distinguishes a walk holding
// because the cluster is degraded from one holding because of a bug.
func (r *StorageClusterOpsReconciler) checkPeers(
	ops *simplyblockv1alpha2.StorageClusterOps,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	var offline []string
	for _, node := range nodes {
		if node.UUID == nodeID {
			continue
		}
		if lower(node.Status) != utils.NodeStatusOnline {
			offline = append(offline, fmt.Sprintf("%s (%s)", node.UUID, node.Status))
		}
	}
	if len(offline) == 0 {
		r.observePeerHold(ops)
		return true, nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
		PeerNodeNotOnline, PeerNodeNotOnline,
		"The walk is holding before node %s: %s", nodeID, strings.Join(offline, ", "))
	return false, nil
}

// observePeerHold records how long the walk held before this node. The hold
// began when the step was entered, which its deadline minus the step's budget
// gives, so nothing extra is persisted for the measurement.
func (r *StorageClusterOpsReconciler) observePeerHold(
	ops *simplyblockv1alpha2.StorageClusterOps,
) {
	deadline, bounded := ops.Status.Step.KubeDeadline()
	if !bounded {
		return
	}
	held := time.Since(deadline.Add(-checkingPeersDeadline)).Seconds()
	if held < 0 {
		return
	}
	rollingRestartPeerHoldSeconds.WithLabelValues(ops.Spec.ClusterRef).Observe(held)
}

// shutDownNode asks the control plane to take the node down, and is finished
// when the node is offline or beyond. The call is skipped when the node is
// already at or past in_shutdown, so re-entering the step after a crash sends
// no second shutdown.
func (r *StorageClusterOpsReconciler) shutDownNode(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	status, listed := nodeStatus(nodes, nodeID)
	if !listed {
		// The node left the cluster while the walk was running. There is
		// nothing to shut down and nothing to restart.
		return true, nil
	}
	switch status {
	case utils.NodeStatusOffline, utils.NodeStatusInRestart, utils.NodeStatusRemoved:
		return true, nil
	case utils.NodeStatusInShutdown:
		return false, nil
	}
	if err := r.API.ShutdownNode(ctx, clusterID, nodeID); err != nil {
		return false, fmt.Errorf("shut down node %s: %w", nodeID, err)
	}
	logf.FromContext(ctx).Info("the node was asked to shut down",
		"operation", ops.Name, "node", nodeID)
	return false, nil
}

// refreshPod deletes the node's storage-node pod, which is what forces the
// image to be pulled again, and is finished when the pod is gone. It is entered
// only when spec.rollingRestart.refreshSNodeAPI is set.
func (r *StorageClusterOpsReconciler) refreshPod(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	pod, err := r.storageNodePod(ctx, ops, nodes, nodeID)
	if err != nil {
		return false, err
	}
	if pod == nil {
		return true, nil
	}
	if !pod.DeletionTimestamp.IsZero() {
		// Already going. Waiting for it to finish is this step's whole job.
		return false, nil
	}
	if err := r.Delete(ctx, pod); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return false, nil
}

// awaitPod waits for the replacement pod to be Ready. It performs no side
// effect: the DaemonSet is what creates the replacement.
func (r *StorageClusterOpsReconciler) awaitPod(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	pod, err := r.storageNodePod(ctx, ops, nodes, nodeID)
	if err != nil {
		return false, err
	}
	if pod == nil || !pod.DeletionTimestamp.IsZero() {
		return false, nil
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// restartNode asks the control plane to bring the node back, and is finished
// when it reports online. The call is skipped when the node is already
// restarting or online.
func (r *StorageClusterOpsReconciler) restartNode(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	status, listed := nodeStatus(nodes, nodeID)
	if !listed {
		return true, nil
	}
	switch status {
	case utils.NodeStatusOnline:
		return true, nil
	case utils.NodeStatusInRestart:
		return false, nil
	}
	if err := r.API.RestartNode(ctx, clusterID, nodeID); err != nil {
		return false, fmt.Errorf("restart node %s: %w", nodeID, err)
	}
	logf.FromContext(ctx).Info("the node was asked to restart",
		"operation", ops.Name, "node", nodeID)
	return false, nil
}

// awaitRebalance waits for the cluster to finish redistributing after the node
// came back. It performs no side effect, and completing it is what advances the
// walk.
func (r *StorageClusterOpsReconciler) awaitRebalance(
	ctx context.Context, clusterID string,
) (bool, error) {
	reading, err := r.clusterReading(ctx, clusterID)
	if err != nil {
		return false, err
	}
	return !reading.Rebalancing, nil
}

// advanceWalk moves to the next node, or finishes the operation when the index
// reaches the end of the list.
//
// Starting the next node is Machine.Reset rather than a transition: entering
// CheckingPeers for node five is not a transition out of node four's
// Rebalancing, and declaring it as one would make the graph cyclic and
// IsTerminal useless. Reset also clears the deadline, which is what gives each
// node its own budget per step rather than one deadline covering the fleet.
func (r *StorageClusterOpsReconciler) advanceWalk(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	machine *statemachine.Machine[step],
) (ctrl.Result, error) {
	walk := walkOf(ops)
	next := walk.NodeIndex + 1
	finished := walk.Nodes[walk.NodeIndex]

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
		NodeRestarted, NodeRestarted,
		"Node %d/%d (%s) was restarted", next, len(walk.Nodes), finished)

	if int(next) >= len(walk.Nodes) {
		err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
			status.RollingRestart.NodeIndex = next
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		rollingRestartNodeIndex.WithLabelValues(ops.Spec.ClusterRef).Set(float64(next))
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
			r.successMessage(ops))
	}

	// Write-ahead in both halves: the index and the step move together, and
	// the step is recorded before the next node is touched.
	machine.Reset()
	deadline := metav1.NewTime(time.Now().Add(checkingPeersDeadline))
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.RollingRestart.NodeIndex = next
		status.Step = statemachine.KubeSnapshot{
			State:    string(machine.CurrentState()),
			Deadline: &deadline,
		}
		status.Message = walkMessage(ops, string(machine.CurrentState()))
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	rollingRestartNodeIndex.WithLabelValues(ops.Spec.ClusterRef).Set(float64(next))
	return ctrl.Result{RequeueAfter: opsAdvance}, nil
}

// storageNodePod finds the Kubernetes pod serving one storage node, and
// reports nil when there is none.
//
// The join is the node's management IP, which the control plane reports and
// which a Kubernetes Node carries as its internal address. It is the only
// identifier the two sides share.
func (r *StorageClusterOpsReconciler) storageNodePod(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (*corev1.Pod, error) {
	var address string
	for _, node := range nodes {
		if node.UUID == nodeID {
			address = node.IP
			break
		}
	}
	if address == "" {
		return nil, nil
	}

	var workers corev1.NodeList
	if err := r.List(ctx, &workers); err != nil {
		return nil, fmt.Errorf("list the Kubernetes nodes: %w", err)
	}
	worker := ""
	for i := range workers.Items {
		for _, addr := range workers.Items[i].Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address == address {
				worker = workers.Items[i].Name
			}
		}
	}
	if worker == "" {
		return nil, nil
	}

	var pods corev1.PodList
	err := r.List(ctx, &pods,
		client.InNamespace(ops.Namespace),
		client.MatchingLabels{
			storageNodePodAppLabel:     storageNodePodApp,
			storageNodePodClusterLabel: ops.Spec.ClusterRef,
		})
	if err != nil {
		return nil, fmt.Errorf("list the storage node pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == worker {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

// walkFinished reports a walk whose index has reached the end of its list,
// which is what completion means. It is true before anything has been planned
// as well, because a walk of no nodes has nothing left to do.
func walkFinished(ops *simplyblockv1alpha2.StorageClusterOps) bool {
	walk := ops.Status.RollingRestart
	return walk != nil && int(walk.NodeIndex) >= len(walk.Nodes)
}

// walkOf is the walk's position, and an empty one before it has been planned.
func walkOf(ops *simplyblockv1alpha2.StorageClusterOps) *simplyblockv1alpha2.RollingRestartStatus {
	if ops.Status.RollingRestart == nil {
		return &simplyblockv1alpha2.RollingRestartStatus{}
	}
	return ops.Status.RollingRestart
}

// walkMessage is the progress line status.message carries, read off the index
// and the list so that `kubectl describe scops` shows the position in the walk
// without reading the cluster object.
func walkMessage(ops *simplyblockv1alpha2.StorageClusterOps, current string) string {
	walk := walkOf(ops)
	if len(walk.Nodes) == 0 {
		return "planning the walk"
	}
	position := int(walk.NodeIndex)
	if position >= len(walk.Nodes) {
		position = len(walk.Nodes) - 1
	}
	return fmt.Sprintf("Node %d/%d (%s): %s",
		position+1, len(walk.Nodes), walk.Nodes[position], current)
}

// lower is strings.ToLower, named for what the control plane's status strings
// need rather than for what it does, because every comparison against them has
// to make the same allowance.
func lower(status string) string { return strings.ToLower(status) }
