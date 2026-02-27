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

	ids, err := buildTargetLvolIDSet(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
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

	if !shouldReplicate(lvolDetail, interval, now) {
		log.Info("Skipping replication (interval not reached)",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
			"lastSnapshot", lvolDetail.RepInfo.LastSnapshotUUID,
			"intervalSec", interval,
		)
		return
	}

	if err := startReplication(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, lvolDetail.UUID); err != nil {
		log.Error(err, "Failed to start replication",
			"lvol", lvolDetail.Name,
			"uuid", lvolDetail.UUID,
		)
		return
	}

	log.Info("Replication started for lvol",
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

func startReplication(ctx context.Context, apiClient *webapi.Client, clusterSecret, clusterUUID, poolUUID, lvolUUID string) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication_start/",
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

func buildTargetLvolIDSet(
	ctx context.Context,
	apiClient *webapi.Client,
	targetClusterSecret string,
	targetClusterUUID string,
	targetPoolUUID string,
) (map[string]struct{}, error) {
	log := logf.FromContext(ctx)

	targetLvols, err := utils.GetLvols(ctx, apiClient, targetClusterSecret, targetClusterUUID, targetPoolUUID)
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

	log.Info("Built target lvol ID set",
		"targetClusterUUID", targetClusterUUID,
		"targetPoolUUID", targetPoolUUID,
		"count", len(ids),
	)

	return ids, nil
}
