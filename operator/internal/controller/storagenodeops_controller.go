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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch

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
	if ops.Spec.Action == "remove" {
		if res, paused := r.clusterPauseCheck(ctx, &ops, clusterUUID, apiClient); paused {
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
	if ops.Spec.Action == "remove" {
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
	case "remove":
		return r.runDrain(ctx, ops, sn, clusterUUID, apiClient)
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
		"resume":   "online",
		"restart":  "online",
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
		r.Recorder.Eventf(ops, corev1.EventTypeWarning, "PinnedVolumeBlocking",
			"drain blocked: %d pinned volume(s) on node %s — remove the %s annotation to proceed",
			len(pinned), nodeUUID, simplyblockv1alpha1.AnnotationPinnedVolume)
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Message = fmt.Sprintf("blocked: %d pinned volume(s) — remove simplyblock.io/pinned-volume annotation", len(pinned))
		_ = r.Status().Patch(ctx, ops, patch)
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	if len(unmanaged) > 0 {
		r.Recorder.Eventf(ops, corev1.EventTypeWarning, "UnmanagedVolumeBlocking",
			"drain blocked: %d unmanaged volume(s) on node %s — remove them manually",
			len(unmanaged), nodeUUID)
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
		r.Recorder.Eventf(ops, corev1.EventTypeWarning, "DrainSuspendPending",
			"waiting for node %s to suspend (current status: %s)", nodeUUID, nodeResp.Status)
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
	if res, handled := r.handleFailedVolumeMigrations(ctx, ops, sn, clusterUUID, apiClient, vmigList.Items); handled {
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
		r.Recorder.Eventf(ops, corev1.EventTypeNormal, "MigrationCompleted",
			"all %d volume migrations completed", completed)
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
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
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
	if res, paused := r.clusterPauseCheck(ctx, ops, clusterUUID, apiClient); paused {
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
		r.Recorder.Eventf(ops, corev1.EventTypeWarning, "MigrationRetry",
			"VolumeMigration %s failed, deleted and will retry with new target", vm.Name)
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
		r.Recorder.Eventf(ops, corev1.EventTypeWarning, "DrainNoMigrationTarget",
			"drain stalled: no online storage node available as migration target for node %s", nodeUUID)
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
		r.Recorder.Eventf(ops, corev1.EventTypeWarning, "DrainVerifyPending",
			"node %s still has %d non-system volume(s) after migration; waiting for backend to confirm empty",
			nodeUUID, len(nonSystem))
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
		r.Recorder.Eventf(ops, corev1.EventTypeNormal, "NodeRemoved",
			"storage node %s removed successfully", nodeUUID)
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
	r.Recorder.Eventf(ops, corev1.EventTypeWarning, "NodeResumed",
		"drain failed, attempted resume of node %s: %s", nodeUUID, reason)
	return r.failOps(ctx, ops, reason)
}

// clusterPauseCheck returns (requeue, true) if the cluster is not ready for drain operations.
func (r *StorageNodeOpsReconciler) clusterPauseCheck(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	clusterUUID string,
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
	r.Recorder.Eventf(ops, corev1.EventTypeWarning, "DrainPaused",
		"drain paused: %s — will resume when cluster is active", reason)
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
	r.Recorder.Event(ops, "Warning", "OpsFailed", reason)

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
