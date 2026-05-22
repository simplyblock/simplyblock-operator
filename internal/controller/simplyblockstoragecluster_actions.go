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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// failAction marks the current ActionStatus as failed with the given error.
func (r *StorageClusterReconciler) failAction(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	err error,
) (ctrl.Result, error) {
	clusterCR.Status.ActionStatus.State = utils.ActionStateFailed
	clusterCR.Status.ActionStatus.Message = err.Error()
	_ = r.Status().Update(ctx, clusterCR)
	return ctrl.Result{}, nil
}

// ── shutdown ──────────────────────────────────────────────────────────────────

func (r *StorageClusterReconciler) reconcileShutdown(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if clusterCR.Status.ActionStatus != nil &&
		clusterCR.Status.ActionStatus.Action == utils.ClusterActionShutdown &&
		clusterCR.Status.ActionStatus.State == utils.ActionStateSuccess &&
		clusterCR.Status.ActionStatus.ObservedGeneration == clusterCR.Generation {
		return ctrl.Result{}, nil
	}

	if clusterCR.Status.ActionStatus == nil ||
		clusterCR.Status.ActionStatus.Action != utils.ClusterActionShutdown {
		clusterCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:             utils.ClusterActionShutdown,
			State:              utils.ActionStateRunning,
			ObservedGeneration: clusterCR.Generation,
		}
		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, clusterCR)
	}

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		return r.failAction(ctx, clusterCR, err)
	}

	apiClient := webapi.NewClient()

	if !clusterCR.Status.ActionStatus.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/shutdown", clusterUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d body=%s", status, string(body))
			}
			log.Error(err, "Cluster shutdown API call failed", "cluster", clusterCR.Name)
			return r.failAction(ctx, clusterCR, fmt.Errorf("shutdown API failed: %w", err))
		}
		log.Info("Cluster shutdown triggered", "cluster", clusterCR.Name)
		clusterCR.Status.ActionStatus.Triggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Cluster GET failed during shutdown poll", "cluster", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "Failed to parse cluster response during shutdown poll")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if resp.Status == utils.ClusterStatusSuspended {
		clusterCR.Status.Status = resp.Status
		clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
		clusterCR.Status.ActionStatus.Message = "Cluster shut down successfully"
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster shut down successfully", "cluster", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// ── start ─────────────────────────────────────────────────────────────────────

func (r *StorageClusterReconciler) reconcileStart(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if clusterCR.Status.ActionStatus != nil &&
		clusterCR.Status.ActionStatus.Action == utils.ClusterActionStart &&
		clusterCR.Status.ActionStatus.State == utils.ActionStateSuccess &&
		clusterCR.Status.ActionStatus.ObservedGeneration == clusterCR.Generation {
		return ctrl.Result{}, nil
	}

	if clusterCR.Status.ActionStatus == nil ||
		clusterCR.Status.ActionStatus.Action != utils.ClusterActionStart {
		clusterCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:             utils.ClusterActionStart,
			State:              utils.ActionStateRunning,
			ObservedGeneration: clusterCR.Generation,
		}
		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, clusterCR)
	}

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		return r.failAction(ctx, clusterCR, err)
	}

	apiClient := webapi.NewClient()

	if !clusterCR.Status.ActionStatus.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/start", clusterUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d body=%s", status, string(body))
			}
			log.Error(err, "Cluster start API call failed", "cluster", clusterCR.Name)
			return r.failAction(ctx, clusterCR, fmt.Errorf("start API failed: %w", err))
		}
		log.Info("Cluster start triggered", "cluster", clusterCR.Name)
		clusterCR.Status.ActionStatus.Triggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Cluster GET failed during start poll", "cluster", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "Failed to parse cluster response during start poll")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if resp.Status == utils.ClusterStatusActive {
		clusterCR.Status.Status = resp.Status
		clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
		clusterCR.Status.ActionStatus.Message = "Cluster started successfully"
		clusterCR.Status.Rebalancing = &resp.Rebalancing
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster started successfully", "cluster", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// ── restart ───────────────────────────────────────────────────────────────────

// reconcileRestart sequences a shutdown then a start.
// ActionStatus.Message holds the current sub-phase: "shutdown" or "start".
func (r *StorageClusterReconciler) reconcileRestart(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if clusterCR.Status.ActionStatus != nil &&
		clusterCR.Status.ActionStatus.Action == utils.ClusterActionRestart &&
		clusterCR.Status.ActionStatus.State == utils.ActionStateSuccess &&
		clusterCR.Status.ActionStatus.ObservedGeneration == clusterCR.Generation {
		return ctrl.Result{}, nil
	}

	if clusterCR.Status.ActionStatus == nil ||
		clusterCR.Status.ActionStatus.Action != utils.ClusterActionRestart {
		clusterCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:             utils.ClusterActionRestart,
			State:              utils.ActionStateRunning,
			Message:            "shutdown", // restart sub-phase
			ObservedGeneration: clusterCR.Generation,
		}
		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, clusterCR)
	}

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		return r.failAction(ctx, clusterCR, err)
	}

	apiClient := webapi.NewClient()
	phase := clusterCR.Status.ActionStatus.Message // "shutdown" or "start"

	if !clusterCR.Status.ActionStatus.Triggered {
		var apiEndpoint string
		if phase == "shutdown" {
			apiEndpoint = fmt.Sprintf("/api/v2/clusters/%s/shutdown", clusterUUID)
		} else {
			apiEndpoint = fmt.Sprintf("/api/v2/clusters/%s/start", clusterUUID)
		}
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, apiEndpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d body=%s", status, string(body))
			}
			log.Error(err, "Cluster restart phase API call failed", "cluster", clusterCR.Name, "phase", phase)
			return r.failAction(ctx, clusterCR, fmt.Errorf("restart %s phase failed: %w", phase, err))
		}
		log.Info("Cluster restart phase triggered", "cluster", clusterCR.Name, "phase", phase)
		clusterCR.Status.ActionStatus.Triggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Cluster GET failed during restart poll", "cluster", clusterCR.Name, "phase", phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "Failed to parse cluster response during restart poll")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	clusterCR.Status.Status = resp.Status

	if phase == "shutdown" && resp.Status == utils.ClusterStatusSuspended {
		clusterCR.Status.ActionStatus.Message = "start"
		clusterCR.Status.ActionStatus.Triggered = false
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster shutdown complete during restart, triggering start", "cluster", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if phase == "start" && resp.Status == utils.ClusterStatusActive {
		clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
		clusterCR.Status.ActionStatus.Message = "Cluster restarted successfully"
		clusterCR.Status.Rebalancing = &resp.Rebalancing
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster restarted successfully", "cluster", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// ── node-recycle ──────────────────────────────────────────────────────────────

func (r *StorageClusterReconciler) reconcileNodeRecycle(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if clusterCR.Status.ActionStatus != nil &&
		clusterCR.Status.ActionStatus.Action == utils.ClusterActionNodeRecycle &&
		clusterCR.Status.ActionStatus.State == utils.ActionStateSuccess &&
		clusterCR.Status.ActionStatus.ObservedGeneration == clusterCR.Generation {
		return ctrl.Result{}, nil
	}

	// Reinitialize on first entry or when spec generation changes so that stale
	// NodeRecycleStatus from a previous run is discarded.
	if clusterCR.Status.ActionStatus == nil ||
		clusterCR.Status.ActionStatus.Action != utils.ClusterActionNodeRecycle ||
		clusterCR.Status.ActionStatus.ObservedGeneration != clusterCR.Generation {
		clusterCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:             utils.ClusterActionNodeRecycle,
			State:              utils.ActionStateRunning,
			ObservedGeneration: clusterCR.Generation,
		}
		clusterCR.Status.NodeRecycleStatus = nil
		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, clusterCR)
	}

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		return r.failAction(ctx, clusterCR, err)
	}

	apiClient := webapi.NewClient()

	// Populate PendingNodes once from the API.
	if clusterCR.Status.NodeRecycleStatus == nil {
		nodes, err := listClusterStorageNodes(ctx, apiClient, clusterSecret, clusterUUID)
		if err != nil {
			log.Error(err, "Failed to list storage nodes for node-recycle init")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if len(nodes) == 0 {
			clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
			clusterCR.Status.ActionStatus.Message = "No nodes to recycle"
			if err := r.Status().Update(ctx, clusterCR); err != nil {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, nil
		}

		uuids := make([]string, 0, len(nodes))
		for _, n := range nodes {
			uuids = append(uuids, n.UUID)
		}
		clusterCR.Status.NodeRecycleStatus = &simplyblockv1alpha1.NodeRecycleStatus{
			PendingNodes:   uuids,
			ProcessedNodes: []string{},
			NodePhase:      nodeRecycleFirstPhase(clusterCR),
		}
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	nrs := clusterCR.Status.NodeRecycleStatus

	if len(nrs.PendingNodes) == 0 {
		clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
		clusterCR.Status.ActionStatus.Message = "All nodes recycled successfully"
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Node recycle completed", "cluster", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	currentNodeUUID := nrs.PendingNodes[0]

	switch nrs.NodePhase {
	case utils.NodeRecyclePhaseSnodeRefresh:
		return r.nodeRecycleSnodeRefresh(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseSnodeRefreshWait:
		return r.nodeRecycleSnodeRefreshWait(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseShuttingDown:
		return r.nodeRecycleShuttingDown(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseRestarting:
		return r.nodeRecycleRestarting(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseRebalancing:
		return r.nodeRecycleRebalancing(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, currentNodeUUID)
	default:
		return r.failAction(ctx, clusterCR, fmt.Errorf("unknown node-recycle phase: %q", nrs.NodePhase))
	}
}

func nodeRecycleFirstPhase(_ *simplyblockv1alpha1.StorageCluster) string {
	return utils.NodeRecyclePhaseShuttingDown
}

// nodeRecycleSnodeRefresh deletes the storage-node DaemonSet pod so the
// DaemonSet restarts it with the image already cached on the node.
func (r *StorageClusterReconciler) nodeRecycleSnodeRefresh(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := clusterCR.Status.NodeRecycleStatus

	found, err := r.deleteStorageNodePod(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "Failed to delete storage node pod for refresh", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !found {
		log.Info("Node not found in storage node list, skipping snode-refresh — proceeding to restart", "nodeUUID", nodeUUID)
		nrs.NodePhase = utils.NodeRecyclePhaseRestarting
		nrs.PhaseTriggered = false
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Storage node pod deleted for refresh, waiting for restart", "nodeUUID", nodeUUID)
	nrs.NodePhase = utils.NodeRecyclePhaseSnodeRefreshWait
	nrs.PhaseTriggered = false
	if err := r.Status().Update(ctx, clusterCR); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *StorageClusterReconciler) nodeRecycleSnodeRefreshWait(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ready, err := r.isStorageNodePodReady(ctx, clusterCR, apiClient, clusterSecret, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "Failed to check storage node pod readiness", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if !ready {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Storage node pod refreshed, proceeding to restart", "nodeUUID", nodeUUID)
	nrs := clusterCR.Status.NodeRecycleStatus
	nrs.NodePhase = utils.NodeRecyclePhaseRestarting
	nrs.PhaseTriggered = false
	if err := r.Status().Update(ctx, clusterCR); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *StorageClusterReconciler) nodeRecycleShuttingDown(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := clusterCR.Status.NodeRecycleStatus

	if !nrs.PhaseTriggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/shutdown", clusterUUID, nodeUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d body=%s", status, string(body))
			}
			log.Error(err, "Node shutdown API call failed", "nodeUUID", nodeUUID)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Info("Node shutdown triggered", "nodeUUID", nodeUUID)
		nrs.PhaseTriggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	nodes, err := listClusterStorageNodes(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		log.Error(err, "Failed to list storage nodes during shutdown poll", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	var nodeStatus string
	for _, n := range nodes {
		if n.UUID == nodeUUID {
			nodeStatus = strings.ToLower(n.Status)
			break
		}
	}
	if nodeStatus == "" {
		log.Error(fmt.Errorf("node not found"), "Node missing from storage node list during shutdown poll", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Polling node status after shutdown trigger", "nodeUUID", nodeUUID, "status", nodeStatus)

	refreshSNode := clusterCR.Spec.NodeRecycle != nil && clusterCR.Spec.NodeRecycle.RefreshSNodeAPI

	switch nodeStatus {
	case utils.NodeStatusOffline, utils.NodeStatusInRestart:
		// Node confirmed offline or transitioning — refresh snode pod first if requested.
		if refreshSNode {
			log.Info("Node shutdown confirmed, refreshing snode pod before restart", "nodeUUID", nodeUUID, "status", nodeStatus)
			nrs.NodePhase = utils.NodeRecyclePhaseSnodeRefresh
		} else {
			log.Info("Node shutdown confirmed, advancing to restart", "nodeUUID", nodeUUID, "status", nodeStatus)
			nrs.NodePhase = utils.NodeRecyclePhaseRestarting
		}
		nrs.PhaseTriggered = false
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	default:
		// Node still online — shutdown not yet effective, keep polling.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *StorageClusterReconciler) nodeRecycleRestarting(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := clusterCR.Status.NodeRecycleStatus

	if !nrs.PhaseTriggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, nodeUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, map[string]bool{"force": true})
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d body=%s", status, string(body))
			}
			log.Error(err, "Node restart API call failed", "nodeUUID", nodeUUID)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Info("Node restart triggered", "nodeUUID", nodeUUID)
		nrs.PhaseTriggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	nodes, err := listClusterStorageNodes(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		log.Error(err, "Failed to list storage nodes during restart poll", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	var nodeStatus string
	for _, n := range nodes {
		if n.UUID == nodeUUID {
			nodeStatus = strings.ToLower(n.Status)
			break
		}
	}
	log.Info("Polling node status after restart trigger", "nodeUUID", nodeUUID, "status", nodeStatus)
	if nodeStatus != utils.NodeStatusOnline {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Node online, waiting for rebalancing to complete", "nodeUUID", nodeUUID)
	nrs.NodePhase = utils.NodeRecyclePhaseRebalancing
	nrs.PhaseTriggered = false
	if err := r.Status().Update(ctx, clusterCR); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *StorageClusterReconciler) nodeRecycleRebalancing(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := clusterCR.Status.NodeRecycleStatus

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Failed to get cluster status during rebalancing wait", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "Failed to parse cluster response during rebalancing wait")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if resp.Rebalancing {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	log.Info("Rebalancing complete, node recycled — advancing to next node", "nodeUUID", nodeUUID)
	nrs.ProcessedNodes = append(nrs.ProcessedNodes, nodeUUID)
	nrs.PendingNodes = nrs.PendingNodes[1:]

	if len(nrs.PendingNodes) > 0 {
		nrs.NodePhase = nodeRecycleFirstPhase(clusterCR)
		nrs.PhaseTriggered = false
	}

	if err := r.Status().Update(ctx, clusterCR); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// listClusterStorageNodes fetches all storage nodes for a cluster.
func listClusterStorageNodes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
) ([]utils.NodeStatusResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		return nil, fmt.Errorf("list storage nodes: %w", err)
	}
	var nodes []utils.NodeStatusResponse
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("unmarshal storage nodes: %w", err)
	}
	return nodes, nil
}

// deleteStorageNodePod finds and deletes the storage-node DaemonSet pod running
// on the Kubernetes node that hosts the given backend storage node.
// Returns (true, nil) on success, (false, nil) when the node is not in the
// storage node list (caller should skip the refresh phase), or (false, err)
// on a real failure.
func (r *StorageClusterReconciler) deleteStorageNodePod(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (bool, error) {
	nodes, err := listClusterStorageNodes(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		return false, err
	}
	var nodeIP string
	for _, n := range nodes {
		if n.UUID == nodeUUID {
			nodeIP = n.IP
			break
		}
	}
	if nodeIP == "" {
		return false, nil // node not in list — caller skips snode-refresh
	}

	k8sNodeName, err := r.findK8sNodeByIP(ctx, nodeIP)
	if err != nil {
		return false, fmt.Errorf("find k8s node for IP %s: %w", nodeIP, err)
	}

	pod, err := r.findStorageNodePod(ctx, clusterCR.Namespace, clusterCR.Name, k8sNodeName)
	if err != nil {
		return false, fmt.Errorf("find storage node pod on %s: %w", k8sNodeName, err)
	}
	if pod == nil {
		return true, nil // already gone — DaemonSet will recreate it
	}
	return true, client.IgnoreNotFound(r.Delete(ctx, pod))
}

// isStorageNodePodReady returns true when the storage-node pod for the given
// backend node exists, is not being deleted, and has its Ready condition true.
func (r *StorageClusterReconciler) isStorageNodePodReady(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, nodeUUID string,
) (bool, error) {
	nodes, err := listClusterStorageNodes(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		return false, err
	}
	var nodeIP string
	for _, n := range nodes {
		if n.UUID == nodeUUID {
			nodeIP = n.IP
			break
		}
	}
	if nodeIP == "" {
		return false, fmt.Errorf("node %s not found in storage node list", nodeUUID)
	}

	k8sNodeName, err := r.findK8sNodeByIP(ctx, nodeIP)
	if err != nil {
		return false, err
	}

	pod, err := r.findStorageNodePod(ctx, clusterCR.Namespace, clusterCR.Name, k8sNodeName)
	if err != nil {
		return false, err
	}
	if pod == nil || pod.DeletionTimestamp != nil {
		return false, nil
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

func (r *StorageClusterReconciler) findK8sNodeByIP(ctx context.Context, ip string) (string, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return "", fmt.Errorf("list k8s nodes: %w", err)
	}
	for _, node := range nodeList.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address == ip {
				return node.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no k8s node with InternalIP %s", ip)
}

func (r *StorageClusterReconciler) findStorageNodePod(
	ctx context.Context,
	namespace, clusterName, k8sNodeName string,
) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"app":                 "storage-node",
			"simplyblock-cluster": clusterName,
		},
	); err != nil {
		return nil, fmt.Errorf("list storage node pods: %w", err)
	}
	for i := range podList.Items {
		if podList.Items[i].Spec.NodeName == k8sNodeName {
			return &podList.Items[i], nil
		}
	}
	return nil, nil
}
