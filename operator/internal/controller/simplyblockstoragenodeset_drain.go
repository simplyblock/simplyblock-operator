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

// defaultSystemVolumeFilter is compiled once at package init from the
// well-known default pattern. A MustCompile panics at startup if the constant
// is malformed — intentional fast-fail for a hardcoded value.
var defaultSystemVolumeFilter = regexp.MustCompile(simplyblockv1alpha1.DefaultSystemVolumeFilterRegex)

// resolveSystemVolumeFilter returns the compiled regex for the CR's system
// volume filter. The default is a package-level constant; custom patterns are
// compiled on first use and cached in the reconciler. If a custom pattern
// fails to compile, an error is returned so the reconciler can surface it to
// the user before touching any volumes.
func resolveSystemVolumeFilter(
	snCR *simplyblockv1alpha1.StorageNodeSet,
	r *StorageNodeSetReconciler,
) (*regexp.Regexp, error) {
	if snCR.Spec.SystemVolumeFilterRegex == nil || *snCR.Spec.SystemVolumeFilterRegex == "" {
		return defaultSystemVolumeFilter, nil
	}
	pattern := *snCR.Spec.SystemVolumeFilterRegex
	if cached, ok := r.systemVolumeFilterCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid systemVolumeFilterRegex %q: %w", pattern, err)
	}
	r.systemVolumeFilterCache.Store(pattern, re)
	return re, nil
}

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

	// For all active sub-phases (Suspending onward), pause drain if the cluster
	// is no longer active or is currently rebalancing. Resume automatically
	// on the next reconcile once the cluster is active and not rebalancing.
	if paused, reason := r.drainClusterPauseCheck(ctx, snCR); paused {
		log := logf.FromContext(ctx)
		log.Info("drain: pausing — cluster not ready",
			"subPhase", subPhase, "reason", reason)
		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.Message = "drain paused: " + reason
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch paused message")
		}
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "DrainPaused",
			"drain paused at %s: %s", subPhase, reason)
		return ctrl.Result{RequeueAfter: drainRequeueBlocking}, nil
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

// drainClusterPauseCheck returns (true, reason) when the cluster is not ready
// for drain progression — either because it is not active or because it is
// currently rebalancing. The drain resumes automatically on the next poll once
// both conditions are cleared. Returns (false, "") when the cluster is ready.
func (r *StorageNodeSetReconciler) drainClusterPauseCheck(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (paused bool, reason string) {
	clusterCR, err := utils.ResolveClusterCR(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		// Can't determine cluster status — don't block, let the sub-phase handle it.
		return false, ""
	}

	if clusterCR.Status.Status != "" && clusterCR.Status.Status != utils.ClusterStatusActive {
		return true, fmt.Sprintf("cluster status is %q (not active)", clusterCR.Status.Status)
	}

	if clusterCR.Status.Rebalancing != nil && *clusterCR.Status.Rebalancing {
		return true, "cluster is rebalancing"
	}

	return false, ""
}

// handleFailedVolumeMigrations checks whether any VolumeMigration CR has reached
// a terminal failure state. If the cluster is not ready it treats the failure as
// a transient pause; otherwise it calls resumeAndFail. Returns (result, true)
// when it has handled the situation and the caller should return immediately.
func (r *StorageNodeSetReconciler) handleFailedVolumeMigrations(
	ctx context.Context,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	items []simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, bool) {
	log := logf.FromContext(ctx)

	for i := range items {
		vm := &items[i]
		if vm.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed &&
			vm.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseAborted {
			continue
		}

		// A suspended/degraded cluster causes ContinueMigration to return 400,
		// making the VolumeMigrationReconciler mark the CR as Failed. Treat this
		// as a transient pause so the drain resumes once the cluster is active.
		if paused, reason := r.drainClusterPauseCheck(ctx, snCR); paused {
			log.Info("drain: VolumeMigration failed but cluster is not ready — pausing and deleting failed CR so it is recreated on resume",
				"vm", vm.Name, "vmPhase", vm.Status.Phase, "clusterReason", reason)
			// Delete ALL Failed/Aborted VMs now so that when the cluster
			// recovers drainMigrate will recreate them with fresh target
			// assignments. Leaving failed CRs in place would cause
			// resumeAndFail to be called as soon as the cluster is active.
			for i := range items {
				if items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed ||
					items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseAborted {
					if err := r.Delete(ctx, &items[i]); err != nil {
						log.Error(err, "drain: failed to delete failed VolumeMigration during pause",
							"vm", items[i].Name)
					}
				}
			}
			patch := client.MergeFrom(snCR.DeepCopy())
			snCR.Status.ActionStatus.Message = "drain paused (migration failed due to cluster state): " + reason
			snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
			if err := r.Status().Patch(ctx, snCR, patch); err != nil {
				log.Error(err, "drain: failed to patch paused message")
			}
			r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "DrainPaused",
				"drain paused at Migrating: VolumeMigration %s failed because cluster is not ready (%s); failed CRs deleted — will recreate when cluster is active",
				vm.Name, reason)
			return ctrl.Result{RequeueAfter: drainRequeueBlocking}, true
		}

		// Delete the Failed VM and let createMissingVolumeMigrations recreate it
		// with a fresh roundRobinTargetNodes assignment. The node remains suspended
		// while we retry — resumeAndFail is not called here so the drain continues.
		// If no target is available the DrainNoMigrationTarget event surfaces the stall.
		reason := vm.Status.ErrorMessage
		if reason == "" {
			reason = fmt.Sprintf("VolumeMigration %s reached phase %s", vm.Name, vm.Status.Phase)
		}
		log.Info("drain: VolumeMigration failed — deleting and retrying with fresh target assignment",
			"vm", vm.Name, "reason", reason)
		for i := range items {
			if items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed ||
				items[i].Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseAborted {
				if err := r.Delete(ctx, &items[i]); err != nil {
					log.Error(err, "drain: failed to delete Failed VolumeMigration for retry", "vm", items[i].Name)
				}
			}
		}
		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.Message = "migration failed, retrying with new target: " + reason
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch retry message")
		}
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "MigrationRetry",
			"VolumeMigration %s failed (%s); deleted — will recreate with a different target node",
			vm.Name, reason)
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, true
	}
	return ctrl.Result{}, false
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

	sysFilter, err := resolveSystemVolumeFilter(snCR, r)
	if err != nil {
		log.Error(err, "drain: invalid systemVolumeFilterRegex")
		return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
	}

	_, pinned, unmanaged, pvNameByVolume, pinnedTargets, _, err := matchVolumesToPVs(ctx, r, volumes, sysFilter)
	if err != nil {
		log.Error(err, "drain: matchVolumesToPVs failed")
		return ctrl.Result{RequeueAfter: drainRequeueValidate}, nil
	}

	// Split pinned volumes: those with a valid target UUID (can migrate) vs those
	// that block drain because the annotation is empty, not a UUID, or self-referencing.
	var pinnedBlocked []string
	for _, volUUID := range pinned {
		target := pinnedTargets[volUUID]
		if isValidUUID(target) && target != nodeUUID {
			// Has a routable target — will be handled by createMissingVolumeMigrations.
			continue
		}
		pvName := pvNameByVolume[volUUID]
		if target == "" {
			r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "PinnedVolumeBlocking",
				"drain blocked: PV %s is pinned with no target node; set annotation %s to a valid storage-node UUID or remove it",
				pvName, simplyblockv1alpha1.AnnotationPinnedVolume)
		} else if target == nodeUUID {
			r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "PinnedVolumeBlocking",
				"drain blocked: PV %s is pinned to the node being drained (%s); re-pin to a different storage-node UUID",
				pvName, nodeUUID)
		} else {
			r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "PinnedVolumeBlocking",
				"drain blocked: PV %s has invalid pin target %q; annotation %s must be a valid storage-node UUID",
				pvName, target, simplyblockv1alpha1.AnnotationPinnedVolume)
		}
		pinnedBlocked = append(pinnedBlocked, pvName)
	}

	// Evaluate ALL blocking conditions before returning so the user sees every
	// issue at once — not one per reconcile iteration.
	var msgParts []string
	if len(pinnedBlocked) > 0 {
		msgParts = append(msgParts, fmt.Sprintf("%d pinned volume(s) with no valid target: %s", len(pinnedBlocked), strings.Join(pinnedBlocked, ", ")))
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
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "DrainSuspendPending",
			"waiting for node %s to suspend (current status: %s)", nodeUUID, nodeResp.Status)
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

	// If any migration failed, handle it — pausing if the cluster is the cause.
	if res, handled := r.handleFailedVolumeMigrations(ctx, snCR, vmigList.Items); handled {
		return res, nil
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

	// Build the set of existing VM names so we can detect missing ones.
	// On first entry len == 0; after a cluster-state pause some Failed CRs were
	// deleted by handleFailedVolumeMigrations and need to be recreated here.
	existingVMNames := make(map[string]struct{}, len(vmigList.Items))
	for i := range vmigList.Items {
		existingVMNames[vmigList.Items[i].Name] = struct{}{}
	}

	// Create VMs for any PV that does not already have one. Only enters when
	// there are missing VMs (first entry or resume-after-pause); otherwise the
	// completion check below handles the rest.
	if len(vmigList.Items) == 0 || r.hasMissingVolumeMigrations(ctx, apiClient, clusterUUID, nodeUUID, snCR, existingVMNames) {
		return r.createMissingVolumeMigrations(ctx, apiClient, clusterUUID, snCR, vmigList.Items, existingVMNames)
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

// hasMissingVolumeMigrations returns true if any PV-managed volume on the node
// does not yet have a corresponding VolumeMigration CR in existingVMNames.
// Used to detect the resume-after-pause case where failed CRs were deleted.
func (r *StorageNodeSetReconciler) hasMissingVolumeMigrations(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, nodeUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	existingVMNames map[string]struct{},
) bool {
	vols, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		return false
	}
	sf, err := resolveSystemVolumeFilter(snCR, r)
	if err != nil {
		return false
	}
	pvm, pinnedVols, _, pvByVol, pinnedTargets, _, err := matchVolumesToPVs(ctx, r, vols, sf)
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
	for _, volUUID := range pinnedVols {
		target := pinnedTargets[volUUID]
		if !isValidUUID(target) || target == nodeUUID {
			continue
		}
		if pvName, ok := pvByVol[volUUID]; ok {
			if _, exists := existingVMNames[drainMigrationName(nodeUUID, pvName)]; !exists {
				return true
			}
		}
	}
	return false
}

// createMissingVolumeMigrations lists PV-managed volumes on the node, skips any
// that already have a VolumeMigration CR in existingVMNames, and creates new CRs
// for the rest using round-robin target assignment.
func (r *StorageNodeSetReconciler) createMissingVolumeMigrations(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
	existingItems []simplyblockv1alpha1.VolumeMigration,
	existingVMNames map[string]struct{},
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	volumes, err := listNodeVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list volumes for migration creation")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	sysFilter, err := resolveSystemVolumeFilter(snCR, r)
	if err != nil {
		log.Error(err, "drain: invalid systemVolumeFilterRegex")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	pvManaged, pinnedVols, _, pvNameByVolumeUUID, pinnedTargetsInMigrate, pvcFetchFailed, err := matchVolumesToPVs(ctx, r, volumes, sysFilter)
	if err != nil {
		log.Error(err, "drain: matchVolumesToPVs failed during migration creation")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}
	// A PV-backed volume appeared as unmanaged because its PVC GET failed
	// transiently. Retrying avoids silently skipping the volume, which would
	// leave it on the node and stall Verifying indefinitely.
	if pvcFetchFailed {
		log.Info("drain: PVC fetch failed for a PV-backed volume — retrying to avoid skipping it")
		return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
	}

	// Collect pinned volumes that have a valid target different from the draining node.
	var pinnedWithTarget []string
	for _, volUUID := range pinnedVols {
		target := pinnedTargetsInMigrate[volUUID]
		if isValidUUID(target) && target != nodeUUID {
			pinnedWithTarget = append(pinnedWithTarget, volUUID)
		}
	}

	if len(pvManaged) == 0 && len(pinnedWithTarget) == 0 && len(existingItems) == 0 {
		if err := advanceDrainSubPhase(ctx, r, snCR, drainSubPhaseVerifying); err != nil {
			log.Error(err, "drain: failed to advance to Verifying (no migrations needed)")
			return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
		}
		return ctrl.Result{RequeueAfter: drainRequeueImmediate}, nil
	}

	// Determine which PV-managed volumes still need a VolumeMigration CR.
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

	// Assign round-robin targets only for the PV-managed (non-pinned) volumes.
	targetByPV := map[string]string{}
	if len(pvNames) > 0 {
		targetByPV, err = roundRobinTargetNodes(ctx, apiClient, clusterUUID, nodeUUID, pvNames)
		if err != nil {
			log.Error(err, "drain: no available target nodes for migration")
			r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "DrainNoMigrationTarget",
				"drain stalled: no online storage node available as migration target for node %s — will retry when a peer node is online",
				nodeUUID)
			return ctrl.Result{RequeueAfter: drainRequeueMigrateNew}, nil
		}
	}

	createdCount := 0
	createVM := func(pvName, targetNodeUUID string) {
		migName := drainMigrationName(nodeUUID, pvName)
		if _, exists := existingVMNames[migName]; exists {
			return
		}
		vmig := &simplyblockv1alpha1.VolumeMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      migName,
				Namespace: snCR.Namespace,
				Labels:    map[string]string{"storage.simplyblock.io/drain-node": nodeUUID},
			},
			Spec: simplyblockv1alpha1.VolumeMigrationSpec{
				PVName:         pvName,
				TargetNodeUUID: targetNodeUUID,
			},
		}
		if err := controllerutil.SetControllerReference(snCR, vmig, r.Scheme); err != nil {
			log.Error(err, "drain: failed to set controller reference on VolumeMigration", "name", migName)
			return
		}
		if err := r.Create(ctx, vmig); err != nil {
			log.Error(err, "drain: failed to create VolumeMigration", "name", migName)
			return
		}
		r.Recorder.Eventf(snCR, corev1.EventTypeNormal, "MigrationCreated",
			"created VolumeMigration %s for PV %s → node %s", migName, pvName, targetNodeUUID)
		createdCount++
	}

	// Create VMs for PV-managed volumes using round-robin targets.
	for _, volUUID := range pvManaged {
		pvName, ok := pvNameByVolumeUUID[volUUID]
		if !ok {
			continue
		}
		createVM(pvName, targetByPV[pvName])
	}

	// Create VMs for pinned volumes using their annotation-specified target.
	for _, volUUID := range pinnedWithTarget {
		pvName, ok := pvNameByVolumeUUID[volUUID]
		if !ok {
			continue
		}
		createVM(pvName, pinnedTargetsInMigrate[volUUID])
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

// drainVerify checks that the node holds no non-system volumes before removing it.
func (r *StorageNodeSetReconciler) drainVerify(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	snCR *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := snCR.Spec.NodeUUID

	// Fetch pools and node volumes in a single pass — drainVerify may also need
	// the pool list for system volume cleanup, so we reuse it rather than
	// making a second GetStoragePools call.
	pools, volumes, err := fetchPoolVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil {
		log.Error(err, "drain: failed to list volumes during verification", "nodeUUID", nodeUUID)
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	sysFilter, err := resolveSystemVolumeFilter(snCR, r)
	if err != nil {
		log.Error(err, "drain: invalid systemVolumeFilterRegex")
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
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
		log.Info("drain: node still has non-system volumes, requeuing",
			"nodeUUID", nodeUUID, "volumeUUIDs", strings.Join(nonSystem, ", "))
		r.Recorder.Eventf(snCR, corev1.EventTypeWarning, "DrainVerifyPending",
			"node %s still has %d non-system volume(s) after migration (%s); waiting for backend to confirm empty",
			nodeUUID, len(nonSystem), strings.Join(nonSystem, ", "))
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	// If system/benchmark volumes remain, delete them now. The backend rejects
	// node removal if any LVols are still present, even system ones. Requeue
	// after issuing the deletes so the next reconcile confirms they are gone
	// before advancing to Removing.
	if len(systemVols) > 0 {
		// Build pool lookup from the pools already fetched above — no extra API call.
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
				log.Info("drain: system volume pool not found, skipping", "volUUID", volUUID)
				continue
			}
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/",
				clusterUUID, poolUUID, volUUID)
			_, delStatus, delErr := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
			delClass := webapi.ClassifyError(delErr, delStatus)
			switch {
			case delErr == nil && (delStatus == http.StatusOK || delStatus == http.StatusNoContent || delStatus == http.StatusNotFound):
				log.Info("drain: deleted system volume", "volUUID", volUUID, "status", delStatus)
			case delClass.Retryable:
				log.Error(delErr, "drain: transient error deleting system volume, will retry on next reconcile",
					"volUUID", volUUID, "status", delStatus)
			default:
				// Permanent rejection — resume and fail the drain.
				return r.resumeAndFail(ctx, apiClient, clusterUUID, snCR,
					fmt.Sprintf("system volume %s delete rejected by backend (status %d)", volUUID, delStatus))
			}
		}
		// Requeue to confirm the volumes are gone before advancing to Removing.
		return ctrl.Result{RequeueAfter: drainRequeueVerify}, nil
	}

	// Node is empty — advance to Removing.
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

	// 200, 204: success. 404: node already gone — treat as success.
	if err == nil && (status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound) {
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

	class := webapi.ClassifyError(err, status)
	if class.Retryable {
		log.Error(err, "drain: transient error on node DELETE, retrying",
			"nodeUUID", nodeUUID, "status", status)
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}

	// Permanent error — resume the node and mark failed.
	return r.resumeAndFail(ctx, apiClient, clusterUUID, snCR,
		fmt.Sprintf("DELETE node returned status %d", status))
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
	_, resumeStatus, resumeErr := apiClient.Do(ctx, http.MethodPost, resumeEndpoint, nil)
	resumeClass := webapi.ClassifyError(resumeErr, resumeStatus)
	if resumeClass.Retryable {
		// Transient resume failure — requeue so the node gets another resume
		// attempt rather than being left suspended indefinitely.
		log.Error(resumeErr, "drain: transient error resuming node, will retry",
			"nodeUUID", nodeUUID, "status", resumeStatus)
		patch := client.MergeFrom(snCR.DeepCopy())
		snCR.Status.ActionStatus.Message = fmt.Sprintf("resume pending after failure: %s", reason)
		snCR.Status.ActionStatus.UpdatedAt = metav1.Now()
		if err := r.Status().Patch(ctx, snCR, patch); err != nil {
			log.Error(err, "drain: failed to patch status during resume retry")
		}
		return ctrl.Result{RequeueAfter: drainRequeueSuspend}, nil
	}
	if resumeErr != nil || (resumeStatus >= http.StatusBadRequest && resumeStatus != http.StatusNotFound) {
		log.Error(resumeErr, "drain: permanent error resuming node — node may remain suspended",
			"nodeUUID", nodeUUID, "status", resumeStatus)
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

// fetchPoolVolumes fetches all pools and returns (pools, nodeVolumes, err).
// Callers that need both the pool list (e.g. for cleanup) and the node volumes
// should call this once and reuse the returned pools, avoiding a second
// GetStoragePools round-trip within the same reconcile.
func fetchPoolVolumes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
) (pools []webapi.StoragePoolInfo, nodeVols []webapi.VolumeInfo, err error) {
	pools, err = apiClient.GetStoragePools(ctx, clusterUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("listNodeVolumes: %w", err)
	}
	for _, pool := range pools {
		vols, err := apiClient.GetPoolVolumes(ctx, clusterUUID, pool.UUID)
		if err != nil {
			return nil, nil, fmt.Errorf("listNodeVolumes: pool %s: %w", pool.UUID, err)
		}
		for _, v := range vols {
			if v.PrimaryNodeUUID != nodeUUID {
				continue
			}
			// Skip volumes already being deleted — backend deletion is async so
			// the volume may still appear in the list briefly after DELETE 204.
			if v.Status == "in_deletion" {
				continue
			}
			nodeVols = append(nodeVols, v)
		}
	}
	return pools, nodeVols, nil
}

// listNodeVolumes returns volumes on nodeUUID. Use fetchPoolVolumes when the
// pool list is also needed (e.g. drainVerify cleanup) to avoid a double fetch.
func listNodeVolumes(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	nodeUUID string,
) ([]webapi.VolumeInfo, error) {
	_, vols, err := fetchPoolVolumes(ctx, apiClient, clusterUUID, nodeUUID)
	return vols, err
}

// matchVolumesToPVs classifies each backend volume into one of three buckets:
//   - pvManaged: volume UUID has a corresponding simplyblock PV, not pinned
//   - pinned: volume UUID has a PV but the PVC has the pinned-volume annotation
//   - unmanaged: volume UUID has no corresponding PV (and is not a system volume)
//
// System volumes (those matching filterRegex by name) are skipped entirely.
// pvNameByVolumeUUID maps volume UUID → PV name for the pvManaged and pinned buckets.
// matchVolumesToPVs classifies each backend volume into pvManaged, pinned, or
// unmanaged buckets. System volumes matching filterRegex are skipped entirely.
//
// Note: if the PVC fetch for a PV-backed volume fails (e.g. API server
// temporarily unavailable), that volume is conservatively placed in the
// unmanaged bucket. This will block drain with an UnmanagedVolumeBlocking
// event until the next reconcile succeeds. It is a transient false-positive,
// not a permanent classification.
// matchVolumesToPVs classifies backend volumes and additionally returns
// pvcFetchFailed=true when at least one PV-backed volume could not be classified
// because its PVC GET failed transiently. Callers in Migrating must requeue on
// pvcFetchFailed to avoid silently skipping volumes that would stall Verifying.
// matchVolumesToPVs classifies backend volumes. For pinned volumes, pinnedTargets
// maps volumeUUID → annotation value, which must be the target storage-node UUID.
// An empty or non-UUID annotation value means "no specific target" and the volume
// will block drain until the user re-pins it to a valid target node.
func matchVolumesToPVs(
	ctx context.Context,
	r *StorageNodeSetReconciler,
	volumes []webapi.VolumeInfo,
	sysFilter *regexp.Regexp,
) (pvManaged, pinned, unmanaged []string, pvNameByVolumeUUID map[string]string, pinnedTargets map[string]string, pvcFetchFailed bool, err error) {
	log := logf.FromContext(ctx)

	pvNameByVolumeUUID = make(map[string]string)
	pinnedTargets = make(map[string]string)

	// Build a map: volumeUUID → pvName for all simplyblock PVs.
	var pvList corev1.PersistentVolumeList
	if err = r.List(ctx, &pvList); err != nil {
		return nil, nil, nil, nil, nil, false, fmt.Errorf("matchVolumesToPVs: list PVs: %w", err)
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

	for _, vol := range volumes {
		// System volume: skip entirely.
		if sysFilter.MatchString(vol.Name) {
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
			pvcFetchFailed = true
			continue
		}

		if annotVal, isPinned := pvc.Annotations[simplyblockv1alpha1.AnnotationPinnedVolume]; isPinned {
			pinned = append(pinned, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
			pinnedTargets[vol.UUID] = annotVal
		} else {
			pvManaged = append(pvManaged, vol.UUID)
			pvNameByVolumeUUID[vol.UUID] = pv.Name
		}
	}

	return pvManaged, pinned, unmanaged, pvNameByVolumeUUID, pinnedTargets, pvcFetchFailed, nil
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

	// Guard against name collisions when two PV names share a long common prefix
	// that gets truncated to the same 63-char string. Append a 6-char FNV-32
	// hash of the original pvName before truncating so each PV always maps to a
	// unique CR name regardless of length.
	const maxLen = 63
	if len(s) > maxLen {
		h := fnv32Hash(pvName)
		suffix := fmt.Sprintf("-%06x", h) // 7 chars: '-' + 6 hex digits
		keep := maxLen - len(suffix)
		if keep < 0 {
			keep = 0
		}
		s = s[:keep] + suffix
	}
	return s
}

// fnv32Hash returns a non-cryptographic 32-bit FNV-1a hash of s, used as a
// uuidRe matches a canonical UUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isValidUUID reports whether s is a well-formed lowercase UUID.
func isValidUUID(s string) bool { return uuidRe.MatchString(s) }

// short disambiguation suffix in drainMigrationName.
func fnv32Hash(s string) uint32 {
	const (
		offset32 uint32 = 2166136261
		prime32  uint32 = 16777619
	)
	h := offset32
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}
