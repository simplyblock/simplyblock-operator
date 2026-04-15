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

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

const (
	// drainNodeLabelKey is patched onto the storage pod on a draining node so the
	// per-node PodDisruptionBudget can select it precisely.
	drainNodeLabelKey = "simplyblock.io/drain-node"

	// drainPDBPrefix is the prefix used for per-node PodDisruptionBudget names.
	drainPDBPrefix = "simplyblock-drain-"

	// managerPDBName is the name of the temporary PDB that protects the manager
	// pod from eviction while it sets up storage PDB protection on its own node.
	managerPDBName = "simplyblock-manager-self"
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
//  3. Confirm  – poll until backend node status == "offline"
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
	ManagerNodeName string
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
	if clusterCR.Spec.MaxFaultTolerance != nil && *clusterCR.Spec.MaxFaultTolerance > 0 {
		maxFaultTolerance = int(*clusterCR.Spec.MaxFaultTolerance)
	}

	apiClient := webapi.NewClient()
	patch := client.MergeFrom(snCR.DeepCopy())
	nextRequeue := time.Duration(0)

	// Pre-create blocking PDBs for all worker nodes before processing any
	// cordon events. This eliminates the race window where MCP cordons and
	// evicts the SPDK pod before the controller has a chance to label it and
	// create the per-node PDB. The PDB is deleted only after drain completes.
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

	nodeUUID := findNodeUUID(snCR, state.Hostname)
	if nodeUUID == "" {
		return 15 * time.Second, fmt.Errorf("node %s not yet registered with backend (UUID missing)", state.Hostname)
	}

	// Check backend status before calling shutdown — the node may already be
	// in_shutdown or offline from a previous (possibly crashed) reconcile run.
	// In that case skip the API call and advance the phase directly.
	nodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Info("Failed to query backend node status before shutdown, retrying", "node", state.Hostname, "err", err)
		return 15 * time.Second, nil
	}

	switch nodeInfo.Status {
	case "offline":
		log.Info("Backend already offline; skipping shutdown API call", "node", state.Hostname)
		if err := r.cleanupPDB(ctx, snCR.Namespace, state.Hostname); err != nil {
			return 10 * time.Second, fmt.Errorf("delete PDB: %w", err)
		}
		state.Phase = simplyblockv1alpha1.DrainPhaseDraining
		state.Message = "backend already offline; drain allowed"
		return 15 * time.Second, nil
	case "in_shutdown":
		log.Info("Backend already in_shutdown; advancing to shutdown_called phase", "node", state.Hostname)
		state.Phase = simplyblockv1alpha1.DrainPhaseShutdownCalled
		state.Message = "backend already in_shutdown; waiting for offline confirmation"
		return 10 * time.Second, nil
	}

	// Call simplyblock shutdown API.
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/shutdown?force=true", clusterUUID, nodeUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return 15 * time.Second, fmt.Errorf("shutdown API: %w", err)
	}

	log.Info("Shutdown API called", "node", state.Hostname, "nodeUUID", nodeUUID)
	state.Phase = simplyblockv1alpha1.DrainPhaseShutdownCalled
	state.Message = "shutdown API called; waiting for offline confirmation"
	return 10 * time.Second, nil
}

// handleShutdownCalled polls the backend until the node status is "offline",
// then relaxes the PDB to allow pod eviction.
func (r *NodeDrainCoordinatorReconciler) handleShutdownCalled(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	nodeUUID := findNodeUUID(snCR, state.Hostname)
	if nodeUUID == "" {
		return 10 * time.Second, nil
	}

	nodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Info("Failed to poll backend node status, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}

	if nodeInfo.Status != "offline" {
		state.Message = fmt.Sprintf("waiting for offline, current: %s", nodeInfo.Status)
		log.Info("Node not offline yet", "node", state.Hostname, "status", nodeInfo.Status)
		return 10 * time.Second, nil
	}

	// Shutdown confirmed — delete the PDB entirely to allow MCP to evict all pods.
	if err := r.cleanupPDB(ctx, snCR.Namespace, state.Hostname); err != nil {
		return 10 * time.Second, fmt.Errorf("delete PDB: %w", err)
	}

	log.Info("Node offline; PDB removed — drain can proceed", "node", state.Hostname)
	state.Phase = simplyblockv1alpha1.DrainPhaseDraining
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

	ip, err := getNodeInternalIP(ctx, r.Client, state.Hostname)
	if err != nil {
		return 15 * time.Second
	}

	// Verify SPDK is reachable before calling restart.
	if err := checkNodeInfoReachable(ctx, ip); err != nil {
		state.Message = "waiting for SPDK to become reachable after reboot"
		log.Info("SPDK not yet reachable, will retry", "node", state.Hostname)
		return 15 * time.Second
	}

	nodeUUID := findNodeUUID(snCR, state.Hostname)
	if nodeUUID == "" {
		return 15 * time.Second
	}

	nodeAddr := fmt.Sprintf("%s:5000", ip)
	restartPayload := map[string]any{
		"force":        true,
		"node_address": nodeAddr,
	}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, nodeUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, restartPayload)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "Restart API failed, will retry", "node", state.Hostname)
		return 15 * time.Second
	}

	log.Info("Restart API called", "node", state.Hostname, "nodeUUID", nodeUUID)
	state.Phase = simplyblockv1alpha1.DrainPhaseRestartCalled
	state.Message = "restart API called; waiting for online confirmation"
	return 10 * time.Second
}

// handleRestartCalled polls the backend until the node is "online", then cleans up.
func (r *NodeDrainCoordinatorReconciler) handleRestartCalled(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	nodeUUID := findNodeUUID(snCR, state.Hostname)
	if nodeUUID == "" {
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

	// Then confirm the simplyblock backend reports the node as online and healthy.
	nodeInfo, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Info("Failed to poll backend node status after restart, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}

	if nodeInfo.Status != "online" {
		state.Message = fmt.Sprintf("Kubernetes node Ready; waiting for backend online, current: %s", nodeInfo.Status)
		log.Info("Node not online yet", "node", state.Hostname, "status", nodeInfo.Status)
		return 10 * time.Second, nil
	}

	if !nodeInfo.Healthy {
		state.Message = "node online; waiting for storage node health check to pass"
		log.Info("Storage node health check not passing yet", "node", state.Hostname)
		return 10 * time.Second, nil
	}

	// Node is back — clean up PDB and pod label.
	if err := r.cleanupDrainResources(ctx, snCR.Namespace, state.Hostname); err != nil {
		log.Error(err, "Cleanup failed, will retry", "node", state.Hostname)
		return 10 * time.Second, nil
	}

	log.Info("Node back online; drain coordination complete", "node", state.Hostname)
	state.Phase = simplyblockv1alpha1.DrainPhaseComplete
	state.Message = "node back online"
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
					"app": "simplyblock-manager",
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

// SetupWithManager wires the controller to watch StorageNode CRs and k8s Nodes.
// A change to any k8s Node is mapped to the StorageNode CR that owns it.
func (r *NodeDrainCoordinatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Named("nodedrain").
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToStorageNodeRequests),
		).
		Complete(r)
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
		for _, workerName := range sn.Spec.WorkerNodes {
			if workerName == node.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: sn.Namespace,
						Name:      sn.Name,
					},
				})
				break
			}
		}
	}
	return requests
}

/* -------------------- helpers -------------------- */

// backendNodeInfo holds the relevant fields from the storage-nodes API response.
type backendNodeInfo struct {
	Status  string `json:"status"`
	Healthy bool   `json:"health_check"`
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

// countActiveDrains returns the number of nodes currently in the active drain
// window. It takes the maximum of:
//  1. Controller drain state (phases: shutdown_called, draining, restart_called)
//  2. Backend node status (in_shutdown, in_restart) for all registered nodes
//
// Using the backend as the authoritative source prevents a controller crash/restart
// from resetting the active count to 0 while nodes are still transitioning in the backend.
func countActiveDrains(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
) int {
	// Count from controller drain state.
	controllerCount := 0
	for _, s := range snCR.Status.DrainCoordination {
		switch s.Phase {
		case simplyblockv1alpha1.DrainPhaseShutdownCalled,
			simplyblockv1alpha1.DrainPhaseDraining,
			simplyblockv1alpha1.DrainPhaseRestartCalled:
			controllerCount++
		}
	}

	// Count from backend status — covers nodes active in the backend but not
	// yet reflected in controller state (e.g., after a controller restart).
	// On API error, conservatively count the node as active to prevent a
	// transient failure from opening the drain slot.
	backendCount := 0
	for _, n := range snCR.Status.Nodes {
		if n.UUID == "" {
			continue
		}
		info, err := getBackendNodeInfo(ctx, apiClient, clusterSecret, clusterUUID, n.UUID)
		if err != nil {
			// Cannot determine state — treat as active to be safe.
			backendCount++
			continue
		}
		switch info.Status {
		case "in_shutdown", "in_restart":
			backendCount++
		}
	}

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

// isNodeReady returns true when the node has a Ready condition with status True.
func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// sanitizeLabelValue truncates to 63 chars (Kubernetes label value limit).
func sanitizeLabelValue(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}
