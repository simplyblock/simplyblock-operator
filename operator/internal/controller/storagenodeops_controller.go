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
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// StorageNodeOpsReconciler drives all imperative StorageNode operations.
// It replaces the existing action-handling in StorageNodeSetReconciler and
// owns VolumeMigration CRs during drain (action=remove).
type StorageNodeOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *StorageNodeOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ops simplyblockv1alpha1.StorageNodeOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Terminal — nothing left to do.
	if ops.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded ||
		ops.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Fetch the target StorageNode.
	var sn simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, types.NamespacedName{
		Name:      ops.Spec.StorageNodeRef,
		Namespace: ops.Namespace,
	}, &sn); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOps(ctx, &ops, "target StorageNode not found")
		}
		return ctrl.Result{}, err
	}

	// Fetch the parent StorageNodeSet for cluster config.
	var sns simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sn.Spec.StorageNodeSetRef,
		Namespace: sn.Namespace,
	}, &sns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve cluster UUID.
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, sn.Namespace, sns.Spec.ClusterName)
	if err != nil {
		log.Info("cluster UUID not ready, requeuing", "cluster", sns.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	// Mutual exclusion: only one ops may run per StorageNode at a time.
	if ops.Status.Phase == "" || ops.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhasePending {
		return r.acquireLock(ctx, &ops, &sn)
	}

	// Cluster pause check for drain operations.
	if ops.Spec.Action == utils.NodeActionRemove {
		if res, paused := r.clusterPauseCheck(ctx, &ops, apiClient); paused {
			return res, nil
		}
	}

	log.Info("dispatching ops", "action", ops.Spec.Action, "subPhase", ops.Status.SubPhase)
	return r.dispatch(ctx, &ops, &sn, &sns, clusterUUID, apiClient)
}

// acquireLock attempts to set StorageNode.status.activeOpsRef to this ops.
// Requeues if another ops holds the lock.
func (r *StorageNodeOpsReconciler) acquireLock(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if sn.Status.ActiveOpsRef != "" && sn.Status.ActiveOpsRef != ops.Name {
		log.Info("another ops is active, requeuing", "activeOps", sn.Status.ActiveOpsRef)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	snPatch := client.MergeFrom(sn.DeepCopy())
	sn.Status.ActiveOpsRef = ops.Name
	if err := r.Status().Patch(ctx, sn, snPatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting activeOpsRef: %w", err)
	}

	now := metav1.Now()
	opsPatch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	ops.Status.StartedAt = &now
	if ops.Spec.Action == utils.NodeActionRemove {
		ops.Status.SubPhase = simplyblockv1alpha1.StorageNodeOpsSubPhaseValidating
	}
	if err := r.Status().Patch(ctx, ops, opsPatch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// dispatch routes the ops to the correct handler.
func (r *StorageNodeOpsReconciler) dispatch(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	switch ops.Spec.Action {
	case utils.NodeActionRemove:
		return r.runDrain(ctx, ops, sn, clusterUUID, apiClient)
	case utils.NodeActionMigrate:
		return r.runMigrate(ctx, ops, sn, sns, clusterUUID, apiClient)
	case "shutdown", "restart", "suspend", "resume":
		return r.runSimpleAction(ctx, ops, sn, sns, clusterUUID, apiClient)
	default:
		return r.failOps(ctx, ops, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
}

// runSimpleAction handles shutdown / restart / suspend / resume by posting to
// the backend and polling until the node reaches its terminal status.
func (r *StorageNodeOpsReconciler) runSimpleAction(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	_ *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID
	action := ops.Spec.Action

	// POST the action if not yet triggered.
	if !ops.Status.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/%s",
			clusterUUID, nodeUUID, action)
		body := map[string]interface{}{}
		if ops.Spec.Force != nil && *ops.Spec.Force {
			body["force"] = true
		}
		if action == "restart" && ops.Spec.ReattachVolume != nil {
			body["reattach_volume"] = *ops.Spec.ReattachVolume
		}
		_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, body)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d", status)
			}
			log.Error(err, "action POST failed", "action", action, "nodeUUID", nodeUUID)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = fmt.Sprintf("%s request sent, waiting for node", action)
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Poll node status until terminal.
	terminalStatus := map[string]string{
		"suspend":  utils.NodeStatusSuspended,
		"resume":   utils.NodeStatusOnline,
		"restart":  utils.NodeStatusOnline,
		"shutdown": "offline",
	}
	want := terminalStatus[action]

	currentStatus, err := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "failed to get node status during action poll")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if currentStatus == want {
		return r.succeedOps(ctx, ops, sn)
	}
	log.Info("waiting for node to reach terminal status",
		"want", want, "current", currentStatus, "action", action)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// runMigrate relocates a storage node onto a different worker host. A migration
// is not a drain: the node keeps its UUID and its partitions/logical-volume
// assignments follow it. It is a restart with a target node_address — the same
// primitive the node-drain coordinator uses to bring a node back after a reboot
// (see nodedrain_controller.go), but pointed at a different worker's
// storage-node-api pod instead of the current one.
//
// The target worker must already be part of the StorageNodeSet topology (labeled
// and running a storage-node-api pod), otherwise node_address is unreachable.
func (r *StorageNodeOpsReconciler) runMigrate(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	nodeUUID := sn.Status.UUID
	target := ops.Spec.TargetWorkerNode

	// Validate the request.
	if target == "" {
		return r.failOps(ctx, ops, "targetWorkerNode is required for action=migrate")
	}
	if target == sn.Spec.WorkerNode {
		return r.failOps(ctx, ops, fmt.Sprintf("targetWorkerNode %q is the node's current worker", target))
	}

	// runMigrate is a three-phase state machine tracked via ops.Status.SubPhase:
	//
	//	Preparing → Migrating → Promoting
	//
	// Each phase issues its one-shot control-plane call gated by
	// ops.Status.Triggered, which advanceSubPhase resets to false on every
	// transition, so a requeue within a phase never repeats the POST.
	switch ops.Status.SubPhase {
	case "":
		// Enter the state machine. Persist Preparing first so the phase is
		// observable before any preparation work begins.
		return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhasePreparing)
	case simplyblockv1alpha1.StorageNodeOpsSubPhasePreparing:
		return r.migratePrepare(ctx, ops, sn, sns, target)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating:
		return r.migrateRestart(ctx, ops, sn, target, clusterUUID, nodeUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhasePromoting:
		return r.migratePromote(ctx, ops, sn, sns, target, clusterUUID, nodeUUID, apiClient)
	default:
		return r.failOps(ctx, ops, fmt.Sprintf("migrate: unexpected sub-phase %q", ops.Status.SubPhase))
	}
}

// migratePrepare runs the Preparing sub-phase: it clones the source worker's
// per-node config onto the target, labels the target into the storage plane so
// the DaemonSet schedules a storage-node-api pod there, then blocks until that
// pod is Ready AND the target's per-pod DNS name is published in the
// storage-node-api EndpointSlice. Only then can the control-plane restart
// resolve node_address, so the phase does not advance to Migrating until the
// DNS precondition holds — otherwise the restart fails name resolution and the
// control plane resets the node to OFFLINE.
func (r *StorageNodeOpsReconciler) migratePrepare(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	target string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: target}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOps(ctx, ops, fmt.Sprintf("target worker node %q not found in the cluster", target))
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if !isNodeReady(&node) {
		return r.failOps(ctx, ops, fmt.Sprintf("target worker node %q is not Ready", target))
	}

	// Clone the source worker's per-node config onto the target before the
	// storage-node pod is scheduled there, so it boots with the same effective
	// configuration as the node being migrated. Any additional NVMe devices
	// requested via spec.newSsdPcie are merged into the cloned PCI_ALLOWED so the
	// target host binds them on start. Done before labeling so the entry exists
	// by the time the pod's init container sources it.
	if err := r.ensureMigratedWorkerConfig(ctx, sns, sn.Spec.WorkerNode, target, ops.Spec.NewSsdPcie); err != nil {
		log.Error(err, "migrate: failed to clone per-node config to target worker",
			"source", sn.Spec.WorkerNode, "target", target)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Label the target so the storage-node DaemonSet schedules a storage-node-api
	// pod there and the StorageNodeSet reconcile publishes its per-pod DNS name
	// in the EndpointSlice.
	if labeled, err := r.ensureWorkerLabeled(ctx, &node, sns.Spec.ClusterName); err != nil {
		log.Error(err, "migrate: failed to label target worker", "worker", target)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	} else if labeled {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "TargetWorkerLabeled", "TargetWorkerLabeled",
			"labeled worker %s for storage plane of cluster %s", target, sns.Spec.ClusterName)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "TargetWorkerLabeled",
			fmt.Sprintf("labeled worker %s for storage plane of cluster %s", target, sns.Spec.ClusterName))
	}

	// Wait for the storage-node-api pod to be Running+Ready on the target.
	ready, err := r.storageNodePodReady(ctx, sns.Namespace, sns.Spec.ClusterName, target)
	if err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if !ready {
		return r.migrateWaiting(ctx, ops, fmt.Sprintf("waiting for storage-node pod on worker %s", target))
	}

	// Gate: the control plane resolves node_address via the target's per-pod
	// headless DNS name, which only exists once the target is published in the
	// storage-node-api EndpointSlice (built from labeled storage-plane nodes).
	// Hold in Preparing until the entry appears so the restart can resolve it.
	inSlice, err := r.endpointSliceHasWorker(ctx, sns.Namespace, target)
	if err != nil {
		log.Error(err, "migrate: failed to read storage-node-api EndpointSlice", "target", target)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if !inSlice {
		return r.migrateWaiting(ctx, ops,
			fmt.Sprintf("waiting for worker %s DNS to be published before restart", target))
	}

	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating)
}

// migrateRestart runs the Migrating sub-phase: it issues the control-plane
// restart pointed at the target worker's node_address (once, gated by
// Triggered) and then polls until the relocated node reports online on the
// target, at which point it advances to Promoting.
func (r *StorageNodeOpsReconciler) migrateRestart(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	target, clusterUUID, nodeUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !ops.Status.Triggered {
		payload := map[string]any{
			// Migration relocates a still-online node, so the control-plane
			// restart must run with force=true — a non-forced restart is rejected
			// unless the node is already OFFLINE ("Node must be offline"). Default
			// to true and honor an explicit spec.force override only when set.
			"force":        ops.Spec.Force == nil || *ops.Spec.Force,
			"node_address": utils.StorageNodeSetAPIAddress(target, sn.Namespace),
		}
		if ops.Spec.ReattachVolume != nil {
			payload["reattach_volume"] = *ops.Spec.ReattachVolume
		}
		if len(ops.Spec.NewSsdPcie) > 0 {
			payload["new_ssd_pcie"] = ops.Spec.NewSsdPcie
		}
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, nodeUUID)
		respBody, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, payload)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(respBody))
			}
			log.Error(err, "migrate: restart POST failed", "nodeUUID", nodeUUID, "target", target)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = fmt.Sprintf("migrating node %s to worker %s, waiting for online", nodeUUID, target)
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrateStarted", "MigrateStarted",
			"restart with target worker %s issued for node %s", target, nodeUUID)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "MigrateStarted",
			fmt.Sprintf("restart with target worker %s issued for node %s", target, nodeUUID))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	currentStatus, err := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "migrate: failed to get node status during poll")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if currentStatus != utils.NodeStatusOnline {
		return r.migrateWaiting(ctx, ops,
			fmt.Sprintf("waiting for node %s to come online on worker %s (status %s)", nodeUUID, target, currentStatus))
	}

	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhasePromoting)
}

// migratePromote runs the Promoting sub-phase: it issues the control-plane
// /promote for the relocated node (once, gated by Triggered), then re-points the
// Kubernetes topology onto the target worker and completes the op. /promote
// activates the new host's devices, fails and migrates the origin host's devices
// (starting a rebalance), sets the primary, and re-homes the logical volumes
// onto the relocated node.
func (r *StorageNodeOpsReconciler) migratePromote(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	target, clusterUUID, nodeUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !ops.Status.Triggered {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/promote", clusterUUID, nodeUUID)
		respBody, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(respBody))
			}
			log.Error(err, "migrate: promote POST failed", "nodeUUID", nodeUUID, "target", target)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = fmt.Sprintf("promoted node %s on worker %s; rebalance started", nodeUUID, target)
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("migrate: promoted relocated node; rebalance started", "nodeUUID", nodeUUID, "target", target)
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigratePromoted", "MigratePromoted",
			"promote issued for node %s on worker %s; rebalance started", nodeUUID, target)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "MigratePromoted",
			fmt.Sprintf("promote issued for node %s on worker %s; rebalance started", nodeUUID, target))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Promote issued — re-point the Kubernetes topology: update this StorageNode's
	// spec.workerNode and swap the owning StorageNodeSet's worker list (and status)
	// from the source to the target worker.
	if err := r.reconcileMigratedTopology(ctx, sn, sns, target, ops.Spec.NewSsdPcie); err != nil {
		log.Error(err, "migrate: failed to reconcile topology after migration", "target", target)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrateCompleted", "MigrateCompleted",
		"node %s is online on worker %s", nodeUUID, target)
	r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "MigrateCompleted",
		fmt.Sprintf("node %s is online on worker %s", nodeUUID, target))
	return r.succeedOps(ctx, ops, sn)
}

// migrateWaiting patches the op's status message and requeues without changing
// the sub-phase — used while a Preparing/Migrating precondition is still pending.
func (r *StorageNodeOpsReconciler) migrateWaiting(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	msg string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Message = msg
	_ = r.Status().Patch(ctx, ops, patch)
	logf.FromContext(ctx).Info("migrate: " + msg)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// endpointSliceHasWorker reports whether the storage-node-api EndpointSlice
// publishes the given worker's per-pod DNS hostname with at least one address —
// i.e. whether <worker>.simplyblock-storage-node-api.<ns>.svc resolves.
func (r *StorageNodeOpsReconciler) endpointSliceHasWorker(
	ctx context.Context,
	namespace, worker string,
) (bool, error) {
	var eps discoveryv1.EndpointSlice
	if err := r.Get(ctx, types.NamespacedName{
		Name:      "simplyblock-storage-node-api-endpoints",
		Namespace: namespace,
	}, &eps); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	want := utils.NodeHostnameLabel(worker)
	for i := range eps.Endpoints {
		e := eps.Endpoints[i]
		if e.Hostname != nil && *e.Hostname == want && len(e.Addresses) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// reconcileMigratedTopology re-points the operator's Kubernetes topology at the
// target worker after the backend node has moved. It updates this StorageNode's
// spec.workerNode in place (permitted for the operator service account; blocked
// for users by the StorageNode validating webhook) and swaps the owning
// StorageNodeSet.spec.workerNodes from the source worker to the target. It also
// migrates the per-node config source of truth: spec.nodeConfigs[source] is
// cloned onto spec.nodeConfigs[target] with newSsdPcie merged into its
// PcieAllowList, and the source entry is dropped. Persisting this keeps the
// StorageNodeSet reconciler from rebuilding the target's ConfigMap entry back to
// fleet defaults (which would drop the newly bound devices) and removes the
// stale source-host config.
func (r *StorageNodeOpsReconciler) reconcileMigratedTopology(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	target string,
	newSsdPcie []string,
) error {
	source := sn.Spec.WorkerNode

	// 1. Re-point the StorageNode CR at the target worker. The UUID and status
	//    are preserved — the same backend node simply runs on a different host.
	//    The name no longer encodes the worker, so the worker label is the
	//    human-facing indicator and is refreshed alongside the spec.
	if sn.Spec.WorkerNode != target {
		patch := client.MergeFrom(sn.DeepCopy())
		sn.Spec.WorkerNode = target
		if sn.Labels == nil {
			sn.Labels = map[string]string{}
		}
		sn.Labels["storage.simplyblock.io/worker"] = sanitiseDNSLabel(target)
		if err := r.Patch(ctx, sn, patch); err != nil {
			return fmt.Errorf("re-pointing StorageNode %s to worker %s: %w", sn.Name, target, err)
		}
	}

	// 2. Reconcile the StorageNodeSet worker list and per-node config: drop the
	//    source worker, add the target, and move nodeConfigs[source] to
	//    nodeConfigs[target] (with newSsdPcie merged in). Re-fetch to patch
	//    against the latest version.
	var fresh simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{Name: sns.Name, Namespace: sns.Namespace}, &fresh); err != nil {
		return err
	}

	// New worker list: source removed, target present.
	workers := make([]string, 0, len(fresh.Spec.WorkerNodes)+1)
	hasTarget := false
	workersChanged := false
	for _, w := range fresh.Spec.WorkerNodes {
		if w == source {
			workersChanged = true
			continue
		}
		if w == target {
			hasTarget = true
		}
		workers = append(workers, w)
	}
	if !hasTarget {
		workers = append(workers, target)
		workersChanged = true
	}

	// Migrated per-node config for the target. Prefer an existing target entry
	// (idempotent re-runs), otherwise clone the source's overrides. The effective
	// PcieAllowList is the entry's own list, or the fleet default when unset;
	// newSsdPcie is merged into it so the added devices persist across rebuilds.
	targetCfg, hadTargetCfg := fresh.Spec.NodeConfigs[target]
	_, hadSourceCfg := fresh.Spec.NodeConfigs[source]
	if !hadTargetCfg {
		targetCfg = fresh.Spec.NodeConfigs[source] // zero value if source has none
	}
	if len(newSsdPcie) > 0 {
		effPcie := targetCfg.PcieAllowList
		if len(effPcie) == 0 {
			effPcie = fresh.Spec.PcieAllowList
		}
		targetCfg.PcieAllowList = mergePcieList(effPcie, newSsdPcie)
	}
	setTarget := hadTargetCfg || hadSourceCfg || len(newSsdPcie) > 0

	if !workersChanged && !hadSourceCfg && !setTarget {
		// Spec already reconciled (idempotent re-run); still prune the stale
		// source-host status entry left behind by the move.
		return r.pruneMigratedSourceStatus(ctx, fresh.Name, fresh.Namespace, source, sn.Status.UUID)
	}

	patch := client.MergeFrom(fresh.DeepCopy())
	fresh.Spec.WorkerNodes = workers
	if hadSourceCfg {
		delete(fresh.Spec.NodeConfigs, source)
	}
	if setTarget {
		if fresh.Spec.NodeConfigs == nil {
			fresh.Spec.NodeConfigs = map[string]simplyblockv1alpha1.StorageNodeOverrides{}
		}
		fresh.Spec.NodeConfigs[target] = targetCfg
	}
	if err := r.Patch(ctx, &fresh, patch); err != nil {
		return fmt.Errorf("reconciling StorageNodeSet %s topology: %w", fresh.Name, err)
	}

	// 3. Prune the stale source-host entry from status. The status sync keys
	//    Status.Nodes by hostname, so once the migrated node reports from the
	//    target it is appended as a new entry while the source entry (same
	//    backend UUID) lingers. Drop it so the set reflects only live hosts.
	return r.pruneMigratedSourceStatus(ctx, fresh.Name, fresh.Namespace, source, sn.Status.UUID)
}

// pruneMigratedSourceStatus removes the StorageNodeSet.Status.Nodes entry left
// on the source host after a node migrated away — matched by the source
// hostname and the migrated node's backend UUID so sibling nodes still on that
// host (multi-socket) and unrelated in-flight (UUID=="") entries are untouched.
func (r *StorageNodeOpsReconciler) pruneMigratedSourceStatus(
	ctx context.Context,
	name, namespace, source, uuid string,
) error {
	if uuid == "" {
		return nil // cannot match safely without the migrated node's UUID
	}
	var sns simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &sns); err != nil {
		return err
	}
	kept := make([]simplyblockv1alpha1.NodeStatus, 0, len(sns.Status.Nodes))
	removed := false
	for _, n := range sns.Status.Nodes {
		if n.Hostname == source && n.UUID == uuid {
			removed = true
			continue
		}
		kept = append(kept, n)
	}
	if !removed {
		return nil
	}
	patch := client.MergeFrom(sns.DeepCopy())
	sns.Status.Nodes = kept
	if err := r.Status().Patch(ctx, &sns, patch); err != nil {
		return fmt.Errorf("pruning migrated source status entry for %s on %s: %w", uuid, source, err)
	}
	return nil
}

// ensureMigratedWorkerConfig clones the source worker's entry in the per-node
// ConfigMap onto the target worker so the storage-node pod scheduled on the
// target boots with the same effective configuration. When newSsdPcie is
// non-empty those PCIe addresses are merged into the cloned entry's PCI_ALLOWED.
//
// It writes the ConfigMap directly because the target is not yet in
// spec.workerNodes, so the StorageNodeSet reconciler would not build an entry
// for it. reconcileMigratedTopology later persists the durable source of truth
// into spec.nodeConfigs[target]. Idempotent: an existing target entry is left
// untouched.
func (r *StorageNodeOpsReconciler) ensureMigratedWorkerConfig(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
	source, target string,
	newSsdPcie []string,
) error {
	name := PerNodeConfigMapName(sns.Name)
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: sns.Namespace}, &cm); err != nil {
		return fmt.Errorf("getting per-node ConfigMap %s: %w", name, err)
	}
	if _, ok := cm.Data[target]; ok {
		return nil // already cloned
	}
	srcEntry, ok := cm.Data[source]
	if !ok {
		return fmt.Errorf("per-node ConfigMap %s has no entry for source worker %q", name, source)
	}

	patch := client.MergeFrom(cm.DeepCopy())
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[target] = mergePcieAllowedIntoEnvFile(srcEntry, newSsdPcie)
	if err := r.Patch(ctx, &cm, patch); err != nil {
		return fmt.Errorf("cloning per-node config %q -> %q: %w", source, target, err)
	}
	return nil
}

// mergePcieAllowedIntoEnvFile returns the per-node env-file text with extra PCIe
// addresses merged into its PCI_ALLOWED= line (deduplicated, order preserved,
// shell-quoted to match buildPerNodeEnvFile). The input is returned unchanged
// when extra is empty. A PCI_ALLOWED line is appended if none is present.
func mergePcieAllowedIntoEnvFile(envFile string, extra []string) string {
	if len(extra) == 0 {
		return envFile
	}
	const prefix = "PCI_ALLOWED="
	lines := strings.Split(envFile, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		current := parseShellCSV(strings.TrimPrefix(line, prefix))
		lines[i] = prefix + utils.ShellQuote(strings.Join(mergePcieList(current, extra), ","))
		return strings.Join(lines, "\n")
	}
	// No PCI_ALLOWED line: append one, preserving a single trailing newline.
	trimmed := strings.TrimRight(envFile, "\n")
	return trimmed + "\n" + prefix + utils.ShellQuote(strings.Join(mergePcieList(nil, extra), ",")) + "\n"
}

// parseShellCSV parses a shell-quoted, comma-separated value (as produced by
// utils.ShellQuote) into its elements, dropping empties.
func parseShellCSV(v string) []string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
		v = v[1 : len(v)-1]
		v = strings.ReplaceAll(v, `'\''`, "'") // undo ShellQuote escaping
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mergePcieList concatenates base and extra, dropping empties and duplicates
// while preserving first-seen order.
func mergePcieList(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, group := range [][]string{base, extra} {
		for _, s := range group {
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// ensureWorkerLabeled applies the storage-plane node label to a worker so the
// storage-node DaemonSet schedules a pod there. It mirrors
// StorageNodeSetReconciler.labelWorkerNodes. Returns true if a label was added.
func (r *StorageNodeOpsReconciler) ensureWorkerLabeled(
	ctx context.Context,
	node *corev1.Node,
	clusterName string,
) (bool, error) {
	const key = "io.simplyblock.node-type"
	value := "simplyblock-storage-plane-" + clusterName
	if node.Labels[key] == value {
		return false, nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[key] = value
	if err := r.Patch(ctx, node, patch); err != nil {
		return false, err
	}
	return true, nil
}

// storageNodePodReady reports whether the storage-node DaemonSet pod on the given
// worker is Running and Ready, which is the precondition for the control plane to
// reach that host's storage-node-api at node_address.
func (r *StorageNodeOpsReconciler) storageNodePodReady(
	ctx context.Context,
	namespace, clusterName, workerName string,
) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"app": "storage-node", "simplyblock-cluster": clusterName},
	); err != nil {
		return false, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != workerName || p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
	}
	return false, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Drain state machine (action=remove)
// Phases: Validating → Suspending → Migrating → Verifying → Removing
// ─────────────────────────────────────────────────────────────────────────────

func (r *StorageNodeOpsReconciler) runDrain(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	switch ops.Status.SubPhase {
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseValidating:
		return r.drainValidate(ctx, ops, sn, clusterUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseSuspending:
		return r.drainSuspend(ctx, ops, sn, clusterUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating:
		return r.drainMigrate(ctx, ops, sn, clusterUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseVerifying:
		return r.drainVerify(ctx, ops, sn, clusterUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving:
		return r.drainRemove(ctx, ops, sn, clusterUUID, apiClient)
	default:
		return r.failOps(ctx, ops, fmt.Sprintf("unknown drain sub-phase %q", ops.Status.SubPhase))
	}
}

func (r *StorageNodeOpsReconciler) drainValidate(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	volumes, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list volumes during validation")
		return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
	}

	sysFilter, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		return r.failOps(ctx, ops, "invalid systemVolumeFilterRegex: "+err.Error())
	}

	_, pinned, unmanaged, _, _, err := matchVolumesToPVs(ctx, r.Client, volumes, sysFilter)
	if err != nil {
		log.Error(err, "drain: matchVolumesToPVs failed during validation")
		return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
	}

	if len(pinned) > 0 {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "PinnedVolumeBlocking", "PinnedVolumeBlocking",
			"drain blocked: %d pinned volume(s) on node %s — remove the %s annotation to proceed",
			len(pinned), nodeUUID, simplyblockv1alpha1.AnnotationPinnedVolume)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "PinnedVolumeBlocking", fmt.Sprintf("drain blocked: %d pinned volume(s) on node %s — remove the %s annotation to proceed", len(pinned), nodeUUID, simplyblockv1alpha1.AnnotationPinnedVolume))
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Message = fmt.Sprintf("blocked: %d pinned volume(s) — remove simplyblock.io/pinned-volume annotation", len(pinned))
		_ = r.Status().Patch(ctx, ops, patch)
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	if len(unmanaged) > 0 {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "UnmanagedVolumeBlocking", "UnmanagedVolumeBlocking",
			"drain blocked: %d unmanaged volume(s) on node %s — remove them manually",
			len(unmanaged), nodeUUID)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "UnmanagedVolumeBlocking", fmt.Sprintf("drain blocked: %d unmanaged volume(s) on node %s — remove them manually", len(unmanaged), nodeUUID))
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Message = fmt.Sprintf("blocked: %d unmanaged volume(s) — remove manually", len(unmanaged))
		_ = r.Status().Patch(ctx, ops, patch)
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseSuspending)
}

func (r *StorageNodeOpsReconciler) drainSuspend(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	if !ops.Status.Triggered {
		currentStatus, err := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID)
		if err != nil {
			log.Error(err, "drain: could not read node status before suspend, retrying")
			return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
		}
		if currentStatus == utils.NodeStatusSuspended {
			log.Info("drain: node already suspended, advancing without POST")
			patch := client.MergeFrom(ops.DeepCopy())
			ops.Status.Triggered = true
			ops.Status.Message = "node already suspended"
			_ = r.Status().Patch(ctx, ops, patch)
			return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/suspend", clusterUUID, nodeUUID)
		_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("suspend API returned status %d", status)
			}
			log.Error(err, "drain: suspend POST failed")
			return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = "suspend request sent, waiting for node to suspend"
		_ = r.Status().Patch(ctx, ops, patch)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	// Poll node status.
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d", status)
		}
		log.Error(err, "drain: failed to GET node status during suspend poll")
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	var nodeResp utils.NodeStatusResponse
	if err := json.Unmarshal(body, &nodeResp); err != nil {
		log.Error(err, "drain: failed to unmarshal node status")
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	if nodeResp.Status != utils.NodeStatusSuspended {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "DrainSuspendPending", "DrainSuspendPending",
			"waiting for node %s to suspend (current status: %s)", nodeUUID, nodeResp.Status)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "DrainSuspendPending", fmt.Sprintf("waiting for node %s to suspend (current status: %s)", nodeUUID, nodeResp.Status))
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating)
}

func (r *StorageNodeOpsReconciler) drainMigrate(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	var vmigList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(ctx, &vmigList,
		client.InNamespace(ops.Namespace),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": nodeUUID},
	); err != nil {
		log.Error(err, "drain: failed to list VolumeMigration CRs")
		return ctrl.Result{RequeueAfter: drainRequeueMigrate}, nil
	}

	// Handle failed migrations.
	if res, handled := r.handleFailedVolumeMigrations(ctx, ops, apiClient, vmigList.Items); handled {
		return res, nil
	}

	completed, inProgress := 0, 0
	for i := range vmigList.Items {
		if vmigList.Items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseCompleted {
			completed++
		} else {
			inProgress++
		}
	}

	existingVMNames := make(map[string]struct{}, len(vmigList.Items))
	for i := range vmigList.Items {
		existingVMNames[vmigList.Items[i].Name] = struct{}{}
	}

	if len(vmigList.Items) == 0 || r.hasMissingVolumeMigrationsOps(ctx, apiClient, clusterUUID, nodeUUID, ops, existingVMNames) {
		return r.createMissingVolumeMigrationsOps(ctx, apiClient, clusterUUID, ops, sn, vmigList.Items, existingVMNames)
	}

	if inProgress == 0 && completed == len(vmigList.Items) {
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.VolumesMigrated = completed
		ops.Status.VolumesPending = 0
		_ = r.Status().Patch(ctx, ops, patch)

		for i := range vmigList.Items {
			vm := &vmigList.Items[i]
			if err := r.Delete(ctx, vm); err != nil {
				log.Error(err, "drain: failed to delete completed VolumeMigration", "name", vm.Name)
			}
		}
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrationCompleted", "MigrationCompleted",
			"all %d volume migrations completed", completed)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "MigrationCompleted", fmt.Sprintf("all %d volume migrations completed", completed))
		return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseVerifying)
	}

	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.VolumesMigrated = completed
	ops.Status.VolumesPending = inProgress
	ops.Status.Message = fmt.Sprintf("Migrating: %d of %d volumes migrated", completed, len(vmigList.Items))
	_ = r.Status().Patch(ctx, ops, patch)
	return ctrl.Result{RequeueAfter: drainRequeueMigrate}, nil
}

func (r *StorageNodeOpsReconciler) handleFailedVolumeMigrations(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	apiClient *webapi.Client,
	items []simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, bool) {
	log := logf.FromContext(ctx)
	var failed []simplyblockv1alpha1.VolumeMigration
	for i := range items {
		if items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed ||
			items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseAborted {
			failed = append(failed, items[i])
		}
	}
	if len(failed) == 0 {
		return ctrl.Result{}, false
	}

	// Check if the cluster is paused — if so, delete and wait.
	if res, paused := r.clusterPauseCheck(ctx, ops, apiClient); paused {
		for i := range failed {
			_ = r.Delete(ctx, &failed[i])
		}
		log.Info("drain: cluster not ready, deleted failed VMs and pausing", "count", len(failed))
		return res, true
	}

	// Cluster ready: delete failed CRs and let createMissingVolumeMigrationsOps recreate them.
	for i := range failed {
		vm := &failed[i]
		if err := r.Delete(ctx, vm); err != nil {
			log.Error(err, "drain: failed to delete failed VolumeMigration", "name", vm.Name)
			continue
		}
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "MigrationRetry", "MigrationRetry",
			"VolumeMigration %s failed, deleted and will retry with new target", vm.Name)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "MigrationRetry", fmt.Sprintf("VolumeMigration %s failed, deleted and will retry with new target", vm.Name))
	}
	return ctrl.Result{RequeueAfter: drainRequeueImmediate}, true
}

func (r *StorageNodeOpsReconciler) hasMissingVolumeMigrationsOps(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
	ops *simplyblockv1alpha1.StorageNodeOps,
	existingVMNames map[string]struct{},
) bool {
	vols, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		return false
	}
	sf, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		return false
	}
	pvm, _, _, pvByVol, _, err := matchVolumesToPVs(ctx, r.Client, vols, sf)
	if err != nil {
		return false
	}
	for _, volUUID := range pvm {
		if pvName, ok := pvByVol[volUUID]; ok {
			if _, exists := existingVMNames[drainMigrationName(nodeUUID, pvName)]; !exists {
				return true
			}
		}
	}
	return false
}

func (r *StorageNodeOpsReconciler) createMissingVolumeMigrationsOps(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	existingItems []simplyblockv1alpha1.VolumeMigration,
	existingVMNames map[string]struct{},
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	volumes, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list volumes for migration creation")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	sysFilter, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		return r.failOps(ctx, ops, "invalid systemVolumeFilterRegex: "+err.Error())
	}

	pvManaged, _, _, pvNameByVolumeUUID, pvcFetchFailed, err := matchVolumesToPVs(ctx, r.Client, volumes, sysFilter)
	if err != nil {
		log.Error(err, "drain: matchVolumesToPVs failed")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}
	if pvcFetchFailed {
		log.Info("drain: PVC fetch failed — retrying to avoid skipping volumes")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	if len(pvManaged) == 0 && len(existingItems) == 0 {
		return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseVerifying)
	}

	pvNames := make([]string, 0, len(pvManaged))
	for _, volUUID := range pvManaged {
		pvName, ok := pvNameByVolumeUUID[volUUID]
		if !ok {
			continue
		}
		if _, exists := existingVMNames[drainMigrationName(nodeUUID, pvName)]; !exists {
			pvNames = append(pvNames, pvName)
		}
	}
	if len(pvNames) == 0 {
		return ctrl.Result{RequeueAfter: drainRequeueMigrate}, nil
	}

	targetByPV, err := roundRobinTargetNodes(ctx, apiClient, clusterUUID, nodeUUID, pvNames)
	if err != nil {
		log.Error(err, "drain: no available target nodes for migration")
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "DrainNoMigrationTarget", "DrainNoMigrationTarget",
			"drain stalled: no online storage node available as migration target for node %s", nodeUUID)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "DrainNoMigrationTarget", fmt.Sprintf("drain stalled: no online storage node available as migration target for node %s", nodeUUID))
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	createdCount := 0
	for _, volUUID := range pvManaged {
		pvName, ok := pvNameByVolumeUUID[volUUID]
		if !ok {
			continue
		}
		migName := drainMigrationName(nodeUUID, pvName)
		if _, exists := existingVMNames[migName]; exists {
			continue
		}
		vmig := &simplyblockv1alpha1.VolumeMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      migName,
				Namespace: ops.Namespace,
				Labels:    map[string]string{"storage.simplyblock.io/drain-node": nodeUUID},
			},
			Spec: simplyblockv1alpha1.VolumeMigrationSpec{
				PVName:         pvName,
				TargetNodeUUID: targetByPV[pvName],
			},
		}
		if err := controllerutil.SetControllerReference(ops, vmig, r.Scheme); err != nil {
			log.Error(err, "drain: failed to set controller reference", "name", migName)
			continue
		}
		if err := r.Create(ctx, vmig); err != nil {
			log.Error(err, "drain: failed to create VolumeMigration", "name", migName)
			continue
		}
		createdCount++
	}

	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.VolumesPending = createdCount
	ops.Status.VolumesMigrated = 0
	ops.Status.Message = fmt.Sprintf("Migrating: 0 of %d volumes migrated", createdCount)
	_ = r.Status().Patch(ctx, ops, patch)
	return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
}

func (r *StorageNodeOpsReconciler) drainVerify(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	pools, volumes, err := fetchPoolVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list volumes during verification")
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	sysFilter, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		return r.failOps(ctx, ops, "invalid systemVolumeFilterRegex: "+err.Error())
	}

	var nonSystem, systemVols []string
	for _, vol := range volumes {
		if sysFilter.MatchString(vol.Name) {
			systemVols = append(systemVols, vol.UUID)
		} else {
			nonSystem = append(nonSystem, vol.UUID)
		}
	}

	if len(nonSystem) > 0 {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "DrainVerifyPending", "DrainVerifyPending",
			"node %s still has %d non-system volume(s) after migration; waiting for backend to confirm empty",
			nodeUUID, len(nonSystem))
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "DrainVerifyPending", fmt.Sprintf("node %s still has %d non-system volume(s) after migration; waiting for backend to confirm empty", nodeUUID, len(nonSystem)))
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	if len(systemVols) > 0 {
		poolByVol := make(map[string]string)
		for _, pool := range pools {
			vols, err := apiClient.GetPoolVolumes(ctx, clusterUUID, pool.UUID)
			if err != nil {
				continue
			}
			for _, v := range vols {
				poolByVol[v.UUID] = pool.UUID
			}
		}
		for _, volUUID := range systemVols {
			poolUUID, ok := poolByVol[volUUID]
			if !ok {
				continue
			}
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/",
				clusterUUID, poolUUID, volUUID)
			_, delStatus, delErr := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
			delClass := webapi.ClassifyError(delErr, delStatus)
			switch {
			case delErr == nil && (delStatus == http.StatusOK || delStatus == http.StatusNoContent || delStatus == http.StatusNotFound):
				log.Info("drain: deleted system volume", "volUUID", volUUID)
			case delClass.Retryable:
				log.Error(delErr, "drain: transient error deleting system volume, retrying", "volUUID", volUUID)
			default:
				return r.resumeAndFail(ctx, ops, sn, apiClient, clusterUUID,
					fmt.Sprintf("system volume %s delete rejected by backend (status %d)", volUUID, delStatus))
			}
		}
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving)
}

func (r *StorageNodeOpsReconciler) drainRemove(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s?force_remove=false",
		clusterUUID, nodeUUID)
	_, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)

	if err == nil && (status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound) {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "NodeRemoved", "NodeRemoved",
			"storage node %s removed successfully", nodeUUID)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "NodeRemoved", fmt.Sprintf("storage node %s removed successfully", nodeUUID))
		return r.succeedOps(ctx, ops, sn)
	}

	class := webapi.ClassifyError(err, status)
	if class.Retryable {
		log.Error(err, "drain: transient error on node DELETE, retrying", "status", status)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	return r.resumeAndFail(ctx, ops, sn, apiClient, clusterUUID,
		fmt.Sprintf("DELETE node returned status %d", status))
}

func (r *StorageNodeOpsReconciler) resumeAndFail(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	apiClient *webapi.Client,
	clusterUUID, reason string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID

	resumeEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/resume", clusterUUID, nodeUUID)
	_, resumeStatus, resumeErr := apiClient.Do(ctx, http.MethodPost, resumeEndpoint, nil)
	resumeClass := webapi.ClassifyError(resumeErr, resumeStatus)
	if resumeClass.Retryable {
		log.Error(resumeErr, "drain: transient error resuming node, will retry", "status", resumeStatus)
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Message = fmt.Sprintf("resume pending after failure: %s", reason)
		_ = r.Status().Patch(ctx, ops, patch)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "NodeResumed", "NodeResumed",
		"drain failed, attempted resume of node %s: %s", nodeUUID, reason)
	r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "NodeResumed", fmt.Sprintf("drain failed, attempted resume of node %s: %s", nodeUUID, reason))
	return r.failOps(ctx, ops, reason)
}

// clusterPauseCheck returns (requeue, true) if the cluster is not ready for drain operations.
func (r *StorageNodeOpsReconciler) clusterPauseCheck(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	_ *webapi.Client,
) (ctrl.Result, bool) {
	log := logf.FromContext(ctx)

	// Resolve the StorageNode to get the namespace and cluster name.
	var sn simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.StorageNodeRef, Namespace: ops.Namespace}, &sn); err != nil {
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, false
	}
	var sns simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{Name: sn.Spec.StorageNodeSetRef, Namespace: sn.Namespace}, &sns); err != nil {
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, false
	}

	clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, ops.Namespace, sns.Spec.ClusterName)
	if err != nil {
		log.Error(err, "drain: could not resolve cluster CR")
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, false
	}

	var reason string
	if clusterCR.Status.Status != "" && clusterCR.Status.Status != utils.ClusterStatusActive {
		reason = fmt.Sprintf("cluster status is %q (not active)", clusterCR.Status.Status)
	} else if clusterCR.Status.Rebalancing != nil && *clusterCR.Status.Rebalancing {
		reason = "cluster is rebalancing"
	}

	if reason == "" {
		return ctrl.Result{}, false
	}

	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Message = "drain paused: " + reason
	_ = r.Status().Patch(ctx, ops, patch)
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "DrainPaused", "DrainPaused",
		"drain paused: %s — will resume when cluster is active", reason)
	r.emitOnStorageNode(ctx, ops, corev1.EventTypeWarning, "DrainPaused", fmt.Sprintf("drain paused: %s — will resume when cluster is active", reason))
	log.Info("drain: pausing — cluster not ready", "reason", reason)
	return ctrl.Result{RequeueAfter: 60 * time.Second}, true
}

// advanceSubPhase patches ops.status.subPhase and requeues immediately.
func (r *StorageNodeOpsReconciler) advanceSubPhase(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	next simplyblockv1alpha1.StorageNodeOpsSubPhase,
) (ctrl.Result, error) {
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.SubPhase = next
	ops.Status.Triggered = false
	ops.Status.Message = fmt.Sprintf("entering phase %s", next)
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
}

// succeedOps marks the ops as Succeeded and releases the lock on the StorageNode.
func (r *StorageNodeOpsReconciler) succeedOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded
	ops.Status.SubPhase = ""
	ops.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.releaseLock(ctx, sn, ops.Name)
}

// failOps marks the ops as Failed with the given reason and releases the lock.
func (r *StorageNodeOpsReconciler) failOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	reason string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(nil, "ops failed", "ops", ops.Name, "reason", reason)
	r.Recorder.Eventf(ops, nil, "Warning", "OpsFailed", "OpsFailed", "%s", reason)
	r.emitOnStorageNode(ctx, ops, "Warning", "OpsFailed", reason)

	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseFailed
	ops.Status.SubPhase = ""
	ops.Status.Message = reason
	ops.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{}, err
	}

	var sn simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, types.NamespacedName{
		Name:      ops.Spec.StorageNodeRef,
		Namespace: ops.Namespace,
	}, &sn); err == nil {
		_ = r.releaseLock(ctx, &sn, ops.Name)
	}
	return ctrl.Result{}, nil
}

// emitOnStorageNode emits an event on the StorageNode that this ops targets,
// mirroring events that are also emitted on the StorageNodeOps CR itself.
func (r *StorageNodeOpsReconciler) emitOnStorageNode(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	eventType, reason, message string,
) {
	var sn simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.StorageNodeRef, Namespace: ops.Namespace}, &sn); err != nil {
		return
	}
	r.Recorder.Eventf(&sn, nil, eventType, reason, reason, "%s", message)
}

// releaseLock clears StorageNode.status.activeOpsRef if it still points to opsName.
func (r *StorageNodeOpsReconciler) releaseLock(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	opsName string,
) error {
	if sn.Status.ActiveOpsRef != opsName {
		return nil
	}
	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.ActiveOpsRef = ""
	return r.Status().Patch(ctx, sn, patch)
}

// resolveOpsSystemVolumeFilter compiles the system volume filter regex from the ops,
// falling back to the default pattern.
func (r *StorageNodeOpsReconciler) resolveOpsSystemVolumeFilter(
	ops *simplyblockv1alpha1.StorageNodeOps,
) (*regexp.Regexp, error) {
	pattern := simplyblockv1alpha1.DefaultSystemVolumeFilterRegex
	if ops.Spec.Drain != nil && ops.Spec.Drain.SystemVolumeFilterRegex != nil {
		pattern = *ops.Spec.Drain.SystemVolumeFilterRegex
	}
	return regexp.Compile(pattern)
}

// storageNodeToOpsRequests maps a StorageNode change to any pending
// StorageNodeOps that targets it, so ops waiting on lock acquisition requeue
// immediately when activeOpsRef is cleared rather than waiting for the poll timer.
func (r *StorageNodeOpsReconciler) storageNodeToOpsRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var opsList simplyblockv1alpha1.StorageNodeOpsList
	if err := r.List(ctx, &opsList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{"spec.storageNodeRef": obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(opsList.Items))
	for _, ops := range opsList.Items {
		if ops.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhasePending ||
			ops.Status.Phase == "" {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      ops.Name,
				Namespace: ops.Namespace,
			}})
		}
	}
	return reqs
}

// SetupWithManager registers the StorageNodeOpsReconciler with the controller manager.
func (r *StorageNodeOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index StorageNodeOps by their target StorageNode for efficient watch lookups.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.StorageNodeOps{},
		"spec.storageNodeRef",
		func(obj client.Object) []string {
			ops := obj.(*simplyblockv1alpha1.StorageNodeOps)
			return []string{ops.Spec.StorageNodeRef}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNodeOps{}).
		Named("storagenodeops").
		Watches(
			&simplyblockv1alpha1.StorageNode{},
			handler.EnqueueRequestsFromMapFunc(r.storageNodeToOpsRequests),
		).
		Owns(&simplyblockv1alpha1.VolumeMigration{}).
		Complete(r)
}
