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
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

// SnapshotReplicationReconciler reconciles a SnapshotReplication object
type SnapshotReplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=snapshotreplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=snapshotreplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=snapshotreplications/finalizers,verbs=update

func (r *SnapshotReplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	apiClient := webapi.NewClient()

	snapRepCR, err := r.getSnapRepCR(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if snapRepCR == nil {
		return ctrl.Result{}, nil
	}

	clusterUUID, clusterSecret, res, err := r.resolveSourceClusterAuth(ctx, snapRepCR)
	if err != nil {
		log.Error(err, "Failed to resolve source cluster auth")
		r.setCondition(ctx, snapRepCR, simplyblockv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "AuthFailed", err.Error())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if res != nil {
		return *res, nil
	}

	// Deletion
	if updated, err := r.handleDeletion(ctx, snapRepCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	// Finalizer
	if updated, err := r.ensureFinalizer(ctx, snapRepCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	// Step 1: ensure addreplication is configured (one-time setup, idempotent via status.configured)
	if res := r.ensureConfigured(ctx, apiClient, snapRepCR, clusterUUID, clusterSecret); res != nil {
		return *res, nil
	}

	// Step 2: failback action — phase-driven per volume
	if snapRepCR.Spec.Action == "failback" {
		if snapRepCR.Status.ObservedFailbackGeneration == snapRepCR.Generation {
			log.Info("Failback already processed for current generation, skipping",
				"name", snapRepCR.Name,
				"generation", snapRepCR.Generation,
			)
			return ctrl.Result{RequeueAfter: 120 * time.Second}, nil
		}

		requeue, err := r.reconcileFailback(ctx, apiClient, snapRepCR, clusterUUID, clusterSecret)
		if err != nil {
			log.Error(err, "Failback reconciliation error")
			r.setCondition(ctx, snapRepCR, simplyblockv1alpha1.ConditionTypeFailback, metav1.ConditionFalse, "Error", err.Error())
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if requeue > 0 {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

		// All volumes reached terminal phase — mark generation processed
		orig := snapRepCR.DeepCopy()
		snapRepCR.Status.ObservedFailbackGeneration = snapRepCR.Generation
		r.setConditionOnCopy(snapRepCR, simplyblockv1alpha1.ConditionTypeFailback, metav1.ConditionTrue, "Completed", "All volumes processed")
		if err := r.Status().Patch(ctx, snapRepCR, client.MergeFrom(orig)); err != nil {
			log.Error(err, "Failed to patch failback generation")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 120 * time.Second}, nil
	}

	// Step 3: normal periodic replication — phase-driven per volume
	if err := r.reconcileNormalReplication(ctx, apiClient, snapRepCR, clusterUUID, clusterSecret); err != nil {
		log.Error(err, "Normal replication reconciliation error")
		r.setCondition(ctx, snapRepCR, simplyblockv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ReplicationError", err.Error())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	r.setCondition(ctx, snapRepCR, simplyblockv1alpha1.ConditionTypeReady, metav1.ConditionTrue, "Replicating", "Replication is running")
	return ctrl.Result{RequeueAfter: time.Duration(utils.IntPtrOrDefault(snapRepCR.Spec.Interval, 300)) * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SnapshotReplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SnapshotReplication{}).
		Named("snapshotreplication").
		Complete(r)
}

/* -------------------- Phase-driven failback reconciliation -------------------- */

// reconcileFailback drives each volume through its failback phases.
// Returns a non-zero requeue duration if any volume is still in-progress,
// or 0 if all volumes are in a terminal phase (Completed or Failed).
func (r *SnapshotReplicationReconciler) reconcileFailback(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	sourceClusterUUID, sourceClusterSecret string,
) (time.Duration, error) {
	log := logf.FromContext(ctx)

	// Verify source cluster is active before proceeding.
	sourceActive, status, err := utils.IsClusterActive(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return 0, fmt.Errorf("failed to verify source cluster active state: %w", err)
	}
	if !sourceActive {
		log.Info("Source cluster not active yet, requeuing",
			"sourceCluster", snapRepCR.Spec.SourceCluster,
			"status", status,
		)
		return 15 * time.Second, nil
	}

	targetClusterUUID, err := utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		log.Info("Target cluster UUID not ready, requeuing", "cluster", snapRepCR.Spec.TargetCluster)
		return 10 * time.Second, nil
	}

	_, targetClusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		return 0, fmt.Errorf("failed to get target cluster auth: %w", err)
	}

	targetPoolUUID, err := utils.ResolvePoolIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster, snapRepCR.Spec.TargetPool)
	if err != nil {
		log.Info("Target pool UUID not found, requeuing", "pool", snapRepCR.Spec.TargetPool)
		return 10 * time.Second, nil
	}

	lvols, err := utils.GetLvols(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
	if err != nil {
		return 0, fmt.Errorf("failed to list target lvols for failback: %w", err)
	}

	includeIDs := snapRepCR.Spec.IncludeVolumeIDs
	excludeIDs := snapRepCR.Spec.ExcludeVolumeIDs

	orig := snapRepCR.DeepCopy()
	r.setConditionOnCopy(snapRepCR, simplyblockv1alpha1.ConditionTypeFailback, metav1.ConditionFalse, "InProgress", "Failback in progress")

	anyInProgress := false

	for _, lvolSummary := range lvols {
		lvolDetail, err := utils.GetLvol(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, lvolSummary.UUID)
		if err != nil {
			log.Error(err, "Failed to get target lvol", "lvolUUID", lvolSummary.UUID)
			r.setVolumePhase(snapRepCR, lvolSummary.UUID, simplyblockv1alpha1.VolPhaseFailed, err.Error())
			anyInProgress = true
			continue
		}

		filterID := failbackFilterID(lvolDetail)

		if !shouldProcessFailbackVolume(filterID, includeIDs, excludeIDs) {
			continue
		}

		currentPhase := r.getVolumePhase(snapRepCR, lvolDetail.UUID)

		// Terminal phases — skip.
		if currentPhase == simplyblockv1alpha1.VolPhaseCompleted ||
			currentPhase == simplyblockv1alpha1.VolPhaseFailed {
			continue
		}

		requeue, advErr := r.advanceFailbackVolume(
			ctx, apiClient, snapRepCR,
			sourceClusterUUID, sourceClusterSecret,
			targetClusterUUID, targetClusterSecret,
			targetPoolUUID, lvolDetail,
			currentPhase,
		)
		if advErr != nil {
			log.Error(advErr, "Failed to advance failback volume phase",
				"lvolUUID", lvolDetail.UUID, "phase", currentPhase)
			r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseFailed, advErr.Error())
		}
		if requeue {
			anyInProgress = true
		}
	}

	if err := r.Status().Patch(ctx, snapRepCR, client.MergeFrom(orig)); err != nil {
		log.Error(err, "Failed to patch volume phase status")
		return 10 * time.Second, nil
	}

	if anyInProgress {
		return 10 * time.Second, nil
	}
	return 0, nil
}

// advanceFailbackVolume advances a single volume through its failback phases.
// Returns true if the volume is still in-progress and needs requeuing.
func (r *SnapshotReplicationReconciler) advanceFailbackVolume(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	sourceClusterUUID, sourceClusterSecret string,
	targetClusterUUID, targetClusterSecret string,
	targetPoolUUID string,
	lvolDetail *utils.Lvol,
	currentPhase string,
) (inProgress bool, err error) {
	log := logf.FromContext(ctx)

	switch currentPhase {

	case "", simplyblockv1alpha1.VolPhasePending:
		// Phase 1 — trigger replicate_lvol on target cluster.
		// First check if the lvol already exists on source to avoid duplicate calls.
		targetIDs, buildErr := buildLvolIDSet(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, "")
		if buildErr != nil {
			log.Info("Could not build source lvol ID set, proceeding with replicate_lvol", "err", buildErr)
		}

		if id, ok := lvolIDFromNQN(lvolDetail.NQN); ok {
			if _, exists := targetIDs[id]; exists {
				log.Info("Lvol already exists on source cluster, skipping replicate_lvol",
					"lvolUUID", lvolDetail.UUID, "lvolID", id)
				r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseReplicatingToSource, "already on source, proceeding to failback")
				return true, nil
			}
		}

		if err := replicateLvol(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, "", lvolDetail.UUID); err != nil {
			return false, fmt.Errorf("replicate_lvol failed: %w", err)
		}
		r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseTriggeringTargetReplication, "replicate_lvol dispatched")
		return true, nil

	case simplyblockv1alpha1.VolPhaseTriggeringTargetReplication:
		// Phase 2 — wait for replicate_lvol task to complete.
		done, task, err := utils.GetLastSnapshotTaskDoneStatus(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, "", lvolDetail.UUID)
		if err != nil {
			log.Info("Cannot check task status, retrying", "lvolUUID", lvolDetail.UUID, "err", err)
			return true, nil
		}
		if !done {
			log.Info("Waiting for replicate_lvol task", "lvolUUID", lvolDetail.UUID, "taskID", task.UUID, "status", task.Status)
			return true, nil
		}
		r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseWaitingForTargetReplication, "replicate_lvol task done")
		return true, nil

	case simplyblockv1alpha1.VolPhaseWaitingForTargetReplication:
		// Phase 3 — trigger failback from target back to source.
		sourcePoolUUID, sourceLvolUUID, isFreshCluster, err := r.resolveSourceFailbackTarget(
			ctx, apiClient, snapRepCR, sourceClusterSecret, sourceClusterUUID, lvolDetail,
		)
		if err != nil {
			return false, fmt.Errorf("resolve source failback target: %w", err)
		}

		if err := failbackLvol(
			ctx, apiClient,
			sourceClusterSecret, sourceClusterUUID, sourcePoolUUID, sourceLvolUUID,
			targetClusterUUID, targetPoolUUID,
			lvolDetail, isFreshCluster,
		); err != nil {
			return false, fmt.Errorf("failback_lvol failed: %w", err)
		}
		r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseReplicatingToSource, "failback triggered")
		return true, nil

	case simplyblockv1alpha1.VolPhaseReplicatingToSource:
		// Phase 4 — wait for failback task to complete, then request target cleanup.
		done, task, err := utils.GetLastSnapshotTaskDoneStatus(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, lvolDetail.UUID)
		if err != nil {
			log.Info("Cannot check failback task status, retrying", "lvolUUID", lvolDetail.UUID, "err", err)
			return true, nil
		}
		if !done {
			log.Info("Waiting for failback task", "lvolUUID", lvolDetail.UUID, "taskID", task.UUID, "status", task.Status)
			return true, nil
		}
		r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseWaitingForTargetDeletion, "failback task done")
		return true, nil

	case simplyblockv1alpha1.VolPhaseWaitingForTargetDeletion:
		// Phase 5 — confirm the target-side snapshot has been cleaned up.
		// Re-check whether the lvol still exists on target; if not, we're done.
		lvols, err := utils.GetLvols(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
		if err != nil {
			log.Info("Cannot list target lvols, retrying", "err", err)
			return true, nil
		}
		for _, tl := range lvols {
			if tl.UUID == lvolDetail.UUID {
				log.Info("Target lvol still exists, waiting for deletion", "lvolUUID", lvolDetail.UUID)
				return true, nil
			}
		}
		r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseCompleted, "failback complete")
		log.Info("Failback complete for volume", "lvolUUID", lvolDetail.UUID)
		return false, nil
	}

	return false, fmt.Errorf("unknown failback phase %q for volume %s", currentPhase, lvolDetail.UUID)
}

/* -------------------- Normal periodic replication -------------------- */

func (r *SnapshotReplicationReconciler) reconcileNormalReplication(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	sourceClusterUUID, sourceClusterSecret string,
) error {
	log := logf.FromContext(ctx)

	failover, targetIDs, res, err := r.computeFailoverAndTargetIDs(ctx, apiClient, snapRepCR, sourceClusterUUID, sourceClusterSecret)
	if err != nil {
		return err
	}
	if res != nil {
		return nil
	}

	interval := utils.IntPtrOrDefault(snapRepCR.Spec.Interval, 300)
	now := time.Now().UTC()

	orig := snapRepCR.DeepCopy()
	changed := false

	poolUUIDs, err := utils.GetPoolUUIDs(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return err
	}

	for _, poolUUID := range poolUUIDs {
		lvols, err := utils.GetLvols(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID)
		if err != nil {
			log.Error(err, "Failed to list lvols", "poolUUID", poolUUID)
			continue
		}

		for _, lvolSummary := range lvols {
			if !lvolSummary.DoReplicate {
				continue
			}

			if len(snapRepCR.Spec.VolumeIDs) > 0 && !slices.Contains(snapRepCR.Spec.VolumeIDs, lvolSummary.UUID) {
				continue
			}

			lvolDetail, err := utils.GetLvol(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID, lvolSummary.UUID)
			if err != nil {
				log.Error(err, "Failed to get lvol", "lvolUUID", lvolSummary.UUID)
				r.setVolumePhase(snapRepCR, lvolSummary.UUID, simplyblockv1alpha1.VolPhaseFailed, err.Error())
				changed = true
				continue
			}

			if failover {
				triggered := r.handleFailoverReplication(ctx, apiClient, snapRepCR, sourceClusterUUID, sourceClusterSecret, poolUUID, lvolDetail, targetIDs)
				if triggered {
					r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseTriggeringTargetReplication, "failover replicate_lvol dispatched")
					changed = true
				}
				continue
			}

			triggered := r.handleNormalReplication(ctx, apiClient, sourceClusterUUID, sourceClusterSecret, poolUUID, lvolDetail, interval, now)
			if triggered {
				r.setVolumePhase(snapRepCR, lvolDetail.UUID, simplyblockv1alpha1.VolPhaseRunning, "replication triggered")
				now2 := metav1.Now()
				r.setVolumeLastReplicationTime(snapRepCR, lvolDetail.UUID, &now2)
				r.setVolumeRepInfo(snapRepCR, lvolDetail)
				changed = true
			}
		}
	}

	if changed {
		if err := r.Status().Patch(ctx, snapRepCR, client.MergeFrom(orig)); err != nil {
			log.Error(err, "Failed to patch volume status after normal replication")
		}
	}

	return nil
}

/* -------------------- helpers -------------------- */

func (r *SnapshotReplicationReconciler) getSnapRepCR(
	ctx context.Context,
	req ctrl.Request,
) (*simplyblockv1alpha1.SnapshotReplication, error) {
	snapRepCR := &simplyblockv1alpha1.SnapshotReplication{}
	if err := r.Get(ctx, req.NamespacedName, snapRepCR); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return snapRepCR, nil
}

func (r *SnapshotReplicationReconciler) resolveSourceClusterAuth(
	ctx context.Context,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
) (clusterUUID string, clusterSecret string, res *ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	clusterUUID, err = utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.SourceCluster)
	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing", "cluster", snapRepCR.Spec.SourceCluster)
		tmp := ctrl.Result{RequeueAfter: 10 * time.Second}
		return "", "", &tmp, nil
	}

	_, clusterSecret, err = utils.GetClusterAuth(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.SourceCluster)
	if err != nil {
		tmp := ctrl.Result{RequeueAfter: 10 * time.Second}
		return "", "", &tmp, err
	}

	return clusterUUID, clusterSecret, nil, nil
}

func (r *SnapshotReplicationReconciler) resolveSourcePoolForFreshFailback(
	ctx context.Context,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
) (string, error) {
	if strings.TrimSpace(snapRepCR.Spec.SourcePool) == "" {
		return "", fmt.Errorf("spec.sourcePool must be set for failback to a fresh source cluster")
	}

	return utils.ResolvePoolIdentifier(
		ctx, r.Client, snapRepCR.Namespace,
		snapRepCR.Spec.SourceCluster, snapRepCR.Spec.SourcePool,
	)
}

func (r *SnapshotReplicationReconciler) ensureConfigured(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	sourceClusterUUID string,
	sourceClusterSecret string,
) *ctrl.Result {
	log := logf.FromContext(ctx)

	if snapRepCR.Status.Configured {
		return nil
	}

	targetClusterUUID, err := utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		log.Info("Target cluster UUID not found, requeuing", "cluster", snapRepCR.Spec.TargetCluster)
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res
	}

	targetPoolUUID, err := utils.ResolvePoolIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster, snapRepCR.Spec.TargetPool)
	if err != nil {
		log.Info("Target pool UUID not found, requeuing", "pool", snapRepCR.Spec.TargetPool)
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res
	}

	params := utils.ReplicationAddParams{
		TargetCluster: targetClusterUUID,
		Timeout:       utils.IntPtrOrDefault(snapRepCR.Spec.Timeout, 0),
		TargetPool:    targetPoolUUID,
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/addreplication/", sourceClusterUUID)
	body, status, err := apiClient.Do(ctx, sourceClusterSecret, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		log.Error(err, "Cluster add replication failed", "status", status, "response", string(body))
		res := ctrl.Result{RequeueAfter: 20 * time.Second}
		return &res
	}

	orig := snapRepCR.DeepCopy()
	snapRepCR.Status.Configured = true
	r.setConditionOnCopy(snapRepCR, simplyblockv1alpha1.ConditionTypeConfigured, metav1.ConditionTrue, "Configured", "addreplication completed successfully")
	if err := r.Status().Patch(ctx, snapRepCR, client.MergeFrom(orig)); err != nil {
		log.Error(err, "Failed to patch configured status")
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res
	}

	res := ctrl.Result{RequeueAfter: 10 * time.Second}
	return &res
}

func (r *SnapshotReplicationReconciler) computeFailoverAndTargetIDs(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	sourceClusterUUID string,
	sourceClusterSecret string,
) (failover bool, targetIDs map[string]struct{}, res *ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	failover, err = utils.ShouldFailoverToRepCluster(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return false, nil, nil, err
	}

	targetIDs = map[string]struct{}{}
	if !failover {
		return false, targetIDs, nil, nil
	}

	targetClusterUUID, err := utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		log.Info("Target cluster UUID not ready, requeuing", "cluster", snapRepCR.Spec.TargetCluster)
		tmp := ctrl.Result{RequeueAfter: 10 * time.Second}
		return true, targetIDs, &tmp, nil
	}

	_, targetClusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		tmp := ctrl.Result{RequeueAfter: 10 * time.Second}
		return true, targetIDs, &tmp, err
	}

	targetPoolUUID, err := utils.ResolvePoolIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster, snapRepCR.Spec.TargetPool)
	if err != nil {
		log.Info("Target pool UUID not found, requeuing", "pool", snapRepCR.Spec.TargetPool)
		tmp := ctrl.Result{RequeueAfter: 10 * time.Second}
		return true, targetIDs, &tmp, nil
	}

	ids, err := buildLvolIDSet(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
	if err != nil {
		log.Error(err, "Failed to build target lvol ID set")
		return true, targetIDs, nil, nil
	}

	return true, ids, nil, nil
}

// handleFailoverReplication triggers replicate_lvol and returns whether it was dispatched.
func (r *SnapshotReplicationReconciler) handleFailoverReplication(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	clusterUUID string,
	clusterSecret string,
	poolUUID string,
	lvolDetail *utils.Lvol,
	targetIDs map[string]struct{},
) bool {
	log := logf.FromContext(ctx)

	currentPhase := r.getVolumePhase(snapRepCR, lvolDetail.UUID)
	if currentPhase == simplyblockv1alpha1.VolPhaseTriggeringTargetReplication ||
		currentPhase == simplyblockv1alpha1.VolPhaseCompleted {
		return false
	}

	if id, ok := lvolIDFromNQN(lvolDetail.NQN); ok {
		if _, exists := targetIDs[id]; exists {
			return false
		}
	}

	if err := replicateLvol(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID); err != nil {
		log.Error(err, "Failed to trigger replicate_lvol", "lvolUUID", lvolDetail.UUID)
		return false
	}

	return true
}

// handleNormalReplication triggers a replication cycle and returns whether it was triggered.
func (r *SnapshotReplicationReconciler) handleNormalReplication(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	clusterSecret string,
	poolUUID string,
	lvolDetail *utils.Lvol,
	interval int,
	now time.Time,
) bool {
	log := logf.FromContext(ctx)

	activeOnSource, err := utils.GetReplicationActiveSides(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID)
	if err != nil {
		log.Error(err, "Failed to determine active side", "lvolUUID", lvolDetail.UUID)
		return false
	}
	if !activeOnSource {
		return false
	}

	if !shouldReplicate(lvolDetail, interval, now) {
		return false
	}

	done, _, err := utils.GetLastSnapshotTaskDoneStatus(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID)
	if err != nil {
		log.Error(err, "Failed to check last snapshot task", "lvolUUID", lvolDetail.UUID)
		return false
	}
	if !done {
		return false
	}

	if err := triggerReplication(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID); err != nil {
		log.Error(err, "Failed to trigger replication", "lvolUUID", lvolDetail.UUID)
		return false
	}

	return true
}

func (r *SnapshotReplicationReconciler) handleDeletion(
	ctx context.Context,
	SnapRepCR *simplyblockv1alpha1.SnapshotReplication,
) (bool, error) {
	if SnapRepCR.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if !controllerutil.ContainsFinalizer(SnapRepCR, "simplyblock.replication.finalizer") {
		return true, nil
	}
	controllerutil.RemoveFinalizer(SnapRepCR, "simplyblock.replication.finalizer")
	return true, r.Update(ctx, SnapRepCR)
}

func (r *SnapshotReplicationReconciler) ensureFinalizer(
	ctx context.Context,
	SnapRepCR *simplyblockv1alpha1.SnapshotReplication,
) (bool, error) {
	if controllerutil.ContainsFinalizer(SnapRepCR, "simplyblock.replication.finalizer") {
		return false, nil
	}
	controllerutil.AddFinalizer(SnapRepCR, "simplyblock.replication.finalizer")
	return true, r.Update(ctx, SnapRepCR)
}

/* -------------------- Volume phase helpers -------------------- */

func (r *SnapshotReplicationReconciler) getVolumePhase(
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	volumeID string,
) string {
	for _, v := range snapRepCR.Status.Volumes {
		if v.VolumeID == volumeID {
			return v.Phase
		}
	}
	return ""
}

func (r *SnapshotReplicationReconciler) setVolumePhase(
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	volumeID, phase, message string,
) {
	for i := range snapRepCR.Status.Volumes {
		if snapRepCR.Status.Volumes[i].VolumeID == volumeID {
			snapRepCR.Status.Volumes[i].Phase = phase
			if message != "" && phase == simplyblockv1alpha1.VolPhaseFailed {
				snapRepCR.Status.Volumes[i].Errors = append(
					snapRepCR.Status.Volumes[i].Errors,
					simplyblockv1alpha1.ReplicationError{
						Timestamp: metav1.Now(),
						Message:   message,
					},
				)
			}
			return
		}
	}
	entry := simplyblockv1alpha1.VolumeReplicationStatus{
		VolumeID: volumeID,
		Phase:    phase,
	}
	if message != "" && phase == simplyblockv1alpha1.VolPhaseFailed {
		entry.Errors = []simplyblockv1alpha1.ReplicationError{
			{Timestamp: metav1.Now(), Message: message},
		}
	}
	snapRepCR.Status.Volumes = append(snapRepCR.Status.Volumes, entry)
}

func (r *SnapshotReplicationReconciler) setVolumeRepInfo(
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	lvol *utils.Lvol,
) {
	if lvol.RepInfo == nil {
		return
	}
	count := int32(lvol.RepInfo.ReplicatedCount)
	for i := range snapRepCR.Status.Volumes {
		if snapRepCR.Status.Volumes[i].VolumeID == lvol.UUID {
			snapRepCR.Status.Volumes[i].LastSnapshotID = lvol.RepInfo.LastSnapshotUUID
			snapRepCR.Status.Volumes[i].ReplicatedCount = &count
			return
		}
	}
}

func (r *SnapshotReplicationReconciler) setVolumeLastReplicationTime(
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	volumeID string,
	t *metav1.Time,
) {
	for i := range snapRepCR.Status.Volumes {
		if snapRepCR.Status.Volumes[i].VolumeID == volumeID {
			snapRepCR.Status.Volumes[i].LastReplicationTime = t
			return
		}
	}
}

/* -------------------- Condition helpers -------------------- */

// setCondition patches the named condition on the CR directly (issues its own Status patch).
func (r *SnapshotReplicationReconciler) setCondition(
	ctx context.Context,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	orig := snapRepCR.DeepCopy()
	r.setConditionOnCopy(snapRepCR, condType, status, reason, message)
	if err := r.Status().Patch(ctx, snapRepCR, client.MergeFrom(orig)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to patch condition", "type", condType)
	}
}

// setConditionOnCopy mutates the in-memory CR without patching (caller must patch).
func (r *SnapshotReplicationReconciler) setConditionOnCopy(
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i, c := range snapRepCR.Status.Conditions {
		if c.Type == condType {
			if c.Status == status && c.Reason == reason {
				return
			}
			snapRepCR.Status.Conditions[i] = metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: snapRepCR.Generation,
			}
			return
		}
	}
	snapRepCR.Status.Conditions = append(snapRepCR.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: snapRepCR.Generation,
	})
}

/* -------------------- Source failback resolution -------------------- */

func (r *SnapshotReplicationReconciler) resolveSourceFailbackTarget(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SnapshotReplication,
	sourceClusterSecret, sourceClusterUUID string,
	targetLvol *utils.Lvol,
) (sourcePoolUUID, sourceLvolUUID string, isFreshCluster bool, err error) {
	log := logf.FromContext(ctx)

	sourcePools, err := utils.GetPoolUUIDs(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return "", "", false, fmt.Errorf("list source pools: %w", err)
	}

	for _, poolUUID := range sourcePools {
		sourceLvols, err := utils.GetLvols(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID)
		if err != nil {
			log.Error(err, "Failed to list source lvols", "poolUUID", poolUUID)
			continue
		}
		for _, sl := range sourceLvols {
			if id, ok := lvolIDFromNQN(sl.NQN); ok {
				targetID, _ := lvolIDFromNQN(targetLvol.NQN)
				if id == targetID {
					return poolUUID, sl.UUID, false, nil
				}
			}
		}
	}

	// Not found — fresh source cluster path.
	sourcePoolUUID, err = r.resolveSourcePoolForFreshFailback(ctx, snapRepCR)
	if err != nil {
		return "", "", true, err
	}
	return sourcePoolUUID, targetLvol.UUID, true, nil
}

/* -------------------- Pure functions -------------------- */

func triggerReplication(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, lvolUUID string) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication_trigger/",
		clusterUUID, poolUUID, lvolUUID,
	)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("trigger replication for lvol %s, status %d: %v, body: %s", lvolUUID, status, err, string(body))
	}
	return nil
}

func shouldReplicate(lvol *utils.Lvol, interval int, now time.Time) bool {
	if interval <= 0 {
		return false
	}
	if lvol.RepInfo.LastReplicationTime == nil {
		return true
	}
	nextRun := lvol.RepInfo.LastReplicationTime.Add(time.Duration(interval) * time.Second)
	return !now.Before(nextRun)
}

func replicateLvol(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, lvolUUID string) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replicate_lvol/",
		clusterUUID, poolUUID, lvolUUID,
	)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("replicate_lvol for %s, status %d: %v, body: %s", lvolUUID, status, err, string(body))
	}
	return nil
}

func lvolIDFromNQN(nqn string) (string, bool) {
	const marker = "lvol:"
	i := strings.LastIndex(nqn, marker)
	if i < 0 {
		return "", false
	}
	id := strings.TrimSpace(nqn[i+len(marker):])
	if id == "" {
		return "", false
	}
	return id, true
}

func buildLvolIDSet(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, poolUUID string,
) (map[string]struct{}, error) {
	var allLvols []utils.Lvol
	var err error

	if poolUUID != "" {
		allLvols, err = utils.GetLvols(ctx, apiClient, clusterSecret, clusterUUID, poolUUID)
		if err != nil {
			return nil, err
		}
	} else {
		pools, poolErr := utils.GetPoolUUIDs(ctx, apiClient, clusterSecret, clusterUUID)
		if poolErr != nil {
			return nil, poolErr
		}
		for _, p := range pools {
			pl, plErr := utils.GetLvols(ctx, apiClient, clusterSecret, clusterUUID, p)
			if plErr != nil {
				continue
			}
			allLvols = append(allLvols, pl...)
		}
	}

	ids := make(map[string]struct{}, len(allLvols))
	for _, tl := range allLvols {
		if tl.NQN == "" {
			continue
		}
		if id, ok := lvolIDFromNQN(tl.NQN); ok {
			ids[id] = struct{}{}
		}
	}

	return ids, nil
}

func shouldProcessFailbackVolume(volumeID string, includeIDs, excludeIDs []string) bool {
	if len(includeIDs) > 0 && !slices.Contains(includeIDs, volumeID) {
		return false
	}
	return !slices.Contains(excludeIDs, volumeID)
}

func failbackFilterID(lvol *utils.Lvol) string {
	if id, ok := lvolIDFromNQN(lvol.NQN); ok {
		return id
	}
	return lvol.UUID
}

func failbackLvol(
	ctx context.Context,
	apiClient *webapi.Client,
	sourceClusterSecret, sourceClusterUUID, sourcePoolUUID, sourceLvolUUID string,
	targetClusterUUID, targetPoolUUID string,
	targetLvol *utils.Lvol,
	isFreshCluster bool,
) error {
	if isFreshCluster {
		if err := startReplicationOnFreshSource(
			ctx, apiClient,
			sourceClusterSecret, sourceClusterUUID, sourcePoolUUID,
			targetLvol.UUID, targetClusterUUID,
		); err != nil {
			return err
		}
	}

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/failback/",
		sourceClusterUUID, sourcePoolUUID, sourceLvolUUID,
	)
	body, status, err := apiClient.Do(ctx, sourceClusterSecret, http.MethodPost, endpoint, map[string]string{
		"target_cluster_id": targetClusterUUID,
		"target_pool_id":    targetPoolUUID,
		"target_lvol_id":    targetLvol.UUID,
	})
	if err != nil || status >= 300 {
		return fmt.Errorf("failback for lvol %s, status %d: %v, body: %s", sourceLvolUUID, status, err, string(body))
	}
	return nil
}

func startReplicationOnFreshSource(
	ctx context.Context,
	apiClient *webapi.Client,
	sourceClusterSecret, sourceClusterUUID, sourcePoolUUID string,
	targetLvolUUID, targetClusterUUID string,
) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/start_replication/",
		sourceClusterUUID, sourcePoolUUID,
	)
	body, status, err := apiClient.Do(ctx, sourceClusterSecret, http.MethodPost, endpoint, map[string]string{
		"target_lvol_id":    targetLvolUUID,
		"target_cluster_id": targetClusterUUID,
	})
	if err != nil || status >= 300 {
		return fmt.Errorf("start_replication on fresh source, status %d: %v, body: %s", status, err, string(body))
	}
	return nil
}
