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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

// SimplyBlockSnapshotReplicationReconciler reconciles a SimplyBlockSnapshotReplication object
type SimplyBlockSnapshotReplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocksnapshotreplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocksnapshotreplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocksnapshotreplications/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimplyBlockSnapshotReplication object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *SimplyBlockSnapshotReplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	apiClient := webapi.NewClient()

	snapRepCR, err := r.getSnapRepCR(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if snapRepCR == nil {
		return ctrl.Result{}, nil // not found
	}

	clusterUUID, clusterSecret, res, err := r.resolveSourceClusterAuth(ctx, snapRepCR)
	if err != nil {
		log.Error(err, "Failed to resolve source cluster auth")
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

	// Ensure addreplication is configured once
	if res := r.ensureConfigured(ctx, apiClient, snapRepCR, clusterUUID, clusterSecret); res != nil {
		return *res, nil
	}

	if snapRepCR.Spec.Action == "failback" {
		if snapRepCR.Status.ObservedFailbackGeneration == snapRepCR.Generation {
			log.Info("Failback already processed for current generation, skipping",
				"name", snapRepCR.Name,
				"generation", snapRepCR.Generation,
			)
			return ctrl.Result{RequeueAfter: 120 * time.Second}, nil
		}

		res, processed, err := r.handleFailbackAction(
			ctx,
			apiClient,
			snapRepCR,
			clusterUUID,
			clusterSecret,
		)
		if err != nil {
			log.Error(err, "Failback failed")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if res != nil {
			return *res, nil
		}

		if processed {
			orig := snapRepCR.DeepCopy()
			snapRepCR.Status.ObservedFailbackGeneration = snapRepCR.Generation
			if err := r.Status().Patch(ctx, snapRepCR, client.MergeFrom(orig)); err != nil {
				log.Error(err, "Failed to patch failback processed generation")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		}

		return ctrl.Result{RequeueAfter: 120 * time.Second}, nil
	}

	failover, targetIDs, res, err := r.computeFailoverAndTargetIDs(ctx, apiClient, snapRepCR, clusterUUID, clusterSecret)
	if err != nil {
		log.Error(err, "Failover pre-check failed", "clusterUUID", clusterUUID)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if res != nil {
		return *res, nil
	}

	interval := utils.IntPtrOrDefault(snapRepCR.Spec.Interval, 300)
	now := time.Now().UTC()

	if err := r.replicateAcrossPools(ctx, apiClient, snapRepCR, clusterUUID, clusterSecret, failover, targetIDs, interval, now); err != nil {
		log.Error(err, "Replication loop failed")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: 120 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockSnapshotReplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockSnapshotReplication{}).
		Named("simplyblocksnapshotreplication").
		Complete(r)
}

/* -------------------- helpers func to reduce Reconcile complexity -------------------- */

func (r *SimplyBlockSnapshotReplicationReconciler) getSnapRepCR(
	ctx context.Context,
	req ctrl.Request,
) (*simplyblockv1alpha1.SimplyBlockSnapshotReplication, error) {
	snapRepCR := &simplyblockv1alpha1.SimplyBlockSnapshotReplication{}
	if err := r.Get(ctx, req.NamespacedName, snapRepCR); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return snapRepCR, nil
}

func (r *SimplyBlockSnapshotReplicationReconciler) resolveSourceClusterAuth(
	ctx context.Context,
	snapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
) (clusterUUID string, clusterSecret string, res *ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	clusterUUID, err = utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.SourceCluster)
	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", snapRepCR.Spec.SourceCluster,
		)
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

func (r *SimplyBlockSnapshotReplicationReconciler) ensureConfigured(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
	sourceClusterUUID string,
	sourceClusterSecret string,
) *ctrl.Result {
	log := logf.FromContext(ctx)

	if snapRepCR.Status.Configured {
		return nil
	}

	targetClusterUUID, err := utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		log.Info("Target cluster UUID not found, requeuing",
			"cluster", snapRepCR.Spec.TargetCluster,
		)
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res
	}

	targetPoolUUID, err := utils.ResolvePoolIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster, snapRepCR.Spec.TargetPool)
	if err != nil {
		log.Info("Target pool UUID not found, requeuing",
			"poolName", snapRepCR.Spec.TargetPool,
			"cluster", snapRepCR.Spec.TargetCluster,
		)
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
	patch := client.MergeFrom(orig)
	if err := r.Status().Patch(ctx, snapRepCR, patch); err != nil {
		log.Error(err, "Failed to patch snapshot replication status after creation")
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res
	}

	log.Info("Snapshot Replication successfully added", "name", snapRepCR.Name)
	res := ctrl.Result{RequeueAfter: 10 * time.Second}
	return &res
}

func (r *SimplyBlockSnapshotReplicationReconciler) computeFailoverAndTargetIDs(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
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
		log.Info("Target cluster UUID not ready yet, requeuing",
			"cluster", snapRepCR.Spec.TargetCluster,
		)
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
		log.Info("Target pool UUID not found, requeuing",
			"poolName", snapRepCR.Spec.TargetPool,
			"cluster", snapRepCR.Spec.TargetCluster,
		)
		tmp := ctrl.Result{RequeueAfter: 10 * time.Second}
		return true, targetIDs, &tmp, nil
	}

	ids, err := buildLvolIDSet(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
	if err != nil {
		log.Error(err, "Failed to build target lvol ID set; will not skip replicate_lvol")
		return true, targetIDs, nil, nil
	}

	return true, ids, nil, nil
}

func (r *SimplyBlockSnapshotReplicationReconciler) replicateAcrossPools(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
	sourceClusterUUID string,
	sourceClusterSecret string,
	failover bool,
	targetIDs map[string]struct{},
	interval int,
	now time.Time,
) error {
	log := logf.FromContext(ctx)

	poolUUIDs, err := utils.GetPoolUUIDs(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return err
	}
	log.Info("Pool UUIDs", "poolUUIDs", poolUUIDs)

	for _, poolUUID := range poolUUIDs {
		log.Info("POOL UUID", "poolUUID", poolUUID)

		lvols, err := utils.GetLvols(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID)
		if err != nil {
			log.Error(err, "Failed to list lvols", "poolUUID", poolUUID)
			continue
		}

		for _, lvolSummary := range lvols {
			if !lvolSummary.DoReplicate {
				continue
			}

			lvolDetail, err := utils.GetLvol(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID, lvolSummary.UUID)
			if err != nil {
				log.Error(err, "Failed to get lvol", "poolUUID", poolUUID, "lvolUUID", lvolSummary.UUID)
				continue
			}

			if failover {
				r.handleFailoverReplication(ctx, apiClient, snapRepCR, sourceClusterUUID, sourceClusterSecret, poolUUID, lvolDetail, targetIDs)
				continue
			}

			r.handleNormalReplication(ctx, apiClient, sourceClusterUUID, sourceClusterSecret, poolUUID, lvolDetail, interval, now)
		}
	}

	return nil
}

func (r *SimplyBlockSnapshotReplicationReconciler) handleFailoverReplication(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
	clusterUUID string,
	clusterSecret string,
	poolUUID string,
	lvolDetail *utils.Lvol,
	targetIDs map[string]struct{},
) {
	log := logf.FromContext(ctx)

	if id, ok := lvolIDFromNQN(lvolDetail.NQN); ok {
		if _, exists := targetIDs[id]; exists {
			log.Info("Skipping replicate_lvol: lvol already exists on target cluster",
				"lvol", lvolDetail.Name,
				"uuid", lvolDetail.UUID,
				"nqn", lvolDetail.NQN,
				"lvolID", id,
				"targetCluster", snapRepCR.Spec.TargetCluster,
			)
			return
		}
	} else {
		log.Info("Could not parse lvol ID from NQN; proceeding with replicate_lvol",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
			"nqn", lvolDetail.NQN,
		)
	}

	if err := replicateLvol(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID); err != nil {
		log.Error(err, "Failed to replicate lvol on target cluster",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
			"targetCluster", snapRepCR.Spec.TargetCluster,
		)
		return
	}

	log.Info("Started lvol Replication on Target Cluster",
		"lvol", lvolDetail.Name,
		"uuid", lvolDetail.UUID,
		"targetCluster", snapRepCR.Spec.TargetCluster,
	)
}

func (r *SimplyBlockSnapshotReplicationReconciler) handleNormalReplication(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	clusterSecret string,
	poolUUID string,
	lvolDetail *utils.Lvol,
	interval int,
	now time.Time,
) {
	log := logf.FromContext(ctx)

	activeOnSource, err := utils.GetReplicationActiveSides(
		ctx,
		apiClient,
		clusterSecret,
		clusterUUID,
		poolUUID,
		lvolDetail.UUID,
	)
	if err != nil {
		log.Error(err, "Failed to determine active side for source lvol",
			"lvolUUID", lvolDetail.UUID,
		)
		return
	}

	if !activeOnSource {
		log.Info("Skipping source trigger because target side is active",
			"lvolUUID", lvolDetail.UUID,
		)
		return
	}

	if !shouldReplicate(lvolDetail, interval, now) {
		log.Info("Skipping replication (interval not reached)",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
			"lastSnapshot", lvolDetail.RepInfo.LastSnapshotUUID,
			"intervalSec", interval,
		)
		return
	}

	done, task, err := utils.GetLastSnapshotTaskDoneStatus(
		ctx,
		apiClient,
		clusterSecret,
		clusterUUID,
		poolUUID,
		lvolDetail.UUID,
	)
	if err != nil {
		log.Error(err, "Failed to check last snapshot replication task",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
		)
		return
	}

	if !done {
		log.Info("skipping replication because previous snapshot replication task not done",
			"lvolUUID", lvolDetail.UUID,
			"taskID", task.UUID,
			"status", task.Status,
		)
		return
	}

	if err := triggerReplication(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID); err != nil {
		log.Error(err, "Failed to trigger replication",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
		)
		return
	}

	log.Info("Replication triggered for lvol",
		"lvol", lvolDetail.Name,
		"uuid", lvolDetail.UUID,
	)

}

/* -------------------- Existing helpers -------------------- */

func (r *SimplyBlockSnapshotReplicationReconciler) handleDeletion(
	ctx context.Context,
	SnapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
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

func (r *SimplyBlockSnapshotReplicationReconciler) ensureFinalizer(
	ctx context.Context,
	SnapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
) (bool, error) {

	if controllerutil.ContainsFinalizer(SnapRepCR, "simplyblock.replication.finalizer") {
		return false, nil
	}

	controllerutil.AddFinalizer(SnapRepCR, "simplyblock.replication.finalizer")
	return true, r.Update(ctx, SnapRepCR)
}

func triggerReplication(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, lvolUUID string) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication_trigger/",
		clusterUUID,
		poolUUID,
		lvolUUID,
	)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("failed to trigger replication for lvol %s, status %d: %v, body: %s", lvolUUID, status, err, string(body))
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
		clusterUUID,
		poolUUID,
		lvolUUID,
	)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("failed to start replication for lvol %s, status %d: %v, body: %s", lvolUUID, status, err, string(body))
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
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
) (map[string]struct{}, error) {
	log := logf.FromContext(ctx)

	targetLvols, err := utils.GetLvols(ctx, apiClient, clusterSecret, clusterUUID, poolUUID)
	if err != nil {
		return nil, err
	}

	ids := make(map[string]struct{}, len(targetLvols))
	for _, tl := range targetLvols {
		if tl.NQN == "" {
			continue
		}
		if id, ok := lvolIDFromNQN(tl.NQN); ok {
			ids[id] = struct{}{}
		}
	}

	log.Info("Built lvol ID set",
		"clusterUUID", clusterUUID,
		"poolUUID", poolUUID,
		"count", len(ids),
	)

	return ids, nil
}

func (r *SimplyBlockSnapshotReplicationReconciler) handleFailbackAction(
	ctx context.Context,
	apiClient *webapi.Client,
	snapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
	sourceClusterUUID string,
	sourceClusterSecret string,
) (*ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)

	sourceActive, status, err := utils.IsClusterActive(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to verify source cluster active state: %w", err)
	}
	if !sourceActive {
		log.Info("Source cluster is not active yet; skipping failback for now",
			"sourceCluster", snapRepCR.Spec.SourceCluster,
			"sourceClusterUUID", sourceClusterUUID,
			"status", status,
		)
		res := ctrl.Result{RequeueAfter: 15 * time.Second}
		return &res, false, nil
	}

	targetClusterUUID, err := utils.ResolveClusterIdentifier(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		log.Info("Target cluster UUID not ready yet, requeuing",
			"cluster", snapRepCR.Spec.TargetCluster,
		)
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, false, nil
	}

	_, targetClusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.TargetCluster)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get target cluster auth: %w", err)
	}

	targetPoolUUID, err := utils.ResolvePoolIdentifier(
		ctx,
		r.Client,
		snapRepCR.Namespace,
		snapRepCR.Spec.TargetCluster,
		snapRepCR.Spec.TargetPool,
	)
	if err != nil {
		log.Info("Target pool UUID not found, requeuing",
			"poolName", snapRepCR.Spec.TargetPool,
			"cluster", snapRepCR.Spec.TargetCluster,
		)
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, false, nil
	}

	lvols, err := utils.GetLvols(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to list target lvols for failback: %w", err)
	}

	includeIDs := snapRepCR.Spec.VolumeIDs
	excludeIDs := snapRepCR.Spec.ExcludeVolumeIDs

	allSucceeded := true

	for _, lvolSummary := range lvols {
		lvolDetail, err := utils.GetLvol(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, lvolSummary.UUID)
		if err != nil {
			log.Error(err, "Failed to get target lvol for failback",
				"targetClusterUUID", targetClusterUUID,
				"targetPoolUUID", targetPoolUUID,
				"lvolUUID", lvolSummary.UUID,
			)
			allSucceeded = false
			continue
		}

		filterID := failbackFilterID(lvolDetail)

		if !shouldProcessFailbackVolume(filterID, includeIDs, excludeIDs) {
			log.Info("Skipping lvol during failback due to include/exclude filters",
				"lvol", lvolDetail.Name,
				"lvolUUID", lvolDetail.UUID,
				"filterID", filterID,
				"includeVolumeIDs", includeIDs,
				"excludeVolumeIDs", excludeIDs,
			)
			continue
		}

		sourcePoolUUID, sourceLvolUUID, err := findSourceLvolForFailback(
			ctx,
			apiClient,
			sourceClusterSecret,
			sourceClusterUUID,
			lvolDetail,
		)
		if err != nil {
			log.Error(err, "Failed to resolve source lvol for failback",
				"targetLvolUUID", lvolDetail.UUID,
				"filterID", filterID,
			)
			allSucceeded = false
			continue
		}

		if err := failbackLvol(
			ctx,
			apiClient,
			sourceClusterSecret,
			sourceClusterUUID,
			sourcePoolUUID,
			sourceLvolUUID,
			targetClusterSecret,
			targetClusterUUID,
			targetPoolUUID,
			lvolDetail,
		); err != nil {
			log.Error(err, "Failed to start failback for lvol",
				"lvol", lvolDetail.Name,
				"lvolUUID", lvolDetail.UUID,
				"filterID", filterID,
			)
			allSucceeded = false
			continue
		}

		log.Info("Started failback for lvol",
			"lvol", lvolDetail.Name,
			"lvolUUID", lvolDetail.UUID,
			"filterID", filterID,
		)
	}

	if !allSucceeded {
		res := ctrl.Result{RequeueAfter: 15 * time.Second}
		return &res, false, nil
	}

	return nil, true, nil
}

func shouldProcessFailbackVolume(volumeID string, includeIDs, excludeIDs []string) bool {
	includeSet := make(map[string]struct{}, len(includeIDs))
	for _, id := range includeIDs {
		includeSet[id] = struct{}{}
	}

	excludeSet := make(map[string]struct{}, len(excludeIDs))
	for _, id := range excludeIDs {
		excludeSet[id] = struct{}{}
	}

	if len(includeSet) > 0 {
		if _, ok := includeSet[volumeID]; !ok {
			return false
		}
	}

	if _, ok := excludeSet[volumeID]; ok {
		return false
	}

	return true
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
	sourceClusterSecret string,
	sourceClusterUUID string,
	sourcePoolUUID string,
	sourceLvolUUID string,
	targetClusterSecret string,
	targetClusterUUID string,
	targetPoolUUID string,
	targetLvol *utils.Lvol,
) error {
	if err := triggerReplication(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, targetLvol.UUID); err != nil {
		return fmt.Errorf("target trigger replication failed for lvol %s: %w", targetLvol.UUID, err)
	}

	if err := waitForReplicationTaskCompletion(
		ctx,
		apiClient,
		targetClusterSecret,
		targetClusterUUID,
		targetPoolUUID,
		targetLvol.UUID,
		10*time.Minute,
		5*time.Second,
	); err != nil {
		return fmt.Errorf("waiting for first target replication task failed for lvol %s: %w", targetLvol.UUID, err)
	}

	if err := suspendLvol(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, targetLvol.UUID); err != nil {
		return fmt.Errorf("suspend target lvol failed for lvol %s: %w", targetLvol.UUID, err)
	}

	if err := triggerReplication(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, targetLvol.UUID); err != nil {
		return fmt.Errorf("second target trigger replication failed for lvol %s: %w", targetLvol.UUID, err)
	}

	if err := waitForReplicationTaskCompletion(
		ctx,
		apiClient,
		targetClusterSecret,
		targetClusterUUID,
		targetPoolUUID,
		targetLvol.UUID,
		10*time.Minute,
		5*time.Second,
	); err != nil {
		return fmt.Errorf("waiting for second target replication task failed for lvol %s: %w", targetLvol.UUID, err)
	}

	if err := deleteLvol(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID, targetLvol.UUID); err != nil {
		return fmt.Errorf("delete target lvol failed for lvol %s: %w", targetLvol.UUID, err)
	}

	if err := replicateLvolOnSourceCluster(
		ctx,
		apiClient,
		sourceClusterSecret,
		sourceClusterUUID,
		sourcePoolUUID,
		sourceLvolUUID,
	); err != nil {
		return fmt.Errorf("replicate lvol on source cluster failed for source lvol %s: %w", sourceLvolUUID, err)
	}
	return nil
}

func suspendLvol(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/suspend/",
		clusterUUID,
		poolUUID,
		lvolUUID,
	)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("failed to suspend lvol %s, status %d: %v, body: %s", lvolUUID, status, err, string(body))
	}
	return nil
}

func deleteLvol(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/",
		clusterUUID,
		poolUUID,
		lvolUUID,
	)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("failed to delete lvol %s, status %d: %v, body: %s", lvolUUID, status, err, string(body))
	}
	return nil
}

func replicateLvolOnSourceCluster(
	ctx context.Context,
	apiClient *webapi.Client,
	sourceClusterSecret string,
	sourceClusterUUID string,
	sourcePoolUUID string,
	sourceLvolUUID string,
) error {

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replicate_lvol_on_source_cluster/",
		sourceClusterUUID,
		sourcePoolUUID,
		sourceLvolUUID,
	)
	body, status, err := apiClient.Do(ctx, sourceClusterSecret, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		return fmt.Errorf("failed to start replication for lvol %s, status %d: %v, body: %s", sourceLvolUUID, status, err, string(body))
	}
	return nil
}

func waitForReplicationTaskCompletion(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolUUID string,
	timeout time.Duration,
	pollInterval time.Duration,
) error {
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		done, task, err := utils.GetLastSnapshotTaskDoneStatus(
			ctx,
			apiClient,
			clusterSecret,
			clusterUUID,
			poolUUID,
			lvolUUID,
		)
		if err != nil {
			return fmt.Errorf("failed to get replication task status for lvol %s: %w", lvolUUID, err)
		}

		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeoutTimer.C:
			return fmt.Errorf(
				"timed out waiting for replication task completion for lvol %s (taskID=%s status=%s)",
				lvolUUID,
				task.UUID,
				task.Status,
			)
		case <-ticker.C:
		}
	}
}

func findSourceLvolForFailback(
	ctx context.Context,
	apiClient *webapi.Client,
	sourceClusterSecret string,
	sourceClusterUUID string,
	targetLvol *utils.Lvol,
) (string, string, error) {
	filterID := failbackFilterID(targetLvol)

	poolUUIDs, err := utils.GetPoolUUIDs(ctx, apiClient, sourceClusterSecret, sourceClusterUUID)
	if err != nil {
		return "", "", fmt.Errorf("failed to list source pools: %w", err)
	}

	for _, poolUUID := range poolUUIDs {
		lvols, err := utils.GetLvols(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID)
		if err != nil {
			continue
		}

		for _, lvolSummary := range lvols {
			lvolDetail, err := utils.GetLvol(ctx, apiClient, sourceClusterSecret, sourceClusterUUID, poolUUID, lvolSummary.UUID)
			if err != nil {
				continue
			}

			if failbackFilterID(lvolDetail) == filterID {
				return poolUUID, lvolDetail.UUID, nil
			}
		}
	}

	return "", "", fmt.Errorf("source lvol not found for target lvol %s (filterID=%s)", targetLvol.UUID, filterID)
}
