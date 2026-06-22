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
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

var (
	snoPostTriggerDelay = 5 * time.Second
	snoPostTriggerSleep = time.Sleep

	snoWaitRetries       = 50
	snoWaitInterval      = 5 * time.Second
	snoWaitIntervalSleep = time.Sleep
)

// StorageNodeOperationReconciler reconciles StorageNodeOperation objects.
type StorageNodeOperationReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	TLSEnabled       bool
	TLSMutualEnabled bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeoperations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeoperations/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch

func (r *StorageNodeOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	opCR := &simplyblockv1alpha1.StorageNodeOperation{}
	if err := r.Get(ctx, req.NamespacedName, opCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal states — nothing left to do.
	if opCR.Status.Phase == simplyblockv1alpha1.StorageNodeOperationPhaseCompleted ||
		opCR.Status.Phase == simplyblockv1alpha1.StorageNodeOperationPhaseFailed {
		return ctrl.Result{}, nil
	}

	snCR := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      opCR.Spec.StorageNodeRef,
		Namespace: opCR.Namespace,
	}, snCR); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOperation(ctx, opCR, fmt.Sprintf("StorageNode %q not found", opCR.Spec.StorageNodeRef))
		}
		return ctrl.Result{}, err
	}

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, opCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing", "cluster", snCR.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Transition to Running on first reconcile.
	if opCR.Status.Phase == "" || opCR.Status.Phase == simplyblockv1alpha1.StorageNodeOperationPhasePending {
		now := metav1.Now()
		opCR.Status.Phase = simplyblockv1alpha1.StorageNodeOperationPhaseRunning
		opCR.Status.StartedAt = &now
		opCR.Status.ObservedGeneration = opCR.Generation
		if err := r.Status().Update(ctx, opCR); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.executeOperation(ctx, opCR, snCR, clusterUUID); err != nil {
		log.Error(err, "Operation failed",
			"action", opCR.Spec.Action,
			"nodeUUID", opCR.Spec.NodeUUID,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *StorageNodeOperationReconciler) executeOperation(
	ctx context.Context,
	opCR *simplyblockv1alpha1.StorageNodeOperation,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
) error {
	log := logf.FromContext(ctx)
	apiClient := webapi.NewClient()

	if !opCR.Status.Triggered {
		if err := r.triggerOperation(ctx, apiClient, opCR, snCR, clusterUUID); err != nil {
			return r.recordFailure(ctx, opCR, err)
		}
		opCR.Status.Triggered = true
		if err := r.Status().Update(ctx, opCR); err != nil {
			return err
		}
	}

	if err := r.waitForOperationCompletion(ctx, apiClient, clusterUUID, opCR.Spec.NodeUUID, opCR.Spec.Action); err != nil {
		return r.recordFailure(ctx, opCR, fmt.Errorf(
			"node did not reach expected state after action %s: %w",
			opCR.Spec.Action,
			err,
		))
	}

	log.Info("Operation completed successfully",
		"action", opCR.Spec.Action,
		"nodeUUID", opCR.Spec.NodeUUID,
	)

	now := metav1.Now()
	opCR.Status.Phase = simplyblockv1alpha1.StorageNodeOperationPhaseCompleted
	opCR.Status.Message = "Operation completed successfully"
	opCR.Status.CompletedAt = &now
	return r.Status().Update(ctx, opCR)
}

func (r *StorageNodeOperationReconciler) triggerOperation(
	ctx context.Context,
	apiClient *webapi.Client,
	opCR *simplyblockv1alpha1.StorageNodeOperation,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
) error {
	log := logf.FromContext(ctx)

	var (
		endpoint string
		method   = http.MethodPost
		body     any
	)

	switch opCR.Spec.Action {

	case "restart":
		payload := map[string]any{
			"force":           operationForce(opCR, true),
			"reattach_volume": utils.BoolPtrOrFalse(opCR.Spec.ReattachVolume),
		}

		if opCR.Spec.WorkerNode != "" {
			if err := r.labelWorkerNodeForOperation(ctx, opCR.Spec.WorkerNode, snCR.Spec.ClusterName); err != nil {
				return fmt.Errorf("failed to label worker node %s: %w", opCR.Spec.WorkerNode, err)
			}

			if err := r.ensureWorkerInEndpointSlice(ctx, snCR, opCR.Spec.WorkerNode); err != nil {
				return fmt.Errorf("failed to ensure endpoint for worker %s: %w", opCR.Spec.WorkerNode, err)
			}

			if err := waitForNodeInfoReachable(ctx, opCR.Spec.WorkerNode, opCR.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
				return fmt.Errorf("node %s never became reachable: %w", opCR.Spec.WorkerNode, err)
			}

			body = map[string]any{
				"force":           operationForce(opCR, true),
				"reattach_volume": utils.BoolPtrOrFalse(opCR.Spec.ReattachVolume),
				"node_address":    utils.StorageNodeSetAPIAddress(opCR.Spec.WorkerNode, opCR.Namespace),
			}
		} else {
			body = payload
		}

		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/restart",
			clusterUUID,
			opCR.Spec.NodeUUID,
		)

	case "remove":
		method = http.MethodDelete
		body = nil
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s?force_remove=%t",
			clusterUUID,
			opCR.Spec.NodeUUID,
			operationForce(opCR, true),
		)

	default:
		body = nil
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/%s",
			clusterUUID,
			opCR.Spec.NodeUUID,
			opCR.Spec.Action,
		)
	}

	respBody, status, err := apiClient.Do(ctx, method, endpoint, body)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Node action API call failed",
			"action", opCR.Spec.Action,
			"nodeUUID", opCR.Spec.NodeUUID,
			"status", status,
			"response", string(respBody),
		)
		return fmt.Errorf("action API failed: status=%d err=%v", status, err)
	}

	log.Info("Node action triggered",
		"action", opCR.Spec.Action,
		"nodeUUID", opCR.Spec.NodeUUID,
		"response", string(respBody),
	)

	snoPostTriggerSleep(snoPostTriggerDelay)
	return nil
}

func (r *StorageNodeOperationReconciler) waitForOperationCompletion(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
	action string,
) error {
	log := logf.FromContext(ctx)

	expectedStatus := map[string]string{
		"suspend":  "suspended",
		"resume":   "online",
		"shutdown": "offline",
		"restart":  "online",
		"remove":   "removed",
	}

	targetStatus, ok := expectedStatus[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)

	for i := 0; i < snoWaitRetries; i++ {
		body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)

		if action == "remove" && status == http.StatusNotFound {
			log.Info("Node successfully removed (404 returned)", "nodeUUID", nodeUUID)
			return nil
		}

		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d", status)
			}
			log.Error(err, "Failed to get node status",
				"nodeUUID", nodeUUID,
				"status", status,
				"response", string(body),
			)
			snoWaitIntervalSleep(snoWaitInterval)
			continue
		}

		var resp utils.NodeStatusResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Error(err, "Failed to parse node status response", "body", string(body))
			snoWaitIntervalSleep(snoWaitInterval)
			continue
		}

		if resp.Status == targetStatus {
			log.Info("Node reached expected status", "nodeUUID", nodeUUID, "status", resp.Status)
			return nil
		}

		snoWaitIntervalSleep(snoWaitInterval)
	}

	return fmt.Errorf("node %s did not reach expected status %q after action %q", nodeUUID, targetStatus, action)
}

func (r *StorageNodeOperationReconciler) labelWorkerNodeForOperation(
	ctx context.Context,
	workerNode string,
	clusterName string,
) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: workerNode}, &node); err != nil {
		return err
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	key := "io.simplyblock.node-type"
	value := "simplyblock-storage-plane-" + clusterName
	if node.Labels[key] == value {
		return nil
	}
	node.Labels[key] = value
	return r.Update(ctx, &node)
}

// ensureWorkerInEndpointSlice adds workerNode to the StorageNode's headless-service
// EndpointSlice so DNS resolves before the node is reachable via normal reconcile.
// spec.workerNode holds the migration target but is never part of spec.workerNodes, so
// reconcileEndpointSlice would never add a DNS hostname entry for it.
func (r *StorageNodeOperationReconciler) ensureWorkerInEndpointSlice(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	workerNode string,
) error {
	if slices.Contains(snCR.Spec.WorkerNodes, workerNode) {
		return nil
	}

	ip, err := getNodeInternalIP(ctx, r.Client, workerNode)
	if err != nil {
		return fmt.Errorf("failed to get IP for worker %s: %w", workerNode, err)
	}

	log := logf.FromContext(ctx)
	nodeIPs := make(map[string]string, len(snCR.Spec.WorkerNodes)+1)
	for _, nodeName := range snCR.Spec.WorkerNodes {
		nodeIP, err := getNodeInternalIP(ctx, r.Client, nodeName)
		if err != nil {
			log.Error(err, "failed to get internal IP for EndpointSlice, skipping node", "node", nodeName)
			continue
		}
		nodeIPs[nodeName] = nodeIP
	}
	nodeIPs[workerNode] = ip

	eps := utils.BuildStorageNodeSetEndpointSlice(snCR, nodeIPs)
	if err := controllerutil.SetControllerReference(snCR, eps, r.Scheme); err != nil {
		return fmt.Errorf("failed to set EndpointSlice owner reference: %w", err)
	}

	var existing discoveryv1.EndpointSlice
	getErr := r.Get(ctx, client.ObjectKeyFromObject(eps), &existing)
	if apierrors.IsNotFound(getErr) {
		return r.Create(ctx, eps)
	}
	if getErr != nil {
		return getErr
	}

	eps.ResourceVersion = existing.ResourceVersion
	return r.Update(ctx, eps)
}

func (r *StorageNodeOperationReconciler) recordFailure(ctx context.Context, opCR *simplyblockv1alpha1.StorageNodeOperation, err error) error {
	now := metav1.Now()
	opCR.Status.Phase = simplyblockv1alpha1.StorageNodeOperationPhaseFailed
	opCR.Status.Message = err.Error()
	opCR.Status.CompletedAt = &now
	_ = r.Status().Update(ctx, opCR)
	return err
}

func (r *StorageNodeOperationReconciler) failOperation(ctx context.Context, opCR *simplyblockv1alpha1.StorageNodeOperation, message string) (ctrl.Result, error) {
	now := metav1.Now()
	opCR.Status.Phase = simplyblockv1alpha1.StorageNodeOperationPhaseFailed
	opCR.Status.Message = message
	opCR.Status.CompletedAt = &now
	_ = r.Status().Update(ctx, opCR)
	return ctrl.Result{}, nil
}

func operationForce(opCR *simplyblockv1alpha1.StorageNodeOperation, defaultValue bool) bool {
	if opCR.Spec.Force == nil {
		return defaultValue
	}
	return *opCR.Spec.Force
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageNodeOperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNodeOperation{}).
		Named("storagenodeoperation").
		Complete(r)
}
