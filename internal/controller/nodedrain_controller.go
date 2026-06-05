/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// drainNodeLabelKey is patched onto the storage pod on a draining node so the
	// per-node PodDisruptionBudget can select it precisely.
	drainNodeLabelKey = "simplyblock.io/drain-node"

	// drainPDBPrefix is the prefix used for per-node PodDisruptionBudget names.
	drainPDBPrefix = "simplyblock-drain-"

	// managerPDBName is the name of the temporary PDB that protects the manager
	// pod from eviction while it sets up storage PDB protection on its own node.
	managerPDBName = "simplyblock-operator-self"

	nodeStatusOffline    = "offline"
	nodeStatusInRestart  = "in_restart"
	nodeStatusInShutdown = "in_shutdown"
)

// NodeDrainCoordinatorReconciler coordinates graceful simplyblock node shutdown
// and restart during Kubernetes node drain events such as rolling OS upgrades.
//
// The controller implements a requeue-based state machine tracked in
// StorageNode.status.drainCoordination. The full per-node flow is:
//
//  1. Detect   – k8s node cordoned (spec.unschedulable=true); wait for drain slot
//  2. Shutdown – label storage pod, create blocking PDB (maxUnavailable=0),
//     call simplyblock shutdown API
//  3. Confirm  – poll until backend node status == nodeStatusOffline
//  4. Release  – relax PDB to maxUnavailable=1; drain proceeds and pod is evicted
//  5. Reboot   – node reboots (OS upgrade applied); wait for SPDK to restart
//  6. Restart  – call simplyblock restart API once snode/info is reachable
//  7. Confirm  – poll until backend node status == "online"
//  8. Cleanup  – delete PDB, remove drain label, mark phase "complete"
//
// Concurrency is controlled by StorageCluster.spec.maxFaultTolerance: at most
// that many nodes per StorageNode CR may be in the active drain window
// (phases shutdown_called, draining, restart_called) simultaneously.
//
// OpenShift MachineConfigPool (MCP) pausing is not implemented here; instead,
// set MachineConfigPool.spec.maxUnavailable to a high value and rely on the
// PDB as the actual throttle. MCP integration can be added via a dynamic client
// watching machineconfiguration.openshift.io/v1.MachineConfigPool objects.
type NodeDrainCoordinatorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ManagerNodeName is the Kubernetes node this manager pod is running on,
	// injected via the downward API (spec.nodeName). When set, the controller
	// will create a temporary self-PDB to prevent premature eviction while
	// setting up storage PDB protection on the same node.
	ManagerNodeName  string
	TLSEnabled       bool
	TLSMutualEnabled bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func (r *NodeDrainCoordinatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snCR := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(ctx, req.NamespacedName, snCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Cluster credentials must be available before we can call the simplyblock API.
	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		// Not yet provisioned; nothing to gate.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Clean up any stale manager self-PDB left over from a previous crash
	// (e.g., manager was killed after creating its PDB but before deleting it).
	if r.ManagerNodeName != "" {
		r.cleanupManagerPDBIfStale(ctx, snCR)
	}

	// MaxFaultTolerance caps how many nodes can be simultaneously in the drain window.
	clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		log.Info("Cluster CR not ready, requeuing", "cluster", snCR.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	maxFaultTolerance := 1
	if clusterCR.Status.MaxFaultTolerance != nil && *clusterCR.Status.MaxFaultTolerance > 0 {
		maxFaultTolerance = int(*clusterCR.Status.MaxFaultTolerance)
	}

	apiClient := webapi.NewClient()
	patch := client.MergeFrom(snCR.DeepCopy())
	nextRequeue := time.Duration(0)

	// Pre-create blocking PDBs for worker nodes that are already online in the
	// backend. This protects SPDK/FDB/webappapi pods from being evicted by MCP
	// before the drain state machine fires, while avoiding blocking the kubelet
	// reboot that is required when a new node applies its KubeletConfig/MachineConfig
	// for the first time (i.e. node add flow).
	for _, workerName := range snCR.Spec.WorkerNodes {
		// Skip nodes that are already in an active drain — their PDB lifecycle
		// is managed by the drain state machine (deleted after offline confirmed).
		state := getDrainState(snCR, workerName)
		if state != nil {
			switch state.Phase {
			case simplyblockv1alpha1.DrainPhaseShutdownCalled,
				simplyblockv1alpha1.DrainPhaseDraining,
				simplyblockv1alpha1.DrainPhaseRestartCalled:
				continue
			}
		}
		// Only pre-create the PDB once the storage node is online — during the
		// initial node-add flow the worker must be allowed to reboot freely to
		// apply KubeletConfig/MachineConfig changes.
		if !isWorkerOnline(snCR, workerName) {
			continue
		}
		if err := r.labelStoragePod(ctx, snCR.Namespace, workerName); err != nil {
			log.Error(err, "Failed to pre-label storage pods", "node", workerName)
		}
		if err := r.ensurePDB(ctx, snCR.Namespace, workerName, 0); err != nil {
			log.Error(err, "Failed to pre-create blocking PDB", "node", workerName)
		}
	}

	for _, workerName := range snCR.Spec.WorkerNodes {
		requeue, shouldBreak := r.processWorker(
			ctx, snCR, workerName, apiClient, clusterUUID, clusterSecret, maxFaultTolerance,
		)
		if requeue > 0 && (nextRequeue == 0 || requeue < nextRequeue) {
			nextRequeue = requeue
		}
		if shouldBreak {
			break
		}
	}

	if err := r.Status().Patch(ctx, snCR, patch); err != nil {
		log.Error(err, "Failed to patch drain coordination status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if nextRequeue > 0 {
		return ctrl.Result{RequeueAfter: nextRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// processWorker handles drain coordination for a single worker node in one
// reconcile pass. It returns the desired requeue duration and whether the
// outer worker loop should stop processing further nodes.
func (r *NodeDrainCoordinatorReconciler) processWorker(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	workerName string,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
	maxFaultTolerance int,
) (requeue time.Duration, shouldBreak bool) {
	log := logf.FromContext(ctx)

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: workerName}, node); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get worker node", "node", workerName)
		}
		return 0, false
	}

	state := getDrainState(snCR, workerName)
	cordoned := node.Spec.Unschedulable

	// Normal operation: no cordon, no state.
	if !cordoned && state == nil {
		return 0, false
	}

	// Node was uncordoned (possibly after reboot + MCO uncordon).
	if !cordoned && state != nil {
		return r.processUncordoned(ctx, snCR, workerName, state, apiClient, clusterUUID, clusterSecret)
	}

	// Node is cordoned: initialise state if first observation.
	if state == nil {
		// Do not start drain coordination for a node that has never been online.
		// MCP cordons new nodes for the initial KubeletConfig/MachineConfig reboot
		// before the storage node is added — triggering drain here would create a
		// blocking PDB and prevent the reboot from completing.
		if !isWorkerOnline(snCR, workerName) {
			log.Info("Node cordoned but not yet online — skipping drain coordination (node add reboot)", "node", workerName)
			return 0, false
		}
		log.Info("Node cordoned — starting drain coordination", "node", workerName)
		newState := simplyblockv1alpha1.NodeDrainState{
			Hostname:  workerName,
			Phase:     simplyblockv1alpha1.DrainPhaseDetected,
			StartedAt: metav1.Now(),
		}
		upsertDrainState(snCR, newState)
		state = getDrainState(snCR, workerName)
	}

	prevPhase := state.Phase
	advRequeue, advErr := r.advanceStateMachine(
		ctx, snCR, state, apiClient, clusterUUID, clusterSecret, maxFaultTolerance,
	)
	if advErr != nil {
		log.Error(advErr, "Drain state machine error", "node", workerName, "phase", state.Phase)
		state.Phase = simplyblockv1alpha1.DrainPhaseFailed
		state.Message = advErr.Error()
	}
	upsertDrainState(snCR, *state)

	// If this node just entered the active drain window, stop processing
	// further workers in this reconcile pass so the slot gate sees the
	// correct active count on the next requeue.
	if prevPhase == simplyblockv1alpha1.DrainPhaseDetected &&
		state.Phase == simplyblockv1alpha1.DrainPhaseShutdownCalled {
		log.Info("Node entered active drain window; deferring remaining workers to next reconcile", "node", workerName)
		if advRequeue == 0 {
			advRequeue = 10 * time.Second
		}
		return advRequeue, true
	}

	// If this node just completed, stop processing further workers so the
	// next node's shutdown only begins once completion is persisted.
	if prevPhase != simplyblockv1alpha1.DrainPhaseComplete &&
		prevPhase != simplyblockv1alpha1.DrainPhaseFailed &&
		(state.Phase == simplyblockv1alpha1.DrainPhaseComplete || state.Phase == simplyblockv1alpha1.DrainPhaseFailed) {
		log.Info("Node drain complete; deferring next node shutdown to next reconcile", "node", workerName, "phase", state.Phase)
		if advRequeue == 0 {
			advRequeue = 5 * time.Second
		}
		return advRequeue, true
	}

	return advRequeue, false
}

// processUncordoned handles a worker node that has been uncordoned while drain
// state is still present (e.g., after reboot or admin intervention).
func (r *NodeDrainCoordinatorReconciler) processUncordoned(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	workerName string,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) (requeue time.Duration, shouldBreak bool) {
	log := logf.FromContext(ctx)

	switch state.Phase {
	case simplyblockv1alpha1.DrainPhaseComplete, simplyblockv1alpha1.DrainPhaseFailed:
		removeDrainState(snCR, workerName)
		log.Info("Cleared terminal drain state after uncordon", "node", workerName, "phase", state.Phase)
	case simplyblockv1alpha1.DrainPhaseDraining:
		// Node uncordoned after reboot — call restart.
		log.Info("Node uncordoned after drain; calling restart", "node", workerName)
		requeue = r.handleDraining(ctx, snCR, state, apiClient, clusterUUID, clusterSecret)
		upsertDrainState(snCR, *state)
	case simplyblockv1alpha1.DrainPhaseRestartCalled:
		// Node uncordoned while restart polling is in progress — this is expected
		// (MCP uncordons after reboot). Continue polling for online + health.
		log.Info("Node uncordoned during restart polling; continuing", "node", workerName)
		prevPhase := state.Phase
		var err error
		requeue, err = r.handleRestartCalled(ctx, snCR, state, apiClient, clusterUUID, clusterSecret)
		if err != nil {
			log.Error(err, "handleRestartCalled failed after uncordon", "node", workerName)
		}
		upsertDrainState(snCR, *state)
		// If this node just completed, stop the loop so the next node's shutdown
		// only begins on the next reconcile cycle after completion is persisted.
		if prevPhase != simplyblockv1alpha1.DrainPhaseComplete &&
			(state.Phase == simplyblockv1alpha1.DrainPhaseComplete || state.Phase == simplyblockv1alpha1.DrainPhaseFailed) {
			log.Info("Node drain complete via uncordon path; deferring next node to next reconcile", "node", workerName)
			if requeue == 0 {
				requeue = 5 * time.Second
			}
			return requeue, true
		}
	case simplyblockv1alpha1.DrainPhaseShutdownCalled:
		// Node uncordoned while waiting for backend to go offline — continue polling.
		log.Info("Node uncordoned during shutdown polling; continuing", "node", workerName)
		var err error
		requeue, err = r.handleShutdownCalled(ctx, snCR, state, apiClient, clusterUUID, clusterSecret)
		if err != nil {
			log.Error(err, "handleShutdownCalled failed after uncordon", "node", workerName)
		}
		upsertDrainState(snCR, *state)
	default:
		// Unexpected uncordon mid-sequence (e.g., admin intervention at detected phase).
		log.Info("Node uncordoned mid-drain; aborting coordination", "node", workerName, "phase", state.Phase)
		if cleanupErr := r.cleanupDrainResources(ctx, snCR.Namespace, workerName); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to clean up drain resources on abort", "node", workerName)
		}
		removeDrainState(snCR, workerName)
	}
	return requeue, false
}

// advanceStateMachine dispatches to the handler for the current drain phase.
func (r *NodeDrainCoordinatorReconciler) advanceStateMachine(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
	maxFaultTolerance int,
) (time.Duration, error) {
	switch state.Phase {
	case simplyblockv1alpha1.DrainPhaseDetected:
		return r.handleDetected(ctx, snCR, state, apiClient, clusterUUID, clusterSecret, maxFaultTolerance)
	case simplyblockv1alpha1.DrainPhaseShutdownCalled:
		return r.handleShutdownCalled(ctx, snCR, state, apiClient, clusterUUID, clusterSecret)
	case simplyblockv1alpha1.DrainPhaseDraining:
		// Restart is only triggered from the uncordon path — not while the node
		// is still cordoned. Just wait here.
		state.Message = "waiting for node to be uncordoned after reboot"
		return 15 * time.Second, nil
	case simplyblockv1alpha1.DrainPhaseRestartCalled:
		return r.handleRestartCalled(ctx, snCR, state, apiClient, clusterUUID, clusterSecret)
	case simplyblockv1alpha1.DrainPhaseComplete, simplyblockv1alpha1.DrainPhaseFailed:
		return 0, nil
	}
	return 0, fmt.Errorf("unknown drain phase %q", state.Phase)
}

// handleDetected waits for a drain slot then initiates the simplyblock shutdown.
// Gate: number of nodes currently in {shutdown_called, draining, restart_called}
// must be less than MaxFaultTolerance.
//
// Importantly, the storage pod is labelled and a blocking PDB (maxUnavailable=0)
// is created BEFORE the slot check, so that MCP/kubectl-drain cannot evict the
// pod while this node is queued behind another drain in progress.
func (r *NodeDrainCoordinatorReconciler) handleDetected(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
	maxFaultTolerance int,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	// If the manager is running on the node being drained, protect it first with
	// a self-PDB so MCP cannot evict it while we set up storage protection below.
	managerOnThisNode := r.ManagerNodeName != "" && r.ManagerNodeName == state.Hostname
	if managerOnThisNode {
		if err := r.ensureManagerPDB(ctx, snCR.Namespace); err != nil {
			return 10 * time.Second, fmt.Errorf("create manager self-PDB: %w", err)
		}
		log.Info("Manager is on draining node; created self-PDB to prevent premature eviction", "node", state.Hostname)
	}

	// Label the storage pod and install a blocking PDB immediately — before the
	// slot check — so MCP cannot evict this node's storage pod while it is
	// waiting for a drain slot behind another in-progress drain.
	if err := r.labelStoragePod(ctx, snCR.Namespace, state.Hostname); err != nil {
		return 10 * time.Second, fmt.Errorf("label storage pod: %w", err)
	}
	if err := r.ensurePDB(ctx, snCR.Namespace, state.Hostname, 0); err != nil {
		return 10 * time.Second, fmt.Errorf("create blocking PDB: %w", err)
	}

	// Storage is now protected. Release the manager self-PDB so MCP can evict
	// and reschedule the manager onto another node, from which it will continue
	// drain coordination with the storage PDB already in place.
	if managerOnThisNode {
		if err := r.deleteManagerPDB(ctx, snCR.Namespace); err != nil {
			log.Error(err, "Failed to delete manager self-PDB; will retry")
			return 10 * time.Second, nil
		}
		log.Info("Storage PDB in place; released manager self-PDB — manager will migrate to another node", "node", state.Hostname)
	}

	activeDrains := countActiveDrains(ctx, snCR, apiClient, clusterUUID, clusterSecret)
	if activeDrains >= maxFaultTolerance {
		state.Message = fmt.Sprintf("waiting for drain slot (%d/%d active)", activeDrains, maxFaultTolerance)
		log.Info("No drain slot available, blocking PDB in place", "node", state.Hostname, "active", activeDrains, "max", maxFaultTolerance)
		return 10 * time.Second, nil
	}

	nodeUUIDs := findAllNodeUUIDs(snCR, state.Hostname)
	if len(nodeUUIDs) == 0 {
		return 15 * time.Second, fmt.Errorf("node %s not yet registered with backend (UUID missing)", state.Hostname)
	}

	// Check backend status for all nodes before calling shutdown — they may
	// already be in_shutdown or offline from a previous (possibly crashed)
	// reconcile run. Handles multi-socket workers where each NUMA socket has
	// its own backend node entry.
	type nodeStatusEntry struct{ uuid, status string }
	statuses := make([]nodeStatusEntry, 0, len(nodeUUIDs))
	for _, uuid := range nodeUUIDs {
		nodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, uuid)
		if err != nil {
			log.Info("Failed to query backend node status before shutdown, retrying", "node", state.Hostname, "err", err)
			return 15 * time.Second, nil
		}
		statuses = append(statuses, nodeStatusEntry{uuid: uuid, status: nodeInfo.Status})
	}

	// Fast-path: all nodes already offline — skip shutdown, allow drain.
	allOffline := true
	for _, s := range statuses {
		if s.status != nodeStatusOffline {
			allOffline = false
			break
		}
	}
	if allOffline {
		log.Info("All backend nodes already offline; skipping shutdown API call", "node", state.Hostname)
		if err := r.cleanupPDB(ctx, snCR.Namespace, state.Hostname); err != nil {
			return 10 * time.Second, fmt.Errorf("delete PDB: %w", err)
		}
		state.Phase = simplyblockv1alpha1.DrainPhaseDraining
		state.ActiveNodeUUID = ""
		state.Message = "all backend nodes already offline; drain allowed"
		return 15 * time.Second, nil
	}

	// Find the first node that is not yet offline and initiate shutdown on it.
	// Shutdown proceeds one node at a time: handleShutdownCalled waits for each
	// node to go offline before calling shutdown on the next socket node.
	for _, s := range statuses {
		if s.status == nodeStatusOffline {
			continue
		}
		state.ActiveNodeUUID = s.uuid
		if s.status == "in_shutdown" {
			log.Info("First pending node already in_shutdown; advancing to shutdown_called", "node", state.Hostname, "nodeUUID", s.uuid)
			state.Message = fmt.Sprintf("node %s already in_shutdown; waiting for offline", s.uuid)
		} else {
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/shutdown", clusterUUID, s.uuid)
			body, httpStatus, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
			if err != nil || httpStatus >= 300 {
				if err == nil {
					err = fmt.Errorf("status %d: %s", httpStatus, string(body))
				}
				return 15 * time.Second, fmt.Errorf("shutdown API for node %s: %w", s.uuid, err)
			}
			log.Info("Shutdown API called for first node", "node", state.Hostname, "nodeUUID", s.uuid)
			state.Message = fmt.Sprintf("shutdown called for node %s; waiting for offline", s.uuid)
		}
		break
	}

	state.Phase = simplyblockv1alpha1.DrainPhaseShutdownCalled
	return 10 * time.Second, nil
}

// handleShutdownCalled polls the active node until it is nodeStatusOffline, then calls
// shutdown on the next socket node. Only after every node on the worker is
// offline is the PDB removed and drain allowed to proceed.
func (r *NodeDrainCoordinatorReconciler) handleShutdownCalled(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	nodeUUIDs := findAllNodeUUIDs(snCR, state.Hostname)
	if len(nodeUUIDs) == 0 {
		return 10 * time.Second, nil
	}

	// Fall back to the first UUID if ActiveNodeUUID is somehow unset.
	activeUUID := state.ActiveNodeUUID
	if activeUUID == "" {
		activeUUID = nodeUUIDs[0]
		state.ActiveNodeUUID = activeUUID
	}

	nodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, activeUUID)
	if err != nil {
		log.Info("Failed to poll backend node status, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}
	if nodeInfo.Status != nodeStatusOffline {
		state.Message = fmt.Sprintf("waiting for node %s to go offline, current: %s", activeUUID, nodeInfo.Status)
		log.Info("Node not offline yet", "node", state.Hostname, "nodeUUID", activeUUID, "status", nodeInfo.Status)
		return 10 * time.Second, nil
	}

	log.Info("Node offline", "node", state.Hostname, "nodeUUID", activeUUID)

	// Advance to the next socket node in sequence.
	if nextUUID := nextUUIDInList(nodeUUIDs, activeUUID); nextUUID != "" {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/shutdown", clusterUUID, nextUUID)
		body, httpStatus, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
		if err != nil || httpStatus >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", httpStatus, string(body))
			}
			return 10 * time.Second, fmt.Errorf("shutdown API for node %s: %w", nextUUID, err)
		}
		log.Info("Shutdown API called for next node", "node", state.Hostname, "nodeUUID", nextUUID)
		state.ActiveNodeUUID = nextUUID
		state.Message = fmt.Sprintf("shutdown called for node %s; waiting for offline", nextUUID)
		return 10 * time.Second, nil
	}

	// All nodes offline — delete the PDB entirely to allow MCP to evict all pods.
	if err := r.cleanupPDB(ctx, snCR.Namespace, state.Hostname); err != nil {
		return 10 * time.Second, fmt.Errorf("delete PDB: %w", err)
	}

	log.Info("All nodes offline; PDB removed — drain can proceed", "node", state.Hostname)
	state.Phase = simplyblockv1alpha1.DrainPhaseDraining
	state.ActiveNodeUUID = ""
	state.Message = "shutdown confirmed; drain allowed"
	return 15 * time.Second, nil
}

// handleDraining is called once the node has been uncordoned and is Ready after
// reboot. It verifies SPDK is reachable before calling the simplyblock restart API.
func (r *NodeDrainCoordinatorReconciler) handleDraining(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) time.Duration {
	log := logf.FromContext(ctx)

	// Verify SPDK is reachable before calling restart.
	if err := checkNodeInfoReachable(ctx, state.Hostname, snCR.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
		state.Message = "waiting for SPDK to become reachable after reboot"
		log.Info("SPDK not yet reachable, will retry", "node", state.Hostname)
		return 15 * time.Second
	}

	nodeUUIDs := findAllNodeUUIDs(snCR, state.Hostname)
	if len(nodeUUIDs) == 0 {
		return 15 * time.Second
	}

	// Call restart for the first node only. handleRestartCalled will wait for it
	// to be online+healthy, then call restart on the next socket node in sequence.
	firstUUID := nodeUUIDs[0]

	// Hold off if the secondary is currently restarting or shutting down.
	busy, err := isPeerBusy(ctx, apiClient, clusterSecret, clusterUUID, firstUUID)
	if err != nil {
		log.Info("Failed to check peer node status, retrying", "node", state.Hostname, "nodeUUID", firstUUID, "err", err)
		return 15 * time.Second
	}
	if busy {
		state.Message = fmt.Sprintf("waiting for peer node %s to finish restart/shutdown", firstUUID)
		log.Info("Peer node is busy; deferring restart", "node", state.Hostname, "nodeUUID", firstUUID)
		return 15 * time.Second
	}

	firstNodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, firstUUID)
	if err != nil {
		log.Info("Failed to check node status before restart, retrying", "node", state.Hostname, "nodeUUID", firstUUID, "err", err)
		return 15 * time.Second
	}
	if firstNodeInfo.Status == nodeStatusInRestart {
		log.Info("Node already in_restart; advancing to restart_called without re-calling API", "node", state.Hostname, "nodeUUID", firstUUID)
		state.ActiveNodeUUID = firstUUID
		state.Phase = simplyblockv1alpha1.DrainPhaseRestartCalled
		state.Message = fmt.Sprintf("node %s already in_restart; waiting for online", firstUUID)
		return 10 * time.Second
	}

	restartPayload := map[string]any{
		"force":        true,
		"node_address": utils.StorageNodeAPIAddress(state.Hostname, snCR.Namespace),
	}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, firstUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, restartPayload)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "Restart API failed, will retry", "node", state.Hostname, "nodeUUID", firstUUID)
		return 15 * time.Second
	}
	log.Info("Restart API called for first node", "node", state.Hostname, "nodeUUID", firstUUID)

	state.ActiveNodeUUID = firstUUID
	state.Phase = simplyblockv1alpha1.DrainPhaseRestartCalled
	state.Message = fmt.Sprintf("restart called for node %s; waiting for online", firstUUID)
	return 10 * time.Second
}

// handleRestartCalled polls the backend until ALL nodes on the worker are
// "online" and healthy (covers multi-socket workers with one backend node per
// NUMA socket). Only then is the worker considered ready and the next worker
// allowed to drain, subject to maxFaultTolerance.
func (r *NodeDrainCoordinatorReconciler) handleRestartCalled(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	nodeUUIDs := findAllNodeUUIDs(snCR, state.Hostname)
	if len(nodeUUIDs) == 0 {
		return 10 * time.Second, nil
	}

	// First verify the Kubernetes node itself is Ready.
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: state.Hostname}, node); err != nil {
		log.Info("Failed to get node for readiness check, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}
	if !isNodeReady(node) {
		state.Message = "waiting for Kubernetes node to become Ready"
		log.Info("Node not Ready yet", "node", state.Hostname)
		return 10 * time.Second, nil
	}

	// Poll the active node until it is online and healthy, then call restart on
	// the next socket node. The next worker is only allowed to drain once every
	// socket node on this worker is back online and healthy.
	activeUUID := state.ActiveNodeUUID
	if activeUUID == "" {
		activeUUID = nodeUUIDs[0]
		state.ActiveNodeUUID = activeUUID
	}

	nodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, activeUUID)
	if err != nil {
		log.Info("Failed to poll backend node status after restart, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}
	if nodeInfo.Status != "online" {
		state.Message = fmt.Sprintf("Kubernetes node Ready; waiting for node %s backend online, current: %s", activeUUID, nodeInfo.Status)
		log.Info("Node not online yet", "node", state.Hostname, "nodeUUID", activeUUID, "status", nodeInfo.Status)
		return 10 * time.Second, nil
	}
	if !nodeInfo.Healthy {
		state.Message = fmt.Sprintf("node %s online; waiting for health check to pass", activeUUID)
		log.Info("Storage node health check not passing yet", "node", state.Hostname, "nodeUUID", activeUUID)
		return 10 * time.Second, nil
	}

	log.Info("Node online and healthy", "node", state.Hostname, "nodeUUID", activeUUID)

	// Advance to the next socket node in sequence.
	if nextUUID := nextUUIDInList(nodeUUIDs, activeUUID); nextUUID != "" {
		// Hold off if the next node's secondary is currently restarting or shutting down.
		busy, err := isPeerBusy(ctx, apiClient, clusterSecret, clusterUUID, nextUUID)
		if err != nil {
			log.Info("Failed to check peer node status, retrying", "node", state.Hostname, "nodeUUID", nextUUID, "err", err)
			return 15 * time.Second, nil
		}
		if busy {
			state.Message = fmt.Sprintf("waiting for peer node %s to finish restart/shutdown", nextUUID)
			log.Info("Peer node is busy; deferring restart", "node", state.Hostname, "nodeUUID", nextUUID)
			return 15 * time.Second, nil
		}

		nextNodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, nextUUID)
		if err != nil {
			log.Info("Failed to check node status before restart, retrying", "node", state.Hostname, "nodeUUID", nextUUID, "err", err)
			return 15 * time.Second, nil
		}
		if nextNodeInfo.Status == nodeStatusInRestart {
			log.Info("Next node already in_restart; skipping restart API call", "node", state.Hostname, "nodeUUID", nextUUID)
			state.ActiveNodeUUID = nextUUID
			state.Message = fmt.Sprintf("node %s already in_restart; waiting for online", nextUUID)
			return 10 * time.Second, nil
		}

		restartPayload := map[string]any{
			"force":        true,
			"node_address": utils.StorageNodeAPIAddress(state.Hostname, snCR.Namespace),
		}
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, nextUUID)
		body, httpStatus, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, restartPayload)
		if err != nil || httpStatus >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", httpStatus, string(body))
			}
			log.Error(err, "Restart API failed for next node, will retry", "node", state.Hostname, "nodeUUID", nextUUID)
			return 15 * time.Second, nil
		}
		log.Info("Restart API called for next node", "node", state.Hostname, "nodeUUID", nextUUID)
		state.ActiveNodeUUID = nextUUID
		state.Message = fmt.Sprintf("restart called for node %s; waiting for online", nextUUID)
		return 10 * time.Second, nil
	}

	// All socket nodes are back online — wait for the cluster to finish
	// rebalancing before releasing this drain slot to the next worker.
	rebalancing, err := isClusterRebalancing(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		log.Info("Failed to check cluster rebalancing status, retrying", "node", state.Hostname, "err", err)
		return 30 * time.Second, nil
	}
	if rebalancing {
		state.Message = "all nodes online; waiting for cluster rebalancing to complete"
		log.Info("Cluster is rebalancing; holding drain slot before next worker", "node", state.Hostname)
		return 30 * time.Second, nil
	}

	// Clean up PDB and pod label.
	if err := r.cleanupDrainResources(ctx, snCR.Namespace, state.Hostname); err != nil {
		log.Error(err, "Cleanup failed, will retry", "node", state.Hostname)
		return 10 * time.Second, nil
	}

	log.Info("All storage nodes back online and cluster rebalancing complete; drain coordination complete", "node", state.Hostname, "nodeCount", len(nodeUUIDs))
	state.Phase = simplyblockv1alpha1.DrainPhaseComplete
	state.ActiveNodeUUID = ""
	state.Message = "all storage nodes back online; cluster rebalancing complete"
	return 0, nil
}

// ensurePDB creates or updates the per-node PodDisruptionBudget.
// maxUnavailable=0 blocks eviction; maxUnavailable=1 allows it.
func (r *NodeDrainCoordinatorReconciler) ensurePDB(
	ctx context.Context,
	namespace, nodeName string,
	maxUnavailable int,
) error {
	pdbName := drainPDBPrefix + sanitizeLabelValue(nodeName)
	maxUnavailableVal := intstr.FromInt32(int32(maxUnavailable))

	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbName,
			Namespace: namespace,
			Labels: map[string]string{
				"simplyblock.io/managed-by": "drain-coordinator",
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailableVal,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					drainNodeLabelKey: nodeName,
				},
			},
		},
	}

	existing := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Name: pdbName, Namespace: namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.MaxUnavailable = &maxUnavailableVal
	return r.Patch(ctx, existing, patch)
}

// labelStoragePod patches the SPDK pod (role=simplyblock-storage-node) and
// simplyblock-webappapi pods running on nodeName with drainNodeLabelKey so the
// per-node PDB can select and protect them during drain coordination.
// These are regular pods (not DaemonSet-owned), so PDB eviction blocking works.
func (r *NodeDrainCoordinatorReconciler) labelStoragePod(
	ctx context.Context,
	namespace, nodeName string,
) error {
	// Label selectors for pods that must be protected during drain.
	// FDB pods are included so that coordinators and log processes are not
	// evicted before the simplyblock shutdown is confirmed — losing an FDB
	// process mid-shutdown causes transaction timeouts in the webAPI.
	targetSelectors := []map[string]string{
		{"role": "simplyblock-storage-node"},
		{"app": "simplyblock-webappapi"},
		{"foundationdb.org/fdb-cluster-name": "simplyblock-fdb-cluster"},
	}

	for _, selector := range targetSelectors {
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList,
			client.InNamespace(namespace),
			client.MatchingLabels(selector),
		); err != nil {
			return fmt.Errorf("list pods %v: %w", selector, err)
		}

		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Spec.NodeName != nodeName {
				continue
			}
			if pod.Labels[drainNodeLabelKey] == sanitizeLabelValue(nodeName) {
				continue // already labelled
			}
			patch := client.MergeFrom(pod.DeepCopy())
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
			}
			pod.Labels[drainNodeLabelKey] = sanitizeLabelValue(nodeName)
			if err := r.Patch(ctx, pod, patch); err != nil {
				return fmt.Errorf("patch pod %s: %w", pod.Name, err)
			}
		}
	}
	return nil
}

// cleanupDrainResources deletes the per-node PDB and removes the drain label
// cleanupPDB deletes the per-node PodDisruptionBudget.
func (r *NodeDrainCoordinatorReconciler) cleanupPDB(
	ctx context.Context,
	namespace, nodeName string,
) error {
	pdbName := drainPDBPrefix + sanitizeLabelValue(nodeName)
	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(ctx, types.NamespacedName{Name: pdbName, Namespace: namespace}, pdb); err == nil {
		if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete PDB: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get PDB: %w", err)
	}
	return nil
}

// cleanupDrainResources deletes the per-node PDB and removes the drain label
// from any pods that still carry it.
func (r *NodeDrainCoordinatorReconciler) cleanupDrainResources(
	ctx context.Context,
	namespace, nodeName string,
) error {
	if err := r.cleanupPDB(ctx, namespace, nodeName); err != nil {
		return err
	}

	// Remove drain label from any surviving pods (e.g., if eviction didn't happen).
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{drainNodeLabelKey: sanitizeLabelValue(nodeName)},
	); err != nil {
		return fmt.Errorf("list labelled pods: %w", err)
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		patch := client.MergeFrom(pod.DeepCopy())
		delete(pod.Labels, drainNodeLabelKey)
		if err := r.Patch(ctx, pod, patch); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove drain label from pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// ensureManagerPDB creates a blocking PDB (maxUnavailable=0) for the manager
// pod itself, preventing MCP from evicting the manager while storage PDB
// protection is being set up on the same node.
func (r *NodeDrainCoordinatorReconciler) ensureManagerPDB(ctx context.Context, namespace string) error {
	maxUnavailable := intstr.FromInt32(0)
	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      managerPDBName,
			Namespace: namespace,
			Labels: map[string]string{
				"simplyblock.io/managed-by": "drain-coordinator",
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "simplyblock-operator",
				},
			},
		},
	}
	existing := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Name: managerPDBName, Namespace: namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

// deleteManagerPDB removes the manager self-PDB, allowing MCP to evict and
// reschedule the manager onto a non-draining node.
func (r *NodeDrainCoordinatorReconciler) deleteManagerPDB(ctx context.Context, namespace string) error {
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Name: managerPDBName, Namespace: namespace}, pdb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.Delete(ctx, pdb)
}

// cleanupManagerPDBIfStale deletes the manager self-PDB when the manager's own
// node is no longer in the detected drain phase. This handles crash-recovery
// where the PDB was created but the manager was killed before deleting it.
func (r *NodeDrainCoordinatorReconciler) cleanupManagerPDBIfStale(ctx context.Context, snCR *simplyblockv1alpha1.StorageNode) {
	log := logf.FromContext(ctx)
	state := getDrainState(snCR, r.ManagerNodeName)
	// PDB is only needed transiently during DrainPhaseDetected on the manager's node.
	if state != nil && state.Phase == simplyblockv1alpha1.DrainPhaseDetected {
		return
	}
	if err := r.deleteManagerPDB(ctx, snCR.Namespace); err != nil {
		log.Error(err, "Failed to clean up stale manager self-PDB")
	}
}

// SetupWithManager wires the controller to watch StorageNode CRs, k8s Nodes,
// and the pods that labelStoragePod tracks. Watching pods ensures that when a
// tracked pod is recreated (e.g. after a crash) the reconcile fires immediately
// to re-apply the drain label, keeping PDB protection continuous.
func (r *NodeDrainCoordinatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Named("nodedrain").
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToStorageNodeRequests),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.trackedPodToStorageNodeRequests),
		).
		Complete(r)
}

// trackedPodToStorageNodeRequests maps a Pod event to the StorageNode CR(s)
// whose workerNodes contains the pod's node. Only fires for the pod selectors
// that labelStoragePod tracks, so unrelated pods cause no reconcile churn.
func (r *NodeDrainCoordinatorReconciler) trackedPodToStorageNodeRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	pod := obj.(*corev1.Pod)

	if pod.Spec.NodeName == "" {
		return nil
	}

	labels := pod.GetLabels()
	tracked := labels["role"] == "simplyblock-storage-node" ||
		labels["app"] == "simplyblock-webappapi" ||
		labels["foundationdb.org/fdb-cluster-name"] == "simplyblock-fdb-cluster"
	if !tracked {
		return nil
	}

	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, sn := range snList.Items {
		if slices.Contains(sn.Spec.WorkerNodes, pod.Spec.NodeName) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: sn.Namespace,
					Name:      sn.Name,
				},
			})
		}
	}
	return requests
}

// nodeToStorageNodeRequests maps a k8s Node event to the StorageNode CR(s)
// that list the node in spec.workerNodes.
func (r *NodeDrainCoordinatorReconciler) nodeToStorageNodeRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	node := obj.(*corev1.Node)

	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, sn := range snList.Items {
		if slices.Contains(sn.Spec.WorkerNodes, node.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: sn.Namespace,
					Name:      sn.Name,
				},
			})
		}
	}
	return requests
}

/* -------------------- helpers -------------------- */

// backendNodeInfo holds the relevant fields from the storage-nodes API response.
type backendNodeInfo struct {
	UUID            string `json:"id"`
	Status          string `json:"status"`
	Healthy         bool   `json:"health_check"`
	SecondaryNodeID string `json:"secondary_node_id"`
}

// getBackendNodeInfo fetches status and health_check for a node UUID.
func getBackendNodeInfo(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (backendNodeInfo, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)
	body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || statusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", statusCode, string(body))
		}
		return backendNodeInfo{}, err
	}

	var info backendNodeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return backendNodeInfo{}, fmt.Errorf("unmarshal node info: %w", err)
	}
	return info, nil
}

// getDrainState returns the drain state entry for the given hostname, or nil.
func getDrainState(snCR *simplyblockv1alpha1.StorageNode, hostname string) *simplyblockv1alpha1.NodeDrainState {
	for i := range snCR.Status.DrainCoordination {
		if snCR.Status.DrainCoordination[i].Hostname == hostname {
			return &snCR.Status.DrainCoordination[i]
		}
	}
	return nil
}

// upsertDrainState inserts or updates the drain state entry for state.Hostname.
func upsertDrainState(snCR *simplyblockv1alpha1.StorageNode, state simplyblockv1alpha1.NodeDrainState) {
	for i := range snCR.Status.DrainCoordination {
		if snCR.Status.DrainCoordination[i].Hostname == state.Hostname {
			snCR.Status.DrainCoordination[i] = state
			return
		}
	}
	snCR.Status.DrainCoordination = append(snCR.Status.DrainCoordination, state)
}

// removeDrainState removes the drain state entry for the given hostname.
func removeDrainState(snCR *simplyblockv1alpha1.StorageNode, hostname string) {
	filtered := snCR.Status.DrainCoordination[:0]
	for _, s := range snCR.Status.DrainCoordination {
		if s.Hostname != hostname {
			filtered = append(filtered, s)
		}
	}
	snCR.Status.DrainCoordination = filtered
}

// countActiveDrains returns the number of *workers* currently in the active
// drain window. maxFaultTolerance is a worker-level limit: with tolerance=2,
// two workers may drain simultaneously regardless of how many socket nodes each
// worker carries. It takes the maximum of:
//
//  1. Controller drain state (phases: shutdown_called, draining, restart_called)
//     — one DrainCoordination entry per worker, so this is already worker-level.
//  2. Backend node status (in_shutdown, in_restart) grouped by worker hostname
//     — prevents a controller crash/restart from resetting the count to zero
//     while nodes are still transitioning in the backend.
func countActiveDrains(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) int {
	// Count workers in the active drain window from controller state.
	// DrainCoordination has one entry per worker, so no grouping needed.
	controllerCount := 0
	for _, s := range snCR.Status.DrainCoordination {
		switch s.Phase {
		case simplyblockv1alpha1.DrainPhaseShutdownCalled,
			simplyblockv1alpha1.DrainPhaseDraining,
			simplyblockv1alpha1.DrainPhaseRestartCalled:
			controllerCount++
		}
	}

	// Count workers with at least one backend node in transition, grouped by
	// hostname so a 2-socket worker counts as 1, not 2.
	// On API error, conservatively mark that worker active to prevent a
	// transient failure from opening a drain slot.
	activeWorkers := map[string]bool{}
	for _, n := range snCR.Status.Nodes {
		if n.UUID == "" || n.Hostname == "" {
			continue
		}
		if activeWorkers[n.Hostname] {
			continue // already counted this worker
		}
		info, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, n.UUID)
		if err != nil {
			activeWorkers[n.Hostname] = true
			continue
		}
		switch info.Status {
		case "in_shutdown", "in_restart":
			activeWorkers[n.Hostname] = true
		}
	}
	backendCount := len(activeWorkers)

	if backendCount > controllerCount {
		return backendCount
	}
	return controllerCount
}

// findNodeUUID returns the backend UUID for the given hostname from StorageNode status.
func findNodeUUID(snCR *simplyblockv1alpha1.StorageNode, hostname string) string {
	for _, n := range snCR.Status.Nodes {
		if n.Hostname == hostname {
			return n.UUID
		}
	}
	return ""
}

// findAllNodeUUIDs returns all backend UUIDs registered for the given hostname.
// For multi-socket workers (socketsToUse configured) each NUMA socket produces
// a separate backend node entry sharing the same hostname, so this may return
// more than one UUID. Returns a single-element slice for standard workers.
func findAllNodeUUIDs(snCR *simplyblockv1alpha1.StorageNode, hostname string) []string {
	var uuids []string
	for _, n := range snCR.Status.Nodes {
		if n.Hostname == hostname && n.UUID != "" {
			uuids = append(uuids, n.UUID)
		}
	}
	return uuids
}

// nextUUIDInList returns the element immediately after current in uuids,
// or an empty string if current is the last element or not found.
func nextUUIDInList(uuids []string, current string) string {
	for i, u := range uuids {
		if u == current && i+1 < len(uuids) {
			return uuids[i+1]
		}
	}
	return ""
}

// isNodeReady returns true when the node has a Ready condition with status True.
// isWorkerOnline returns true if the worker node has status "online" in the
// StorageNode status, meaning it has been fully added and is serving I/O.
func isWorkerOnline(snCR *simplyblockv1alpha1.StorageNode, workerName string) bool {
	for _, n := range snCR.Status.Nodes {
		if n.Hostname == workerName {
			return n.Status == "online"
		}
	}
	return false
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// isPeerBusy lists all storage nodes in the cluster and finds the one that
// has nodeUUID as its secondary_node_id. That node is the HA peer (primary)
// of the node we want to restart. Returns true if the peer is in_restart or
// in_shutdown, which means we must defer the restart.
func isPeerBusy(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (bool, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes", clusterUUID)
	body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || statusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", statusCode, string(body))
		}
		return false, fmt.Errorf("list storage nodes: %w", err)
	}

	var nodes []backendNodeInfo
	if err := json.Unmarshal(body, &nodes); err != nil {
		return false, fmt.Errorf("unmarshal storage nodes: %w", err)
	}

	for _, n := range nodes {
		if n.SecondaryNodeID == nodeUUID {
			return n.Status == nodeStatusInRestart || n.Status == nodeStatusInShutdown, nil
		}
	}
	return false, nil
}

// isClusterRebalancing returns true when the simplyblock cluster is actively
// rebalancing data. The drain slot is held until rebalancing completes so that
// the next worker drain does not start while the cluster is already under load.
func isClusterRebalancing(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
) (bool, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || statusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", statusCode, string(body))
		}
		return false, fmt.Errorf("get cluster info: %w", err)
	}

	var info struct {
		Rebalancing bool `json:"is_re_balancing"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("unmarshal cluster info: %w", err)
	}
	return info.Rebalancing, nil
}

// sanitizeLabelValue truncates to 63 chars (Kubernetes label value limit).
func sanitizeLabelValue(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}
