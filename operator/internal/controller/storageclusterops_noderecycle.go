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

// reconcileNodeRecycle drives the full node-recycle state machine for a
// StorageClusterOps CR. State is tracked via cluster.Status.NodeRecycleStatus
// so that it survives operator restarts. ops.Status.Triggered is set on first
// entry to prevent re-initialising a run that is already in progress.
func (r *StorageClusterOpsReconciler) reconcileNodeRecycle(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// First entry: clear any stale NodeRecycleStatus left from a previous run.
	if !ops.Status.Triggered {
		cluster.Status.NodeRecycleStatus = nil
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		opsPatch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		if err := r.Status().Patch(ctx, ops, opsPatch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Node recycle initialised", "cluster", cluster.Name)
		return ctrl.Result{Requeue: true}, nil
	}

	apiClient := webapi.NewClient()
	clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
	if err != nil {
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	// Discover all nodes on first reconcile after initialisation.
	if cluster.Status.NodeRecycleStatus == nil {
		nodes, err := listClusterStorageNodeSets(ctx, apiClient, clusterUUID)
		if err != nil {
			log.Error(err, "Failed to list storage nodes for node-recycle init")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if len(nodes) == 0 {
			return r.succeedOps(ctx, ops, cluster, "No nodes to recycle")
		}
		uuids := make([]string, 0, len(nodes))
		for _, n := range nodes {
			uuids = append(uuids, n.UUID)
		}
		cluster.Status.NodeRecycleStatus = &simplyblockv1alpha1.NodeRecycleStatus{
			PendingNodes:   uuids,
			ProcessedNodes: []string{},
			NodePhase:      nodeRecycleFirstPhase(cluster),
		}
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	nrs := cluster.Status.NodeRecycleStatus
	if len(nrs.PendingNodes) == 0 {
		return r.succeedOps(ctx, ops, cluster, "All nodes recycled successfully")
	}

	currentNodeUUID := nrs.PendingNodes[0]
	switch nrs.NodePhase {
	case utils.NodeRecyclePhaseSnodeRefresh:
		return r.scopsNodeRecycleSnodeRefresh(ctx, cluster, apiClient, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseSnodeRefreshWait:
		return r.scopsNodeRecycleSnodeRefreshWait(ctx, cluster, apiClient, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseShuttingDown:
		return r.scopsNodeRecycleShuttingDown(ctx, ops, cluster, apiClient, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseRestarting:
		return r.scopsNodeRecycleRestarting(ctx, cluster, apiClient, clusterUUID, currentNodeUUID)
	case utils.NodeRecyclePhaseRebalancing:
		return r.scopsNodeRecycleRebalancing(ctx, cluster, apiClient, clusterUUID, currentNodeUUID)
	default:
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("unknown node-recycle phase: %q", nrs.NodePhase))
	}
}

func (r *StorageClusterOpsReconciler) scopsNodeRecycleSnodeRefresh(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := cluster.Status.NodeRecycleStatus

	// Persist the phase advance BEFORE the irreversible pod delete so that an
	// operator crash between delete and update re-enters snode-refresh-wait.
	nrs.NodePhase = utils.NodeRecyclePhaseSnodeRefreshWait
	nrs.PhaseTriggered = false
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	found, err := r.scopsDeleteStorageNodeSetPod(ctx, cluster, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "Failed to delete storage node pod for refresh", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if !found {
		log.Info("Node not in storage node list, skipping snode-refresh — proceeding to restart", "nodeUUID", nodeUUID)
		nrs.NodePhase = utils.NodeRecyclePhaseRestarting
		nrs.PhaseTriggered = false
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Storage node pod deleted for refresh, waiting for restart", "nodeUUID", nodeUUID)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *StorageClusterOpsReconciler) scopsNodeRecycleSnodeRefreshWait(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ready, err := r.scopsIsStorageNodeSetPodReady(ctx, cluster, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "Failed to check storage node pod readiness", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if !ready {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Storage node pod refreshed, proceeding to restart", "nodeUUID", nodeUUID)
	nrs := cluster.Status.NodeRecycleStatus
	nrs.NodePhase = utils.NodeRecyclePhaseRestarting
	nrs.PhaseTriggered = false
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

// scopsNodeRecycleTriggerPhase handles the write-ahead trigger half of a phase:
// checks whether the node is already in the target state (idempotency), persists
// PhaseTriggered=true before the API call, then fires the API call.
func (r *StorageClusterOpsReconciler) scopsNodeRecycleTriggerPhase(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
	alreadyDoneStatuses []string,
	method, endpoint string,
	requestBody interface{},
	actionName string,
) *ctrl.Result {
	log := logf.FromContext(ctx)
	nrs := cluster.Status.NodeRecycleStatus

	nodes, err := listClusterStorageNodeSets(ctx, apiClient, clusterUUID)
	if err != nil {
		log.Error(err, fmt.Sprintf("Failed to fetch node status before %s API call", actionName), "nodeUUID", nodeUUID)
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res
	}

	alreadyDone := false
	for _, n := range nodes {
		if n.UUID == nodeUUID {
			s := strings.ToLower(n.Status)
			for _, done := range alreadyDoneStatuses {
				if s == done {
					log.Info(fmt.Sprintf("Node already in %s state, skipping %s API call", s, actionName), "nodeUUID", nodeUUID)
					alreadyDone = true
					break
				}
			}
			break
		}
	}

	nrs.PhaseTriggered = true
	if err := r.Status().Update(ctx, cluster); err != nil {
		res := ctrl.Result{Requeue: true}
		return &res
	}

	if !alreadyDone {
		body, status, err := apiClient.Do(ctx, method, endpoint, requestBody)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d body=%s", status, string(body))
			}
			log.Error(err, fmt.Sprintf("Node %s API call failed", actionName), "nodeUUID", nodeUUID)
			res := ctrl.Result{RequeueAfter: 10 * time.Second}
			return &res
		}
		log.Info(fmt.Sprintf("Node %s triggered", actionName), "nodeUUID", nodeUUID)
	}
	res := ctrl.Result{RequeueAfter: 10 * time.Second}
	return &res
}

func (r *StorageClusterOpsReconciler) scopsNodeRecycleShuttingDown(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := cluster.Status.NodeRecycleStatus

	if !nrs.PhaseTriggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/shutdown", clusterUUID, nodeUUID)
		if res := r.scopsNodeRecycleTriggerPhase(ctx, cluster, apiClient, clusterUUID, nodeUUID,
			[]string{utils.NodeStatusInShutdown, utils.NodeStatusOffline, utils.NodeStatusInRestart},
			http.MethodPost, endpoint, nil, "shutdown",
		); res != nil {
			return *res, nil
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	nodes, err := listClusterStorageNodeSets(ctx, apiClient, clusterUUID)
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

	refreshSNode := ops.Spec.NodeRecycle != nil && ops.Spec.NodeRecycle.RefreshSNodeAPI

	switch nodeStatus {
	case utils.NodeStatusOffline, utils.NodeStatusInRestart:
		if refreshSNode {
			log.Info("Node shutdown confirmed, refreshing snode pod before restart", "nodeUUID", nodeUUID)
			nrs.NodePhase = utils.NodeRecyclePhaseSnodeRefresh
		} else {
			log.Info("Node shutdown confirmed, advancing to restart", "nodeUUID", nodeUUID)
			nrs.NodePhase = utils.NodeRecyclePhaseRestarting
		}
		nrs.PhaseTriggered = false
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *StorageClusterOpsReconciler) scopsNodeRecycleRestarting(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := cluster.Status.NodeRecycleStatus

	if !nrs.PhaseTriggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, nodeUUID)
		if res := r.scopsNodeRecycleTriggerPhase(ctx, cluster, apiClient, clusterUUID, nodeUUID,
			[]string{utils.NodeStatusInRestart, utils.NodeStatusOnline},
			http.MethodPost, endpoint, map[string]bool{"force": true}, "restart",
		); res != nil {
			return *res, nil
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	nodes, err := listClusterStorageNodeSets(ctx, apiClient, clusterUUID)
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
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *StorageClusterOpsReconciler) scopsNodeRecycleRebalancing(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nrs := cluster.Status.NodeRecycleStatus

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
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
		nrs.NodePhase = nodeRecycleFirstPhase(cluster)
		nrs.PhaseTriggered = false
	}
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

// ── Pod helpers ───────────────────────────────────────────────────────────────

func (r *StorageClusterOpsReconciler) scopsDeleteStorageNodeSetPod(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (bool, error) {
	nodes, err := listClusterStorageNodeSets(ctx, apiClient, clusterUUID)
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
		return false, nil
	}
	k8sNodeName, err := r.scopsFindK8sNodeByIP(ctx, nodeIP)
	if err != nil {
		return false, fmt.Errorf("find k8s node for IP %s: %w", nodeIP, err)
	}
	pod, err := r.scopsFindStorageNodeSetPod(ctx, cluster.Namespace, cluster.Name, k8sNodeName)
	if err != nil {
		return false, fmt.Errorf("find storage node pod on %s: %w", k8sNodeName, err)
	}
	if pod == nil {
		return true, nil
	}
	return true, client.IgnoreNotFound(r.Delete(ctx, pod))
}

func (r *StorageClusterOpsReconciler) scopsIsStorageNodeSetPodReady(
	ctx context.Context,
	cluster *simplyblockv1alpha1.StorageCluster,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (bool, error) {
	nodes, err := listClusterStorageNodeSets(ctx, apiClient, clusterUUID)
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
	k8sNodeName, err := r.scopsFindK8sNodeByIP(ctx, nodeIP)
	if err != nil {
		return false, err
	}
	pod, err := r.scopsFindStorageNodeSetPod(ctx, cluster.Namespace, cluster.Name, k8sNodeName)
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

func (r *StorageClusterOpsReconciler) scopsFindK8sNodeByIP(ctx context.Context, ip string) (string, error) {
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

func (r *StorageClusterOpsReconciler) scopsFindStorageNodeSetPod(
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

// ── package-level helpers ─────────────────────────────────────────────────────

func nodeRecycleFirstPhase(_ *simplyblockv1alpha1.StorageCluster) string {
	return utils.NodeRecyclePhaseShuttingDown
}

func listClusterStorageNodeSets(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
) ([]utils.NodeStatusResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
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
