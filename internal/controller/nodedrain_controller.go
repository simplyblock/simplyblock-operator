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

	for _, workerName := range snCR.Spec.WorkerNodes {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: workerName}, node); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get worker node", "node", workerName)
			}
			continue
		}

		state := getDrainState(snCR, workerName)
		cordoned := node.Spec.Unschedulable

		// Normal operation: no cordon, no state.
		if !cordoned && state == nil {
			continue
		}

		// Node was uncordoned (possibly after reboot + MCO uncordon).
		if !cordoned && state != nil {
			switch state.Phase {
			case simplyblockv1alpha1.DrainPhaseComplete, simplyblockv1alpha1.DrainPhaseFailed:
				removeDrainState(snCR, workerName)
				log.Info("Cleared terminal drain state after uncordon", "node", workerName, "phase", state.Phase)
				continue
			case simplyblockv1alpha1.DrainPhaseDraining:
				// Node uncordoned after reboot — call restart directly.
				log.Info("Node uncordoned after drain; calling restart", "node", workerName)
				requeue := r.handleDraining(ctx, snCR, state, apiClient, clusterUUID, clusterSecret)
				upsertDrainState(snCR, *state)
				if requeue > 0 && (nextRequeue == 0 || requeue < nextRequeue) {
					nextRequeue = requeue
				}
			default:
				// Unexpected uncordon mid-sequence (e.g., admin intervention).
				log.Info("Node uncordoned mid-drain; aborting coordination", "node", workerName, "phase", state.Phase)
				if cleanupErr := r.cleanupDrainResources(ctx, snCR.Namespace, workerName); cleanupErr != nil {
					log.Error(cleanupErr, "Failed to clean up drain resources on abort", "node", workerName)
				}
				removeDrainState(snCR, workerName)
			}
			continue
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

		requeue, advErr := r.advanceStateMachine(
			ctx, snCR, state, apiClient, clusterUUID, clusterSecret, maxFaultTolerance,
		)
		if advErr != nil {
			log.Error(advErr, "Drain state machine error", "node", workerName, "phase", state.Phase)
			state.Phase = simplyblockv1alpha1.DrainPhaseFailed
			state.Message = advErr.Error()
		}
		upsertDrainState(snCR, *state)

		if requeue > 0 && (nextRequeue == 0 || requeue < nextRequeue) {
			nextRequeue = requeue
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
func (r *NodeDrainCoordinatorReconciler) handleDetected(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNode,
	state *simplyblockv1alpha1.NodeDrainState,
	apiClient *webapi.Client,
	clusterUUID, clusterSecret string,
	maxFaultTolerance int,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	activeDrains := countActiveDrains(snCR)
	if activeDrains >= maxFaultTolerance {
		state.Message = fmt.Sprintf("waiting for drain slot (%d/%d active)", activeDrains, maxFaultTolerance)
		log.Info("No drain slot available", "node", state.Hostname, "active", activeDrains, "max", maxFaultTolerance)
		return 10 * time.Second, nil
	}

	nodeUUID := findNodeUUID(snCR, state.Hostname)
	if nodeUUID == "" {
		return 15 * time.Second, fmt.Errorf("node %s not yet registered with backend (UUID missing)", state.Hostname)
	}

	// Label the storage pod on this node so the per-node PDB can select it.
	if err := r.labelStoragePod(ctx, snCR.Namespace, snCR.Spec.ClusterName, state.Hostname); err != nil {
		return 10 * time.Second, fmt.Errorf("label storage pod: %w", err)
	}

	// Create a blocking PDB to prevent premature pod eviction during drain.
	if err := r.ensurePDB(ctx, snCR.Namespace, state.Hostname, 0); err != nil {
		return 10 * time.Second, fmt.Errorf("create blocking PDB: %w", err)
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

	backendStatus, err := getBackendNodeStatus(ctx, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Info("Failed to poll backend node status, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}

	if backendStatus != "offline" {
		state.Message = fmt.Sprintf("waiting for offline, current: %s", backendStatus)
		log.Info("Node not offline yet", "node", state.Hostname, "status", backendStatus)
		return 10 * time.Second, nil
	}

	// Shutdown confirmed — relax the PDB to allow drain to proceed.
	if err := r.ensurePDB(ctx, snCR.Namespace, state.Hostname, 1); err != nil {
		return 10 * time.Second, fmt.Errorf("relax PDB: %w", err)
	}

	log.Info("Node offline; PDB relaxed — drain can proceed", "node", state.Hostname)
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

	restartPayload := map[string]any{
		"force":        true,
		"node_address": ip,
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

	backendStatus, err := getBackendNodeStatus(ctx, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Info("Failed to poll backend node status after restart, retrying", "node", state.Hostname, "err", err)
		return 10 * time.Second, nil
	}

	if backendStatus != "online" {
		state.Message = fmt.Sprintf("waiting for online, current: %s", backendStatus)
		log.Info("Node not online yet", "node", state.Hostname, "status", backendStatus)
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

// labelStoragePod patches the storage-node DaemonSet pod running on nodeName
// to add drainNodeLabelKey so the per-node PDB can select it.
func (r *NodeDrainCoordinatorReconciler) labelStoragePod(
	ctx context.Context,
	namespace, clusterName, nodeName string,
) error {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"app":                 "storage-node",
			"simplyblock-cluster": clusterName,
		},
	); err != nil {
		return fmt.Errorf("list storage pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.Labels[drainNodeLabelKey] == nodeName {
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
	return nil
}

// cleanupDrainResources deletes the per-node PDB and removes the drain label
// from any pods that still carry it.
func (r *NodeDrainCoordinatorReconciler) cleanupDrainResources(
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

// getBackendNodeStatus fetches the backend status string for a node UUID.
func getBackendNodeStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)
	body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || statusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", statusCode, string(body))
		}
		return "", err
	}

	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("unmarshal node status: %w", err)
	}
	return resp.Status, nil
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
// window (shutdown_called, draining, or restart_called).
func countActiveDrains(snCR *simplyblockv1alpha1.StorageNode) int {
	count := 0
	for _, s := range snCR.Status.DrainCoordination {
		switch s.Phase {
		case simplyblockv1alpha1.DrainPhaseShutdownCalled,
			simplyblockv1alpha1.DrainPhaseDraining,
			simplyblockv1alpha1.DrainPhaseRestartCalled:
			count++
		}
	}
	return count
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

// sanitizeLabelValue truncates to 63 chars (Kubernetes label value limit).
func sanitizeLabelValue(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}
