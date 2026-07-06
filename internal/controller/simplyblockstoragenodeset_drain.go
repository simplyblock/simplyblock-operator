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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// Drain sub-phase names.
const (
	drainSubPhaseValidating = "Validating"
	drainSubPhaseSuspending = "Suspending"
	drainSubPhaseMigrating  = "Migrating"
	drainSubPhaseVerifying  = "Verifying"
	drainSubPhaseRemoving   = "Removing"
)

// Requeue intervals used by the drain state machine.
const (
	drainRequeueImmediate  = 1 * time.Second
	drainRequeueSuspend    = 10 * time.Second
	drainRequeueMigrate    = 15 * time.Second
	drainRequeueMigrateNew = 10 * time.Second
	drainRequeueVerify     = 30 * time.Second
	drainRequeueBlocking   = 60 * time.Second
	drainRequeueValidate   = 30 * time.Second
)

// performDrainAndRemove is the entry point for the drain-remove state machine.
// It is called from reconcileAction when action == "remove" and a drain flow is
// required (i.e. when the caller opts in by leaving the existing remove path and
// routing here). It dispatches to the appropriate sub-phase handler based on the
// current SubPhase stored in ActionStatus.
func (r *StorageNodeSetReconciler) performDrainAndRemove(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	// Skip immediately if this drain already reached a terminal state.
	// Without this guard, stale reconciles read SubPhase="" after success and
	// re-enter the initialization branch, restarting the drain on a node that
	// was already removed.
	if snCR.Status.ActionStatus != nil &&
		snCR.Status.ActionStatus.NodeUUID == snCR.Spec.NodeUUID &&
		(snCR.Status.ActionStatus.State == utils.ActionStateSuccess ||
			snCR.Status.ActionStatus.State == utils.ActionStateFailed) {
		return ctrl.Result{}, nil
	}

	// Determine current sub-phase.
	subPhase := ""
	if snCR.Status.ActionStatus != nil {
		subPhase = snCR.Status.ActionStatus.SubPhase
	}

	// If SubPhase is empty or "Validating", ensure ActionStatus is initialised
	// then delegate to drainValidate.
	if subPhase == "" || subPhase == drainSubPhaseValidating {
		if snCR.Status.ActionStatus == nil {
			snCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{}
		}
		as := snCR.Status.ActionStatus
		if as.State == "" || as.SubPhase == "" {
			as.Action = snCR.Spec.Action
			as.NodeUUID = snCR.Spec.NodeUUID
			as.State = utils.ActionStateRunning
			as.SubPhase = drainSubPhaseValidating
			as.UpdatedAt = metav1.Now()
			if err := r.Status().Update(ctx, snCR); err != nil {
				return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
			}
		}
		return r.drainValidate(ctx, apiClient, clusterUUID, snCR)
	}

	switch subPhase {
	case drainSubPhaseSuspending:
		return r.drainSuspend(ctx, apiClient, clusterUUID, snCR)
	case drainSubPhaseMigrating:
		return r.drainMigrate(ctx, apiClient, clusterUUID, snCR)
	case drainSubPhaseVerifying:
		return r.drainVerify(ctx, apiClient, clusterUUID, snCR)
	case drainSubPhaseRemoving:
		return r.drainRemove(ctx, apiClient, clusterUUID, snCR)
	default:
		return ctrl.Result{}, fmt.Errorf("drain: unknown SubPhase %q", subPhase)
	}
}

// drainHandleCancellation is called when the action field is cleared while a
// drain is in progress. It resumes the node so it returns to online, then
// clears ActionStatus.
func (r *StorageNodeSetReconciler) drainHandleCancellation(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if snCR.Status.ActionStatus == nil {
		return ctrl.Result{}, nil
	}

	// Re-fetch from the API server to guard against stale informer cache reads.
	// A stale reconcile may see Spec.Action="" while a concurrent one is actively
	// draining — a live read prevents a spurious resume from racing with suspend.
	fresh := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(snCR), fresh); err != nil {
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	if fresh.Spec.Action == utils.NodeActionRemove {
		// Drain is still active in the live CR — the cache was stale; skip cancellation.
		return ctrl.Result{}, nil
	}

	nodeUUID := snCR.Status.ActionStatus.NodeUUID
	subPhase := snCR.Status.ActionStatus.SubPhase

	// Only attempt resume if we reached or passed the Suspending phase AND the
	// node is actually suspended — skip if it's already online.
	if subPhase == drainSubPhaseSuspending ||
		subPhase == drainSubPhaseMigrating ||
		subPhase == drainSubPhaseVerifying ||
		subPhase == drainSubPhaseRemoving {
		currentStatus, err := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID)
		if err != nil {
			log.Error(err, "drain: cancellation could not read node status, skipping resume", "nodeUUID", nodeUUID)
		} else if currentStatus == utils.NodeStatusSuspended {
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/resume", clusterUUID, nodeUUID)
			if _, _, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil); err != nil {
				log.Error(err, "drain: cancellation resume failed (best effort)", "nodeUUID", nodeUUID)
			} else {
				r.Recorder.Eventf(snCR, corev1.EventTypeNormal, "NodeResumed",
					"drain cancelled: resumed node %s", nodeUUID)
			}
		} else {
			log.Info("drain: cancellation skipping resume, node already online", "nodeUUID", nodeUUID, "status", currentStatus)
		}
	}

	// Delete all owned VolumeMigration CRs so a subsequent drain starts fresh and
	// emits MigrationCreated events. Without this, reusing in-flight CRs silently
	// completes the migration with no user-visible indication the drain restarted.
	var vmigList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(ctx, &vmigList,
		client.InNamespace(snCR.Namespace),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": nodeUUID},
	); err != nil {
		log.Error(err, "drain: cancellation failed to list VolumeMigration CRs", "nodeUUID", nodeUUID)
	} else {
		for i := range vmigList.Items {
			vm := &vmigList.Items[i]
			if err := r.Delete(ctx, vm); err != nil {
				log.Error(err, "drain: cancellation failed to delete VolumeMigration", "name", vm.Name)
			}
		}
		if len(vmigList.Items) > 0 {
			log.Info("drain: cancelled — deleted in-flight VolumeMigration CRs",
				"nodeUUID", nodeUUID, "count", len(vmigList.Items))
		}
	}

	patch := client.MergeFrom(snCR.DeepCopy())
	snCR.Status.ActionStatus = nil
	if err := r.Status().Patch(ctx, snCR, patch); err != nil {
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	return ctrl.Result{}, nil
}

// drainValidate classifies volumes on the node into system, PV-managed, pinned,
// and unmanaged buckets and blocks the drain until all blocking volumes are gone.
func (r *StorageNodeSetReconciler) drainValidate(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	// Ensure ActionStatus is in running/Validating state.
	if snCR.Status.ActionStatus == nil {
		snCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:    snCR.Spec.Action,
			NodeUUID:  nodeUUID,
			State:     utils.ActionStateRunning,
			SubPhase:  drainSubPhaseValidating,
			UpdatedAt: metav1.Now(),
		}
	}

	// Block drain if the cluster is currently rebalancing — concurrent volume
	// movement would compete with the drain's VolumeMigration CRs.
	clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "drain: failed to resolve StorageCluster for rebalancing check")
		return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
	}
	if clusterCR.Status.Rebalancing != nil && *clusterCR.Status.Rebalancing {
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "ClusterRebalancing",
			"drain blocked: cluster is rebalancing, will retry when complete")
		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.Message = "drain blocked: cluster is currently rebalancing"
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch status (rebalancing blocked)")
		}
		return ctrl.Result{RequeueAfter: drainRequeueBlocking}, nil
	}

	volumes, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list node volumes", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
	}

	filterRegex := simplyblockv1alpha1.DefaultSystemVolumeFilterRegex
	if snCR.Spec.SystemVolumeFilterRegex != nil && *snCR.Spec.SystemVolumeFilterRegex != "" {
		filterRegex = *snCR.Spec.SystemVolumeFilterRegex
	}

	_, pinned, unmanaged, _, err := matchVolumesToPVs(ctx, r, volumes, filterRegex)
	if err != nil {
		log.Error(err, "drain: matchVolumesToPVs failed")
		return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
	}

	// Evaluate ALL blocking conditions before returning so the user sees every
	// issue at once — not one per reconcile iteration.
	var msgParts []string
	if len(pinned) > 0 {
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "PinnedVolumeBlocking",
			"drain blocked: pinned volumes %s must be unpinned before drain",
			strings.Join(pinned, ", "))
		msgParts = append(msgParts, fmt.Sprintf("%d pinned volume(s): %s", len(pinned), strings.Join(pinned, ", ")))
	}
	if len(unmanaged) > 0 {
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "UnmanagedVolumeBlocking",
			"drain blocked: unmanaged volumes %s must be removed before drain",
			strings.Join(unmanaged, ", "))
		msgParts = append(msgParts, fmt.Sprintf("%d unmanaged volume(s): %s", len(unmanaged), strings.Join(unmanaged, ", ")))
	}
	if len(msgParts) > 0 {
		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.Message = "drain blocked by " + strings.Join(msgParts, "; ")
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch status (blocking)")
		}
		return ctrl.Result{RequeueAfter: drainRequeueBlocking}, nil
	}

	// Advance to Suspending.
	if err := advanceDrainSubPhase(ctx, r, snCR, drainSubPhaseSuspending); err != nil {
		log.Error(err, "drain: failed to advance to Suspending")
		return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
	}
	return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
}

// drainSuspend POSTs to the suspend endpoint and polls until the node reports
// status "suspended", then advances to the Migrating sub-phase.
func (r *StorageNodeSetReconciler) drainSuspend(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	if !snCR.Status.ActionStatus.Triggered {
		// Check actual node status before POSTing — skip if already suspended.
		currentStatus, err := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID)
		if err != nil {
			log.Error(err, "drain: could not read node status before suspend, retrying", "nodeUUID", nodeUUID)
			return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
		}
		if currentStatus == utils.NodeStatusSuspended {
			log.Info("drain: node already suspended, advancing without POST", "nodeUUID", nodeUUID)
			patch := client.MergeFrom(snCR.DeepCopy())
			snCR.Status.ActionStatus.Triggered = true
			snCR.Status.ActionStatus.Message = "node already suspended"
			snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
			if err := r.Status().Patch(ctx, snCR, patch); err != nil {
				log.Error(err, "drain: failed to patch Triggered=true (already suspended)")
			}
			return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/suspend", clusterUUID, nodeUUID)
		_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("suspend API returned status %d", status)
			}
			log.Error(err, "drain: suspend POST failed", "nodeUUID", nodeUUID)
			return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
		}

		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.Triggered = true
		snCR.Status.ActionStatus.Message = "suspend request sent, waiting for node to suspend"
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch Triggered=true")
		}
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	// Poll node status.
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("node status GET returned status %d", status)
		}
		log.Error(err, "drain: failed to GET node status during suspend poll", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	var nodeResp utils.NodeStatusResponse
	if err := json.Unmarshal(body, &nodeResp); err != nil {
		log.Error(err, "drain: failed to unmarshal node status response")
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	if nodeResp.Status != utils.NodeStatusSuspended {
		log.Info("drain: node not yet suspended, requeuing",
			"nodeUUID", nodeUUID, "currentStatus", nodeResp.Status)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	// Node is suspended — advance to Migrating.
	if err := advanceDrainSubPhase(ctx, r, snCR, drainSubPhaseMigrating); err != nil {
		log.Error(err, "drain: failed to advance to Migrating")
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
}

// drainMigrate creates VolumeMigration CRs for all PV-managed volumes on the
// node and waits until they all complete.
func (r *StorageNodeSetReconciler) drainMigrate(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	// List all VolumeMigration CRs owned by this drain.
	var vmigList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(ctx, &vmigList,
		client.InNamespace(snCR.Namespace),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": nodeUUID},
	); err != nil {
		log.Error(err, "drain: failed to list VolumeMigration CRs")
		return ctrl.Result{RequeueAfter: drainRequeueMigrate}, nil
	}

	// If any migration failed, abort the drain.
	for i := range vmigList.Items {
		vm := &vmigList.Items[i]
		if vm.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed ||
			vm.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseAborted {
			reason := vm.Status.ErrorMessage
			if reason == "" {
				reason = fmt.Sprintf("VolumeMigration %s reached phase %s", vm.Name, vm.Status.Phase)
			}
			return r.resumeAndFail(ctx, apiClient, clusterUUID, snCR, reason)
		}
	}

	// Count completed vs in-progress migrations.
	completed := 0
	inProgress := 0
	for i := range vmigList.Items {
		if vmigList.Items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseCompleted {
			completed++
		} else {
			inProgress++
		}
	}

	// First time entering Migrating: no CRs exist yet — create them.
	if len(vmigList.Items) == 0 {
		volumes, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
		if err != nil {
			log.Error(err, "drain: failed to list volumes for migration creation")
			return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
		}

		filterRegex := simplyblockv1alpha1.DefaultSystemVolumeFilterRegex
		if snCR.Spec.SystemVolumeFilterRegex != nil && *snCR.Spec.SystemVolumeFilterRegex != "" {
			filterRegex = *snCR.Spec.SystemVolumeFilterRegex
		}

		pvManaged, _, _, pvNameByVolumeUUID, err := matchVolumesToPVs(ctx, r, volumes, filterRegex)
		if err != nil {
			log.Error(err, "drain: matchVolumesToPVs failed during migration creation")
			return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
		}

		if len(pvManaged) == 0 {
			// No PV-managed volumes — skip straight to Verifying.
			if err := advanceDrainSubPhase(ctx, r, snCR, drainSubPhaseVerifying); err != nil {
				log.Error(err, "drain: failed to advance to Verifying (no migrations needed)")
				return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
			}
			return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
		}

		// Build the PV name list for round-robin assignment.
		pvNames := make([]string, 0, len(pvManaged))
		for _, volUUID := range pvManaged {
			if pv, ok := pvNameByVolumeUUID[volUUID]; ok {
				pvNames = append(pvNames, pv)
			}
		}

		// Assign each PV a target node via round-robin across all online peers.
		targetByPV, err := roundRobinTargetNodes(ctx, apiClient, clusterUUID, nodeUUID, pvNames)
		if err != nil {
			log.Error(err, "drain: no available target nodes for migration")
			return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
		}

		createdCount := 0
		for _, volUUID := range pvManaged {
			pvName, ok := pvNameByVolumeUUID[volUUID]
			if !ok {
				continue
			}

			migName := drainMigrationName(nodeUUID, pvName)
			vmig := &simplyblockv1alpha1.VolumeMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      migName,
					Namespace: snCR.Namespace,
					Labels: map[string]string{
						"storage.simplyblock.io/drain-node": nodeUUID,
					},
				},
				Spec: simplyblockv1alpha1.VolumeMigrationSpec{
					PVName:         pvName,
					TargetNodeUUID: targetByPV[pvName],
				},
			}
			if err := controllerutil.SetControllerReference(snCR, vmig, r.Scheme); err != nil {
				log.Error(err, "drain: failed to set controller reference on VolumeMigration", "name", migName)
				continue
			}
			if err := r.Create(ctx, vmig); err != nil {
				log.Error(err, "drain: failed to create VolumeMigration", "name", migName)
				continue
			}
			r.Recorder.Eventf(snCR, corev1.EventTypeNormal, "MigrationCreated",
				"created VolumeMigration %s for PV %s", migName, pvName)
			createdCount++
		}

		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.VolumesPending = createdCount
		snCR.Status.ActionStatus.VolumesMigrated = 0
		snCR.Status.ActionStatus.Message = fmt.Sprintf("Migrating: 0 of %d volumes migrated", createdCount)
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch VolumesPending counter")
		}
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	// All existing CRs are completed — update counters, clean up and advance.
	if inProgress == 0 && completed == len(vmigList.Items) {
		// Write final counters before deleting the CRs so the status reflects
		// the true end state (all migrated, none pending).
		finalPatch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.VolumesMigrated = completed
		snCR.Status.ActionStatus.VolumesPending = 0
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, finalPatch); err != nil {
			log.Error(err, "drain: failed to patch final migration counters")
		}

		for i := range vmigList.Items {
			vm := &vmigList.Items[i]
			if err := r.Delete(ctx, vm); err != nil {
				log.Error(err, "drain: failed to delete completed VolumeMigration", "name", vm.Name)
			}
		}
		r.Recorder.Eventf(snCR, corev1.EventTypeNormal, "MigrationCompleted",
			"all %d volume migrations completed", completed)

		if err := advanceDrainSubPhase(ctx, r, snCR, drainSubPhaseVerifying); err != nil {
			log.Error(err, "drain: failed to advance to Verifying")
			return ctrl.Result{RequeueAfter: drainRequeueMigrate}, nil
		}
		return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
	}

	// Migrations still in progress — update counters.
	patch := client.MergeFrom(snCR.DeepCopy())
	total := len(vmigList.Items)
	snCR.Status.ActionStatus.VolumesMigrated = completed
	snCR.Status.ActionStatus.VolumesPending = inProgress
	snCR.Status.ActionStatus.Message = fmt.Sprintf("Migrating: %d of %d volumes migrated", completed, total)
	snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
	if err := r.Status().Patch(ctx, snCR, patch); err != nil {
		log.Error(err, "drain: failed to patch migration progress counters")
	}
	return ctrl.Result{RequeueAfter: drainRequeueMigrate}, nil
}

// drainVerify checks that the node holds no non-system volumes before removing it.
func (r *StorageNodeSetReconciler) drainVerify(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	volumes, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list volumes during verification", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	filterRegex := simplyblockv1alpha1.DefaultSystemVolumeFilterRegex
	if snCR.Spec.SystemVolumeFilterRegex != nil && *snCR.Spec.SystemVolumeFilterRegex != "" {
		filterRegex = *snCR.Spec.SystemVolumeFilterRegex
	}

	re, err := regexp.Compile(filterRegex)
	if err != nil {
		log.Error(err, "drain: invalid SystemVolumeFilterRegex, using empty filter")
		re = regexp.MustCompile("^$") // match nothing
	}

	var nonSystem []string
	for _, vol := range volumes {
		if !re.MatchString(vol.Name) {
			nonSystem = append(nonSystem, vol.UUID)
		}
	}

	if len(nonSystem) > 0 {
		log.Info("drain: node still has non-system volumes, requeuing",
			"nodeUUID", nodeUUID, "volumeUUIDs", strings.Join(nonSystem, ", "))
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	// All remaining volumes are system volumes (or node is empty).
	if err := advanceDrainSubPhase(ctx, r, snCR, drainSubPhaseRemoving); err != nil {
		log.Error(err, "drain: failed to advance to Removing")
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}
	return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
}

// drainRemove sends the DELETE request to remove the storage node from the cluster.
func (r *StorageNodeSetReconciler) drainRemove(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-nodes/%s?force_remove=false",
		clusterUUID,
		nodeUUID,
	)

	_, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		log.Error(err, "drain: DELETE node request failed, retrying", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		r.Recorder.Eventf(snCR, corev1.EventTypeNormal, "NodeRemoved",
			"storage node %s removed successfully", nodeUUID)
		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.State = utils.ActionStateSuccess
		snCR.Status.ActionStatus.SubPhase = ""
		snCR.Status.ActionStatus.VolumesPending = 0
		snCR.Status.ActionStatus.Message = "node removed successfully"
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch success status after remove")
		}
		return ctrl.Result{}, nil
	}

	// 5xx: transient backend error — retry without resuming the node.
	if status >= http.StatusInternalServerError {
		log.Error(nil, "drain: DELETE node returned 5xx, retrying",
			"nodeUUID", nodeUUID, "status", status)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	// 4xx (other than 404): permanent backend rejection — resume and fail.
	return r.resumeAndFail(ctx, apiClient, clusterUUID, snCR,
		fmt.Sprintf("DELETE node returned unexpected status %d", status))
}

// resumeAndFail attempts a best-effort resume of the suspended node, then marks
// the action as failed.
func (r *StorageNodeSetReconciler) resumeAndFail(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	reason string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	resumeEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/resume", clusterUUID, nodeUUID)
	if _, _, err := apiClient.Do(ctx, http.MethodPost, resumeEndpoint, nil); err != nil {
		log.Error(err, "drain: best-effort resume failed", "nodeUUID", nodeUUID)
	}

	r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "NodeResumed",
		"drain failed, attempted resume of node %s: %s", nodeUUID, reason)

	patch := client.MergeFrom(snCR.DeepCopy())
	snCR.Status.ActionStatus.State = utils.ActionStateFailed
	snCR.Status.ActionStatus.SubPhase = ""
	snCR.Status.ActionStatus.Message = reason
	snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
	if err := r.Status().Patch(ctx, snCR, patch); err != nil {
		log.Error(err, "drain: failed to patch failed status")
	}
	return ctrl.Result{}, nil
}

// advanceDrainSubPhase patches only the SubPhase and Message fields of the
// ActionStatus.
func advanceDrainSubPhase(
	ctx context.Context,
	r *StorageNodeSetReconciler,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	newPhase string,
) error {
	patch := client.MergeFrom(snCR.DeepCopy())
	snCR.Status.ActionStatus.SubPhase = newPhase
	snCR.Status.ActionStatus.Message = fmt.Sprintf("drain: entering phase %s", newPhase)
	snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
	return r.Status().Patch(ctx, snCR, patch)
}

// listNodeVolumes returns all volumes whose primary node is nodeUUID by listing
// every pool in the cluster and filtering by VolumeInfo.PrimaryNodeUUID.
func listNodeVolumes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
) ([]webapi.VolumeInfo, error) {
	pools, err := apiClient.GetStoragePools(ctx, clusterUUID)
	if err != nil {
		return nil, fmt.Errorf("listNodeVolumes: %w", err)
	}

	var result []webapi.VolumeInfo
	for _, pool := range pools {
		vols, err := apiClient.GetPoolVolumes(ctx, clusterUUID, pool.UUID)
		if err != nil {
			return nil, fmt.Errorf("listNodeVolumes: pool %s: %w", pool.UUID, err)
		}
		for _, v := range vols {
			if v.PrimaryNodeUUID == nodeUUID {
				result = append(result, v)
			}
		}
	}
	return result, nil
}

// matchVolumesToPVs classifies each backend volume into one of three buckets:
//   - pvManaged: volume UUID has a corresponding simplyblock PV, not pinned
//   - pinned: volume UUID has a PV but the PVC has the pinned-volume annotation
//   - unmanaged: volume UUID has no corresponding PV (and is not a system volume)
//
// System volumes (those matching filterRegex by name) are skipped entirely.
// pvNameByVolumeUUID maps volume UUID → PV name for the pvManaged and pinned buckets.
func matchVolumesToPVs(
	ctx context.Context,
	r *StorageNodeSetReconciler,
	volumes []webapi.VolumeInfo,
	filterRegex string,
) (pvManaged, pinned, unmanaged []string, pvNameByVolumeUUID map[string]string, err error) {
	log := logf.FromContext(ctx)

	pvNameByVolumeUUID = make(map[string]string)

	// Build a map: volumeUUID → pvName for all simplyblock PVs.
	var pvList corev1.PersistentVolumeList
	if err = r.List(ctx, &pvList); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("matchVolumesToPVs: list PVs: %w", err)
	}

	// pvByVolumeUUID maps volume UUID → PV object for simplyblock PVs.
	pvByVolumeUUID := make(map[string]*corev1.PersistentVolume, len(pvList.Items))
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != utils.CSIProvisioner {
			continue
		}
		// VolumeHandle format: clusterUUID:poolUUID:volumeUUID — extract last segment.
		volHandle := pv.Spec.CSI.VolumeHandle
		if volHandle != "" {
			parts := strings.SplitN(volHandle, ":", 3)
			volumeUUID := parts[len(parts)-1]
			if volumeUUID != "" {
				pvByVolumeUUID[volumeUUID] = pv
			}
		}
	}

	re, reErr := regexp.Compile(filterRegex)
	if reErr != nil {
		log.Error(reErr, "matchVolumesToPVs: invalid filterRegex, treating all volumes as non-system", "regex", filterRegex)
		re = regexp.MustCompile("^$") // match nothing — no volumes will be treated as system
	}

	for _, vol := range volumes {
		// System volume: skip entirely.
		if re.MatchString(vol.Name) {
			continue
		}

		pv, isManagedByCSI := pvByVolumeUUID[vol.UUID]
		if !isManagedByCSI {
			unmanaged = append(unmanaged, vol.UUID)
			continue
		}

		// PV exists — check if the PVC has the pinned annotation.
		if pv.Spec.ClaimRef == nil {
			// PV has no claim; treat as PV-managed (not pinned).
			pvManaged = append(pvManaged, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
			continue
		}

		var pvc corev1.PersistentVolumeClaim
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: pv.Spec.ClaimRef.Namespace,
			Name:      pv.Spec.ClaimRef.Name,
		}, &pvc); err != nil {
			log.Error(err, "matchVolumesToPVs: failed to get PVC, treating volume as unmanaged",
				"pvc", pv.Spec.ClaimRef.Name, "namespace", pv.Spec.ClaimRef.Namespace)
			unmanaged = append(unmanaged, vol.UUID)
			continue
		}

		if _, isPinned := pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolume]; isPinned {
			pinned = append(pinned, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
		} else {
			pvManaged = append(pvManaged, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
		}
	}

	return pvManaged, pinned, unmanaged, pvNameByVolumeUUID, nil
}

// getNodeBackendStatus fetches the current status string of a single storage
// node directly from the backend API.
func getNodeBackendStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, nodeUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("getNodeBackendStatus: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("getNodeBackendStatus: status %d", status)
	}
	var resp utils.NodeStatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("getNodeBackendStatus: unmarshal: %w", err)
	}
	return resp.Status, nil
}

// roundRobinTargetNodes lists all online nodes (excluding the drained node) and
// assigns each PV name a target node UUID using round-robin order. The i-th PV
// in pvNames is assigned to onlineNodes[i % len(onlineNodes)], distributing
// migrations evenly across the cluster without requiring persistent state.
// Returns an error if no online peer node is available.
func roundRobinTargetNodes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	excludeNodeUUID string,
	pvNames []string,
) (map[string]string, error) {
	nodes, err := apiClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		return nil, fmt.Errorf("roundRobinTargetNodes: %w", err)
	}

	var online []string
	for _, n := range nodes {
		if n.UUID != excludeNodeUUID && n.Status == utils.NodeStatusOnline {
			online = append(online, n.UUID)
		}
	}
	if len(online) == 0 {
		return nil, fmt.Errorf("roundRobinTargetNodes: no online node available other than %s", excludeNodeUUID)
	}

	assignment := make(map[string]string, len(pvNames))
	for i, pv := range pvNames {
		assignment[pv] = online[i%len(online)]
	}
	return assignment, nil
}

// drainMigrationName builds a DNS-label-safe name for a VolumeMigration CR.
func drainMigrationName(nodeUUID, pvName string) string {
	prefix := "drain-"
	if len(nodeUUID) >= 8 {
		prefix += nodeUUID[:8] + "-"
	}
	name := prefix + pvName
	name = strings.ToLower(name)
	// Replace invalid chars with '-'.
	var result []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else {
			result = append(result, '-')
		}
	}
	s := strings.Trim(string(result), "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}
