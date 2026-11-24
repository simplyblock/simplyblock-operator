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
// The walk moves a batch at a time. A batch is one node, except in a cluster
// in failure-domain mode with at least as many failure domains as a stripe has
// chunks: each failure domain then holds one chunk of a stripe, a whole one
// may be down at once, and the batch is the nodes of one failure domain. Its
// nodes are shut down one after another, because the control plane takes one
// shutdown at a time, and restarted together, because it restarts the nodes of
// a failure domain that is wholly down in parallel. The cluster rebalances
// once per batch rather than once per node, which is what makes a large fleet
// take a walk per failure domain instead of a walk per node.
//
// A node added mid-walk is not restarted and one removed mid-walk is skipped
// when the walk reaches it. Both follow from a rolling restart being over the
// fleet it was started against, and neither is a failure.
//
// A node leaving the cluster belongs to its removal, which is why the walk
// never touches one: it is not planned, it is skipped when its removal starts
// mid-walk, and only a removal still in progress or given up holds the walk's
// peer check. A removed node's record stays in the control plane's list with
// the status removed, and it holds nothing.
//
// design-storagecluster.md §7 is the specification.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/stepclaim"
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

// errNodeSkipped is a planned node the walk passes over without restarting
// it: the control plane stopped listing it, or its removal started after the
// walk was planned. It advances the walk at once and is recorded as a skip
// rather than a restart.
var errNodeSkipped = errors.New("the node left the cluster after the walk was planned")

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
	batch := batchOf(walkOf(ops))
	if len(batch) == 0 {
		// Every node is done, and the caller's IsTerminal check will finish
		// the operation on the next pass. Reporting the step finished here is
		// what gets it there.
		return true, nil
	}

	nodes, err := r.storageNodes(ctx, clusterID)
	if err != nil {
		return false, err
	}

	active, err := r.activeBatch(ctx, ops, nodes, batch)
	if err != nil {
		return false, err
	}
	if len(active) == 0 {
		return false, errNodeSkipped
	}

	switch current {
	case stepCheckingPeers:
		return r.checkPeers(ops, nodes, batch)
	case stepShuttingDownNode:
		return r.shutDownBatch(ctx, ops, clusterID, nodes, batch, active)
	case stepRefreshingPod:
		return eachNode(active, func(nodeID string) (bool, error) {
			return r.refreshPod(ctx, ops, clusterID, nodes, nodeID)
		})
	case stepAwaitingPod:
		return eachNode(active, func(nodeID string) (bool, error) {
			return r.awaitPod(ctx, ops, clusterID, nodes, nodeID)
		})
	case stepRestartingNode:
		return r.restartBatch(ctx, ops, clusterID, nodes, active)
	case stepRebalancing:
		return r.awaitRebalance(ctx, clusterID)
	default:
		return false, fatalf("step %s is not part of the rolling restart", current)
	}
}

// batchOf is the nodes the walk is restarting now: BatchSize nodes of the plan
// starting at NodeIndex, and none once the walk is finished. A walk planned
// before batches existed carries no size and moves one node at a time.
func batchOf(walk *simplyblockv1alpha2.RollingRestartStatus) []string {
	start := int(walk.NodeIndex)
	if start >= len(walk.Nodes) {
		return nil
	}
	end := min(start+max(int(walk.BatchSize), 1), len(walk.Nodes))
	return walk.Nodes[start:end]
}

// activeBatch is the nodes of the batch the walk may act on, in plan order,
// and it records the rest as skipped.
//
// A node the control plane stopped listing, or one that started leaving the
// cluster after the walk was planned, has nothing the walk may do to it.
// Shutting a leaving node down writes over its removal's status, and
// restarting it brings it back into service mid-removal, so every step of it
// is finished as it stands. The skip is written when it is first seen rather
// than when the batch ends, so the record does not depend on the node still
// being gone by then.
func (r *StorageClusterOpsReconciler) activeBatch(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	nodes []utils.NodeStatusResponse,
	batch []string,
) ([]string, error) {
	walk := walkOf(ops)
	var active, skipped []string
	for _, nodeID := range batch {
		status, listed := nodeStatus(nodes, nodeID)
		switch {
		case listed && !utils.NodeIsLeaving(status):
			active = append(active, nodeID)
		case !slices.Contains(walk.Skipped, nodeID):
			skipped = append(skipped, nodeID)
		}
	}
	if len(skipped) == 0 {
		return active, nil
	}

	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		for _, nodeID := range skipped {
			if !slices.Contains(status.RollingRestart.Skipped, nodeID) {
				status.RollingRestart.Skipped = append(status.RollingRestart.Skipped, nodeID)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	for _, nodeID := range skipped {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
			NodeSkipped, NodeSkipped,
			"Node %d/%d (%s) was skipped: it left the cluster after the walk was planned",
			slices.Index(walk.Nodes, nodeID)+1, len(walk.Nodes), nodeID)
	}
	return active, nil
}

// eachNode runs one step for every node of the batch, and the step is finished
// when it is finished for all of them. Every node is visited on every pass, so
// one node still waiting does not keep the step from starting on the next.
func eachNode(nodes []string, step func(nodeID string) (bool, error)) (bool, error) {
	finished := true
	for _, nodeID := range nodes {
		done, err := step(nodeID)
		if err != nil {
			return false, err
		}
		finished = finished && done
	}
	return finished, nil
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
	layout, err := r.stripeLayout(ctx, ops, clusterID)
	if err != nil {
		return err
	}
	staying := make([]utils.NodeStatusResponse, 0, len(nodes))
	for _, node := range nodes {
		if !utils.NodeIsLeaving(lower(node.Status)) {
			staying = append(staying, node)
		}
	}
	// The nodes of one failure domain are listed next to each other, which is
	// what lets a batch be a position and a length. The sort is stable, so
	// within a failure domain, and across a cluster that has none, the order
	// is still the control plane's.
	if layout.failureDomains {
		slices.SortStableFunc(staying, func(a, b utils.NodeStatusResponse) int {
			return failureDomainOf(a) - failureDomainOf(b)
		})
	}
	planned := make([]string, 0, len(staying))
	for _, node := range staying {
		planned = append(planned, node.UUID)
	}
	rollingRestartNodeCount.WithLabelValues(ops.Spec.ClusterRef).Set(float64(len(planned)))
	rollingRestartNodeIndex.WithLabelValues(ops.Spec.ClusterRef).Set(0)

	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.RollingRestart = &simplyblockv1alpha2.RollingRestartStatus{
			Nodes:     planned,
			NodeIndex: 0,
			BatchSize: batchSizeAt(planned, 0, nodes, layout),
		}
	})
}

// stripeLayout is what decides whether a whole failure domain may be down at
// once: whether the cluster places its chunks by failure domain at all, and
// how many chunks a stripe has.
type stripeLayout struct {
	failureDomains bool
	chunks         int
	parity         int
}

// stripeLayout reads the cluster's layout from the StorageCluster and the
// control plane's reading of it.
func (r *StorageClusterOpsReconciler) stripeLayout(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, clusterID string,
) (stripeLayout, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.ClusterRef}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return stripeLayout{}, fmt.Errorf("read cluster %s: %w", ops.Spec.ClusterRef, err)
	}
	if !ptr.BoolFromOrFalse(cluster.Spec.EnableFailureDomains) {
		return stripeLayout{}, nil
	}
	reading, err := r.clusterReading(ctx, clusterID)
	if err != nil {
		return stripeLayout{}, err
	}
	return stripeLayout{
		failureDomains: true,
		chunks:         reading.NDCS + reading.NPCS,
		parity:         reading.NPCS,
	}, nil
}

// batchSizeAt is how many nodes of the plan, starting at start, may be
// restarted together.
//
// It is one unless a whole failure domain may be down at once, which is the
// control plane's own rule read ahead of time (sbcli's
// _check_ftt_allows_node_removal): the chunks of a stripe are spread as evenly
// as possible over the failure domains, so with at least as many failure
// domains as a stripe has chunks each holds one chunk, and every node of one
// failure domain down costs a stripe no more than one node down does. With
// fewer, a failure domain holds several chunks of a stripe and the control
// plane refuses the second shutdown in it, which would leave the walk holding
// with a node down.
//
// The control plane still decides every shutdown when it is asked for. This
// only keeps the walk from asking for one it can see will be refused.
//
// The batch is then the run of planned nodes, from start, in the failure
// domain of the first. A node leaving the cluster is not counted toward the
// failure domains and ends a run, and one in no failure domain makes every
// batch a single node: neither is a layout this can vouch for.
func batchSizeAt(
	planned []string, start int, nodes []utils.NodeStatusResponse, layout stripeLayout,
) int32 {
	if !layout.failureDomains || layout.parity < 1 {
		return 1
	}
	domainOf := map[string]int{}
	domains := map[int]struct{}{}
	for _, node := range nodes {
		if utils.NodeIsLeaving(lower(node.Status)) {
			continue
		}
		domain := failureDomainOf(node)
		if domain < 0 {
			return 1
		}
		domainOf[node.UUID] = domain
		domains[domain] = struct{}{}
	}
	if len(domains) < layout.chunks {
		return 1
	}

	first, listed := domainOf[planned[start]]
	if !listed {
		return 1
	}
	size := int32(1)
	for _, nodeID := range planned[start+1:] {
		if domain, listed := domainOf[nodeID]; !listed || domain != first {
			break
		}
		size++
	}
	return size
}

// failureDomainOf is the node's failure-domain index, and -1 for a node in
// none, which is how the control plane spells it too.
func failureDomainOf(node utils.NodeStatusResponse) int {
	if node.FailureDomain == nil {
		return -1
	}
	return *node.FailureDomain
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
				FailureDomain: ptr.To(dto.FailureDomain),
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
// The peers are every node outside the batch. The nodes of a batch are the
// ones a whole failure domain's loss already accounts for, so one of them
// being down holds nothing: that is the walk resumed part-way through taking
// the batch down.
//
// It performs no side effect. Its deadline is what distinguishes a walk holding
// because the cluster is degraded from one holding because of a bug.
func (r *StorageClusterOpsReconciler) checkPeers(
	ops *simplyblockv1alpha2.StorageClusterOps,
	nodes []utils.NodeStatusResponse,
	batch []string,
) (bool, error) {
	var offline []string
	for _, node := range nodes {
		// A removed node is gone from the cluster, whatever its record says,
		// and nothing it could report would ever satisfy the check. A removal
		// still running, or one the control plane gave up on, does hold it.
		if slices.Contains(batch, node.UUID) || lower(node.Status) == utils.NodeStatusRemoved {
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
		"The walk is holding before %s: %s", describeBatch(batch), strings.Join(offline, ", "))
	return false, nil
}

// describeBatch names a batch in an event: the node, or how many nodes and the
// first of them. A failure domain can hold more nodes than a message has room
// for, and status.rollingRestart lists them.
func describeBatch(batch []string) string {
	if len(batch) == 1 {
		return "node " + batch[0]
	}
	return fmt.Sprintf("the %d nodes starting at %s", len(batch), batch[0])
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

// shutDownBatch asks the control plane to take the batch's nodes down, one
// after another, and is finished when every one of them is offline or beyond.
//
// One after another because the control plane refuses a shutdown while another
// node of the cluster is shutting down. A call is skipped when its node is
// already at or past in_shutdown, so re-entering the step after a crash sends
// no second shutdown.
//
// The control plane decides every shutdown, and it can refuse a later node of
// the batch for a reason that was not there when the batch was sized: a
// rebalance that started, a journal still replicating, a migration running on
// some node. The batch then ends at the nodes already down. They are restarted
// without the refused node, which is walked next as a batch of its own, where
// a refusal holds the walk with nothing down, as it does for any first node.
// Holding here instead would keep part of a failure domain down for as long as
// the refusal lasted. A call that failed for a passing reason is treated the
// same way, because the two cannot be told apart from here and a shorter batch
// costs only time.
func (r *StorageClusterOpsReconciler) shutDownBatch(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	nodes []utils.NodeStatusResponse,
	batch, active []string,
) (bool, error) {
	for position, nodeID := range active {
		status, _ := nodeStatus(nodes, nodeID)
		switch status {
		case utils.NodeStatusOffline, utils.NodeStatusInRestart, utils.NodeStatusRemoved:
			continue
		case utils.NodeStatusInShutdown:
			return false, nil
		}
		claimed, err := r.onceNode(ctx, ops, stepShuttingDownNode, nodeID, func() error {
			if err := r.API.ShutdownNode(ctx, clusterID, nodeID); err != nil {
				return fmt.Errorf("shut down node %s: %w", nodeID, err)
			}
			return nil
		})
		if claimed && err == nil {
			logf.FromContext(ctx).Info("the node was asked to shut down",
				"operation", ops.Name, "node", nodeID)
		}
		if claimed && err != nil && position > 0 {
			return false, r.shortenBatch(ctx, ops, batch, nodeID, err)
		}
		return false, err
	}
	return true, nil
}

// shortenBatch ends the batch before nodeID, whose shutdown was refused.
func (r *StorageClusterOpsReconciler) shortenBatch(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	batch []string,
	nodeID string,
	refusal error,
) error {
	//nolint:gosec // a position within a batch, which a CRD list bounds
	size := int32(slices.Index(batch, nodeID))
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.RollingRestart.BatchSize = size
	})
	if err != nil {
		return err
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
		BatchShortened, BatchShortened,
		"The batch ends at the %d node(s) already down, and node %s is walked after them: %v",
		size, nodeID, refusal)
	return nil
}

// onceNode makes call for one node of the batch under a record of it in
// status.rollingRestart.calls, and reports whether this pass made it.
//
// It is what status.step.claim is to an action with one call per step
// (actions.go, once). A batch makes one call per node, and the passes that
// follow a restart still read every node of the batch as offline until the
// control plane has moved them, so without a record per node each of those
// passes would ask again. The record is written before the call, at the
// version this pass read the operation at, so a pass that lost that race made
// no call. A record whose lease ran out is one whose call did not move the
// node, and the call is made again.
func (r *StorageClusterOpsReconciler) onceNode(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	current step,
	nodeID string,
	call func() error,
) (bool, error) {
	now := time.Now()
	for _, made := range ops.Status.RollingRestart.Calls {
		if made.Node == nodeID && made.Step == current && now.Before(made.LeaseUntil.Time) {
			return false, nil
		}
	}

	read := ops.DeepCopy()
	record := simplyblockv1alpha2.RollingRestartCall{
		Node: nodeID, Step: current, LeaseUntil: metav1.NewTime(now.Add(claimLease)),
	}
	calls := slices.DeleteFunc(slices.Clone(ops.Status.RollingRestart.Calls),
		func(made simplyblockv1alpha2.RollingRestartCall) bool { return made.Node == nodeID })
	ops.Status.RollingRestart.Calls = append(calls, record)

	err := r.Status().Patch(ctx, ops,
		client.MergeFromWithOptions(read, client.MergeFromWithOptimisticLock{}))
	if err != nil {
		*ops = *read
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("record the call for node %s: %w", nodeID, err)
	}
	// An API server whose CRD predates the field drops the record and answers
	// success, and the call would then be made again on every pass.
	stored := ops.Status.RollingRestart != nil && slices.ContainsFunc(ops.Status.RollingRestart.Calls,
		func(made simplyblockv1alpha2.RollingRestartCall) bool {
			return made.Node == nodeID && made.Step == current &&
				made.LeaseUntil.Unix() == record.LeaseUntil.Unix()
		})
	if !stored {
		return false, fmt.Errorf("%w: StorageClusterOps %s", stepclaim.ErrClaimNotStored,
			client.ObjectKeyFromObject(ops))
	}
	return true, call()
}

// refreshPod deletes the node's storage-node pod, which is what forces the
// image to be pulled again, and is finished when the pod is gone. It is entered
// only when spec.rollingRestart.refreshSNodeAPI is set.
func (r *StorageClusterOpsReconciler) refreshPod(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	if peerCluster, remote, err := r.remoteNodeSite(ctx, ops, nodeID); err != nil || remote {
		if err != nil {
			return false, err
		}
		// Asking is the step: the other site deletes the pod if it predates
		// the operation, and the next step waits for its replacement.
		_, err := r.refreshOnPeer(ctx, ops, peerCluster, clusterID, nodeID)
		return err == nil, err
	}
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
	// The DaemonSet replaces a deleted pod at once, so the step never sees the
	// node without one. A pod created after the walk started is that
	// replacement, and deleting it too would replace the pod again on every
	// pass until the step's deadline.
	if refreshedSince(pod, ops) {
		return true, nil
	}
	if err := r.Delete(ctx, pod); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return false, nil
}

// refreshedSince reports whether pod was created after the operation started,
// which is what makes it the replacement rather than the pod to replace.
func refreshedSince(pod *corev1.Pod, ops *simplyblockv1alpha2.StorageClusterOps) bool {
	return !pod.CreationTimestamp.Time.Before(ops.CreationTimestamp.Time)
}

// awaitPod waits for the replacement pod to be Ready. It performs no side
// effect: the DaemonSet is what creates the replacement.
func (r *StorageClusterOpsReconciler) awaitPod(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	nodes []utils.NodeStatusResponse,
	nodeID string,
) (bool, error) {
	if peerCluster, remote, err := r.remoteNodeSite(ctx, ops, nodeID); err != nil || remote {
		if err != nil {
			return false, err
		}
		return r.refreshOnPeer(ctx, ops, peerCluster, clusterID, nodeID)
	}
	pod, err := r.storageNodePod(ctx, ops, nodes, nodeID)
	if err != nil {
		return false, err
	}
	if pod == nil || !pod.DeletionTimestamp.IsZero() || !refreshedSince(pod, ops) {
		return false, nil
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// restartBatch asks the control plane to bring the batch's nodes back, and is
// finished when every one of them reports online. A call is skipped when its
// node is already restarting or online.
//
// Every node is asked on the same pass. The control plane restarts the nodes
// of a failure domain in parallel only while none of them is online, so asking
// for them one by one would restart the first alone and then the rest in
// sequence behind it.
func (r *StorageClusterOpsReconciler) restartBatch(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	clusterID string,
	nodes []utils.NodeStatusResponse,
	active []string,
) (bool, error) {
	finished := true
	var failures []error
	for _, nodeID := range active {
		status, _ := nodeStatus(nodes, nodeID)
		if status == utils.NodeStatusOnline {
			continue
		}
		finished = false
		if status == utils.NodeStatusInRestart {
			continue
		}
		claimed, err := r.onceNode(ctx, ops, stepRestartingNode, nodeID, func() error {
			if err := r.API.RestartNode(ctx, clusterID, nodeID); err != nil {
				return fmt.Errorf("restart node %s: %w", nodeID, err)
			}
			return nil
		})
		if claimed && err == nil {
			logf.FromContext(ctx).Info("the node was asked to restart",
				"operation", ops.Name, "node", nodeID)
		}
		// One node's call failing is not a reason to leave the others unasked.
		if err != nil {
			failures = append(failures, err)
		}
	}
	return finished, errors.Join(failures...)
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
//
// A node of the batch the walk passed over is already in
// status.rollingRestart.skipped, written when the skip was first seen
// (activeBatch), so neither the events nor the success message count it among
// the restarted nodes.
//
// The next batch is sized here, from the fleet as it stands now, and written
// with the index: the two are one position, and a pass that crashed between
// them would resume a batch of the wrong length.
func (r *StorageClusterOpsReconciler) advanceWalk(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	machine *statemachine.Machine[step],
) (ctrl.Result, error) {
	walk := walkOf(ops)
	batch := batchOf(walk)
	//nolint:gosec // the length of a batch, which a CRD list bounds
	next := walk.NodeIndex + int32(len(batch))

	nextSize := int32(0)
	if int(next) < len(walk.Nodes) {
		size, err := r.sizeBatch(ctx, ops, walk.Nodes, int(next))
		if err != nil {
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
		}
		nextSize = size
	}

	for offset, nodeID := range batch {
		if slices.Contains(walk.Skipped, nodeID) {
			continue
		}
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
			NodeRestarted, NodeRestarted,
			"Node %d/%d (%s) was restarted",
			int(walk.NodeIndex)+offset+1, len(walk.Nodes), nodeID)
	}

	if int(next) >= len(walk.Nodes) {
		err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
			status.RollingRestart.NodeIndex = next
			status.RollingRestart.BatchSize = 0
			status.RollingRestart.Calls = nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		rollingRestartNodeIndex.WithLabelValues(ops.Spec.ClusterRef).Set(float64(next))
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
			r.successMessage(ops))
	}

	// Write-ahead in both halves: the index and the step move together, and
	// the step is recorded before the next batch is touched. The calls made
	// for the batch that just finished go with it.
	machine.Reset()
	deadline := metav1.NewTime(time.Now().Add(checkingPeersDeadline))
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.RollingRestart.NodeIndex = next
		status.RollingRestart.BatchSize = nextSize
		status.RollingRestart.Calls = nil
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

// sizeBatch is how many planned nodes, starting at start, the walk restarts
// together, from the fleet and the cluster's layout as they stand now.
func (r *StorageClusterOpsReconciler) sizeBatch(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, planned []string, start int,
) (int32, error) {
	clusterID, err := r.clusterUUIDFor(ctx, ops)
	if err != nil {
		return 0, err
	}
	layout, err := r.stripeLayout(ctx, ops, clusterID)
	if err != nil {
		return 0, err
	}
	if !layout.failureDomains {
		return 1, nil
	}
	nodes, err := r.storageNodes(ctx, clusterID)
	if err != nil {
		return 0, err
	}
	return batchSizeAt(planned, start, nodes, layout), nil
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

// remoteNodeSite reports whether nodeID is a node of the other site of a
// stretched cluster, whose pod runs there, and that site's managed cluster.
func (r *StorageClusterOpsReconciler) remoteNodeSite(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, nodeID string,
) (string, bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.ClusterRef}, &cluster); err != nil {
		return "", false, client.IgnoreNotFound(err)
	}
	if cluster.Spec.Stretched == nil {
		return "", false, nil
	}
	var storageNodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &storageNodes, client.InNamespace(ops.Namespace)); err != nil {
		return "", false, fmt.Errorf("list the storage nodes: %w", err)
	}
	for i := range storageNodes.Items {
		if n := &storageNodes.Items[i]; n.Status.UUID == nodeID {
			return cluster.Spec.Stretched.PeerCluster, n.IsRemote(), nil
		}
	}
	return "", false, nil
}

// refreshOnPeer asks the other site's operator to refresh the node's pod, with
// the operation's start as the line the replacement must be newer than, which
// makes asking again harmless. It reports whether that replacement is Ready.
func (r *StorageClusterOpsReconciler) refreshOnPeer(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, peerCluster, clusterID, nodeID string,
) (bool, error) {
	if r.Peer == nil || peerCluster == "" {
		return false, fmt.Errorf("node %s runs on the other site, whose operator only the site link reaches, "+
			"and this operator has no site link to it", nodeID)
	}
	refreshed, err := r.Peer.RefreshNode(ctx, peerCluster, clusterID, nodeID, ops.CreationTimestamp.Time)
	if err != nil {
		return false, fmt.Errorf("refresh the pod of node %s on the other site: %w", nodeID, err)
	}
	return refreshed, nil
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
	// A batch is named by its range. A failure domain can hold more nodes than
	// a message has room for, and status.rollingRestart.nodes lists them.
	if batch := batchOf(walk); len(batch) > 1 {
		return fmt.Sprintf("Nodes %d-%d/%d: %s",
			position+1, position+len(batch), len(walk.Nodes), current)
	}
	return fmt.Sprintf("Node %d/%d (%s): %s",
		position+1, len(walk.Nodes), walk.Nodes[position], current)
}

// lower is strings.ToLower, named for what the control plane's status strings
// need rather than for what it does, because every comparison against them has
// to make the same allowance.
func lower(status string) string { return strings.ToLower(status) }
