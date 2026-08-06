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
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// StorageClusterOpsReconciler reconciles ClusterOps resources.
type StorageClusterOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *StorageClusterOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ops simplyblockv1alpha1.StorageClusterOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: release the cluster lock before the CR disappears.
	if !ops.DeletionTimestamp.IsZero() {
		var cluster simplyblockv1alpha1.StorageCluster
		if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}, &cluster); err == nil {
			r.releaseClusterLock(ctx, &ops, &cluster)
		}
		controllerutil.RemoveFinalizer(&ops, utils.FinalizerStorageClusterOps)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// Ensure our finalizer is present so we can clear activeOpsRef on deletion.
	if !controllerutil.ContainsFinalizer(&ops, utils.FinalizerStorageClusterOps) {
		controllerutil.AddFinalizer(&ops, utils.FinalizerStorageClusterOps)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// Terminal phases — remove the finalizer so the CR can be GC'd, then stop.
	switch ops.Status.Phase {
	case simplyblockv1alpha1.StorageClusterOpsPhaseSucceeded, simplyblockv1alpha1.StorageClusterOpsPhaseFailed:
		if controllerutil.ContainsFinalizer(&ops, utils.FinalizerStorageClusterOps) {
			controllerutil.RemoveFinalizer(&ops, utils.FinalizerStorageClusterOps)
			return ctrl.Result{}, r.Update(ctx, &ops)
		}
		return ctrl.Result{}, nil
	}

	// Fetch the target cluster.
	var cluster simplyblockv1alpha1.StorageCluster
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOps(ctx, &ops, nil, fmt.Sprintf("cluster %q not found", ops.Spec.ClusterRef))
		}
		return ctrl.Result{}, fmt.Errorf("get cluster %q: %w", ops.Spec.ClusterRef, err)
	}

	// Mutual exclusion: if another StorageClusterOps is active on this cluster, stay Pending.
	if cluster.Status.ActiveOpsRef != "" && cluster.Status.ActiveOpsRef != ops.Name {
		log.Info("Another StorageClusterOps is active, staying Pending",
			"ops", ops.Name, "activeOps", cluster.Status.ActiveOpsRef)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Acquire the lock: set activeOpsRef on the cluster.
	if cluster.Status.ActiveOpsRef != ops.Name {
		patch := client.MergeFrom(cluster.DeepCopy())
		cluster.Status.ActiveOpsRef = ops.Name
		if err := r.Status().Patch(ctx, &cluster, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("set activeOpsRef: %w", err)
		}
	}

	// Transition to Running if still Pending.
	if ops.Status.Phase == simplyblockv1alpha1.StorageClusterOpsPhasePending || ops.Status.Phase == "" {
		now := metav1.Now()
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseRunning
		ops.Status.StartedAt = &now
		if err := r.Status().Patch(ctx, &ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Dispatch to operation handler.
	switch ops.Spec.Action {
	case utils.ClusterActionActivate:
		return r.reconcileActivate(ctx, &ops, &cluster)
	case utils.ClusterActionExpand:
		return r.reconcileExpand(ctx, &ops, &cluster)
	case utils.ClusterActionShutdown:
		return r.reconcileShutdown(ctx, &ops, &cluster)
	case utils.ClusterActionStart:
		return r.reconcileStart(ctx, &ops, &cluster)
	case utils.ClusterActionRestart:
		return r.reconcileRestart(ctx, &ops, &cluster)
	case utils.ClusterActionNodeRecycle:
		return r.reconcileNodeRecycle(ctx, &ops, &cluster)
	default:
		return r.failOps(ctx, &ops, &cluster, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
}

// reconcileActivate handles the activate operation: POST then poll until active.
func (r *StorageClusterOpsReconciler) reconcileActivate(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	apiClient := webapi.NewClient()

	if !ops.Status.Triggered {
		clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
		if err != nil {
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("resolve cluster UUID: %v", err))
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/activate", clusterUUID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "activate POST failed", "cluster", cluster.Name)
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("activate POST failed: %v", err))
		}

		log.Info("Cluster activate POST sent", "cluster", cluster.Name)
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = "activate POST sent, polling for active status"
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Poll cluster status.
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, cluster.Namespace, cluster.Name)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "activate poll GET failed", "cluster", cluster.Name)
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("activate poll failed: %v", err))
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("parse cluster response: %v", err))
	}

	if resp.Status != utils.ClusterStatusActive {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Update cluster status.
	clusterPatch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.Status = utils.ClusterStatusActive
	cluster.Status.UUID = resp.UUID
	cluster.Status.NQN = resp.NQN
	cluster.Status.ClusterName = cluster.Name
	cluster.Status.Configured = true
	cluster.Status.Rebalancing = &resp.Rebalancing
	cluster.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", resp.NDCS, resp.NPCS)
	mft := int32(resp.MaxFaultTolerance)
	cluster.Status.MaxFaultTolerance = &mft
	if err := r.Status().Patch(ctx, cluster, clusterPatch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Cluster activated successfully", "cluster", cluster.Name)
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "Activated", "Activated",
		"Cluster %s activated successfully", cluster.Name)
	return r.succeedOps(ctx, ops, cluster, "Cluster activated successfully")
}

// reconcileExpand handles the expand operation: POST then poll until active.
func (r *StorageClusterOpsReconciler) reconcileExpand(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	apiClient := webapi.NewClient()

	if !ops.Status.Triggered {
		clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
		if err != nil {
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("resolve cluster UUID: %v", err))
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/expand", clusterUUID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "expand POST failed", "cluster", cluster.Name)
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("expand POST failed: %v", err))
		}

		log.Info("Cluster expand POST sent", "cluster", cluster.Name)
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = "expand POST sent, polling for active status"
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Poll cluster status.
	clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "expand poll GET failed", "cluster", cluster.Name)
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("expand poll failed: %v", err))
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("parse cluster response: %v", err))
	}

	if resp.Status != utils.ClusterStatusActive {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Update cluster status.
	clusterPatch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.Status = utils.ClusterStatusActive
	cluster.Status.UUID = resp.UUID
	cluster.Status.NQN = resp.NQN
	cluster.Status.ClusterName = cluster.Name
	cluster.Status.Configured = true
	cluster.Status.Rebalancing = &resp.Rebalancing
	cluster.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", resp.NDCS, resp.NPCS)
	mft := int32(resp.MaxFaultTolerance)
	cluster.Status.MaxFaultTolerance = &mft
	if err := r.Status().Patch(ctx, cluster, clusterPatch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Cluster expanded successfully", "cluster", cluster.Name)
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "Expanded", "Expanded",
		"Cluster %s expanded successfully", cluster.Name)
	return r.succeedOps(ctx, ops, cluster, "Cluster expanded successfully")
}

// reconcileShutdown sends POST /shutdown and polls until the cluster is no longer active.
func (r *StorageClusterOpsReconciler) reconcileShutdown(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	apiClient := webapi.NewClient()

	clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
	if err != nil {
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	if !ops.Status.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/shutdown", clusterUUID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "shutdown POST failed", "cluster", cluster.Name)
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("shutdown POST failed: %v", err))
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = "Shutting down — waiting for cluster to go offline"
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster shutdown POST sent, polling", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		log.Error(err, "shutdown poll GET failed", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "shutdown poll: parse cluster response failed")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if resp.Status == utils.ClusterStatusActive {
		log.Info("Cluster still active, waiting for shutdown", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Cluster shut down successfully", "cluster", cluster.Name, "status", resp.Status)
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "Shutdown", "Shutdown",
		"Cluster %s shut down successfully", cluster.Name)
	return r.succeedOps(ctx, ops, cluster, "Cluster shut down successfully")
}

// reconcileStart sends POST /start and polls until the cluster is active.
func (r *StorageClusterOpsReconciler) reconcileStart(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	apiClient := webapi.NewClient()

	clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
	if err != nil {
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	if !ops.Status.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/start", clusterUUID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "start POST failed", "cluster", cluster.Name)
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("start POST failed: %v", err))
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = "Starting — waiting for cluster to become active"
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster start POST sent, polling", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		log.Error(err, "start poll GET failed", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "start poll: parse cluster response failed")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if resp.Status != utils.ClusterStatusActive {
		log.Info("Cluster not yet active, waiting", "cluster", cluster.Name, "status", resp.Status)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Cluster started successfully", "cluster", cluster.Name)
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "Started", "Started",
		"Cluster %s started successfully", cluster.Name)
	return r.succeedOps(ctx, ops, cluster, "Cluster started successfully")
}

// reconcileRestart handles a two-phase cluster restart: POST /shutdown, wait
// until the cluster leaves "active", then POST /start and wait until it returns
// to "active". The sub-phase is tracked in ops.Status.Message.
func (r *StorageClusterOpsReconciler) reconcileRestart(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	const (
		subPhaseShuttingDown = "shutting-down"
		subPhaseStarting     = "starting"
	)
	log := logf.FromContext(ctx)
	apiClient := webapi.NewClient()

	clusterUUID, err := utils.GetClusterID(ctx, apiClient, cluster)
	if err != nil {
		return r.failOps(ctx, ops, cluster, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	// Phase 1: send shutdown.
	if !ops.Status.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/shutdown", clusterUUID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "restart: shutdown POST failed", "cluster", cluster.Name)
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("restart shutdown POST failed: %v", err))
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = subPhaseShuttingDown
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster shutdown POST sent for restart", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Poll current cluster status from the API.
	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "restart: cluster status poll failed", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "restart: parse cluster response failed")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Phase 2: wait for shutdown to complete, then send start.
	if ops.Status.Message == subPhaseShuttingDown {
		if resp.Status == utils.ClusterStatusActive {
			log.Info("Waiting for cluster to leave active state after shutdown", "cluster", cluster.Name, "status", resp.Status)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		startEndpoint := fmt.Sprintf("/api/v2/clusters/%s/start", clusterUUID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, startEndpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "restart: start POST failed", "cluster", cluster.Name)
			return r.failOps(ctx, ops, cluster, fmt.Sprintf("restart start POST failed: %v", err))
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Message = subPhaseStarting
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("Cluster start POST sent for restart", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Phase 3: wait for cluster to become active again.
	if resp.Status != utils.ClusterStatusActive {
		log.Info("Waiting for cluster to become active after start", "cluster", cluster.Name, "status", resp.Status)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Cluster restarted successfully", "cluster", cluster.Name)
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "Restarted", "Restarted",
		"Cluster %s restarted successfully", cluster.Name)
	return r.succeedOps(ctx, ops, cluster, "Cluster restarted successfully")
}

// succeedOps transitions ops to Succeeded and releases the cluster lock.
func (r *StorageClusterOpsReconciler) succeedOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseSucceeded
	ops.Status.Message = message
	ops.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.releaseClusterLock(ctx, ops, cluster)
	return ctrl.Result{}, nil
}

// failOps transitions ops to Failed, emits an event, and releases the cluster lock.
func (r *StorageClusterOpsReconciler) failOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
	reason string,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseFailed
	ops.Status.Message = reason
	ops.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "Failed", "Failed",
		"StorageClusterOps %s failed: %s", ops.Name, reason)
	r.releaseClusterLock(ctx, ops, cluster)
	return ctrl.Result{}, nil
}

// releaseClusterLock clears activeOpsRef on the cluster if it still points to this ops.
func (r *StorageClusterOpsReconciler) releaseClusterLock(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageClusterOps,
	cluster *simplyblockv1alpha1.StorageCluster,
) {
	if cluster == nil {
		return
	}
	if cluster.Status.ActiveOpsRef != ops.Name {
		return
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.ActiveOpsRef = ""
	_ = r.Status().Patch(ctx, cluster, patch)
}

// clusterToOpsRequests maps a StorageCluster event to any pending ClusterOps
// targeting it, so they wake up immediately when the cluster lock is released.
func (r *StorageClusterOpsReconciler) clusterToOpsRequests(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster := obj.(*simplyblockv1alpha1.StorageCluster)
	var opsList simplyblockv1alpha1.StorageClusterOpsList
	if err := r.List(ctx, &opsList,
		client.InNamespace(cluster.Namespace),
		client.MatchingFields{"spec.clusterRef": cluster.Name},
	); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, ops := range opsList.Items {
		if ops.Status.Phase == simplyblockv1alpha1.StorageClusterOpsPhasePending ||
			ops.Status.Phase == simplyblockv1alpha1.StorageClusterOpsPhaseRunning ||
			ops.Status.Phase == "" {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Name:      ops.Name,
				Namespace: ops.Namespace,
			}})
		}
	}
	return reqs
}

func (r *StorageClusterOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.StorageClusterOps{},
		"spec.clusterRef",
		func(obj client.Object) []string {
			ops := obj.(*simplyblockv1alpha1.StorageClusterOps)
			return []string{ops.Spec.ClusterRef}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageClusterOps{}).
		Named("storageclusterops").
		Watches(
			&simplyblockv1alpha1.StorageCluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToOpsRequests),
		).
		Complete(r)
}
