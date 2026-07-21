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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// checkNodeInfoReachableFn indirects checkNodeInfoReachable so the migrate flow
// can be exercised in tests without a live storage-node-api endpoint.
var checkNodeInfoReachableFn = checkNodeInfoReachable

const (
	// migrateRequeue is the poll cadence while preparing / awaiting online / retrying.
	migrateRequeue = 5 * time.Second
	// migrateStepRequeue is a brief hop used between phases that must persist a
	// durable state change before proceeding.
	migrateStepRequeue = 2 * time.Second
	// migrateOverallDeadline bounds the whole migration measured from StartedAt, so a
	// target that never becomes reachable / online, or a promote that keeps failing
	// transiently, fails terminally instead of looping forever.
	migrateOverallDeadline = 10 * time.Minute
)

// Values written to the presence-based migration annotations. Only presence is
// checked by the reconcilers; the values are descriptive.
const (
	migrationPendingValue = "pending"
	migratedAwayValue     = "migrated"
)

// runMigrate drives action=migrate: relocating a backend storage node to a
// different worker host while keeping its UUID and data. Non-blocking phase
// machine (a single step + RequeueAfter per reconcile):
//
//	Preparing   validate target; create the adopted, provisioning-suppressed target
//	            StorageNode CR (so the set controller labels the host and adds it to
//	            the storage-node-api EndpointSlice); wait until the target is reachable
//	Restarting  POST /restart with node_address = target host (storm-guarded); await online
//	Promoting   POST /promote (activate new-host devices, fail+migrate origin devices —
//	            starts the rebalance, set primary, re-home lvols)
//	Rebinding   clear migration-pending on the target CR; mark the origin CR
//	            migrated-away (persisted BEFORE the spec swap); release the lock; swap
//	            spec.workerNodes (drop origin, add target) + move nodeConfigs; succeed.
func (r *StorageNodeOpsReconciler) runMigrate(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	switch ops.Status.SubPhase {
	case simplyblockv1alpha1.StorageNodeOpsSubPhasePreparing:
		return r.migratePrepare(ctx, ops, sn, sns, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseRestarting:
		return r.migrateRestart(ctx, ops, sn, clusterUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhasePromoting:
		return r.migratePromote(ctx, ops, sn, clusterUUID, apiClient)
	case simplyblockv1alpha1.StorageNodeOpsSubPhaseRebinding:
		return r.migrateRebind(ctx, ops, sn, sns)
	default:
		return r.failOps(ctx, ops, fmt.Sprintf("unknown migrate sub-phase %q", ops.Status.SubPhase))
	}
}

// migratePrepare validates the request, creates the adopted target StorageNode CR,
// and waits until the target worker's storage-node-api is reachable.
func (r *StorageNodeOpsReconciler) migratePrepare(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	_ *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	target := ops.Spec.TargetWorkerNode

	if target == "" {
		return r.failOps(ctx, ops, "action=migrate requires spec.targetWorkerNode")
	}
	if sn.Status.UUID == "" {
		return r.failOps(ctx, ops, "cannot migrate a storage node that has no backend UUID yet")
	}
	if target == sn.Spec.WorkerNode {
		return r.failOps(ctx, ops, fmt.Sprintf("target worker %q is already this node's host", target))
	}

	// Target must be a real k8s node.
	var targetNode corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: target}, &targetNode); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOps(ctx, ops, fmt.Sprintf("target worker node %q not found in the cluster", target))
		}
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	}

	// Ensure the adopted, provisioning-suppressed target CR exists (idempotent).
	targetName := storageNodeCRName(sns.Name, target, sn.Spec.SocketID, storageNodeNodeIndex(sn))
	var existing simplyblockv1alpha1.StorageNode
	err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: ops.Namespace}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		newCR := buildMigrationTargetCR(sns, sn, target, targetName)
		if createErr := r.Create(ctx, newCR); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			log.Error(createErr, "migrate: failed to create target StorageNode CR", "name", targetName)
			return ctrl.Result{RequeueAfter: migrateRequeue}, nil
		}
		log.Info("migrate: created adopted target StorageNode CR",
			"name", targetName, "target", target, "uuid", sn.Status.UUID)
		// Give the set controller a chance to label the target and add it to the
		// EndpointSlice before we probe reachability.
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	case err != nil:
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	default:
		// A CR already occupies this (worker, socket, ordinal). It must be OUR
		// migration CR (carrying the adopt annotation for this UUID); otherwise the
		// target already hosts a storage node at that slot.
		if existing.Annotations[simplyblockv1alpha1.AnnotationAdoptUUID] != sn.Status.UUID {
			return r.failOps(ctx, ops, fmt.Sprintf(
				"target worker %q already hosts a storage node at socket %q index %d (CR %s)",
				target, sn.Spec.SocketID, storageNodeNodeIndex(sn), targetName))
		}
	}

	// Wait for the target's storage-node-api to answer. The set controller unions
	// manual StorageNode CRs into labelWorkerNodes + the EndpointSlice, so the target
	// pod is scheduled and its per-pod DNS resolves once a set reconcile has run.
	if err := checkNodeInfoReachableFn(ctx, target, ops.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
		if migrateDeadlineExceeded(ops) {
			return r.failOps(ctx, ops, fmt.Sprintf("target worker %q never became reachable: %v", target, err))
		}
		log.Info("migrate: target not yet reachable, requeuing", "target", target)
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	}

	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseRestarting)
}

// migrateRestart fires (once) the control-plane restart directed at the target host
// and polls until the node is back online.
func (r *StorageNodeOpsReconciler) migrateRestart(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	target := ops.Spec.TargetWorkerNode
	nodeUUID := sn.Status.UUID

	if !ops.Status.Triggered {
		// Storm guard: don't re-fire a restart the backend already has underway
		// (mirrors nodedrain_controller). Best-effort read; on error we fire.
		if cur, statErr := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID); statErr == nil && cur == utils.NodeStatusInRestart {
			log.Info("migrate: node already in_restart; not re-firing", "uuid", nodeUUID)
		} else {
			body := map[string]any{
				"force":           ops.Spec.Force != nil && *ops.Spec.Force,
				"reattach_volume": utils.BoolPtrOrFalse(ops.Spec.ReattachVolume),
				"new_ssd_pcie":    migrateNewSsdPcie(ops),
				"node_address":    utils.StorageNodeSetAPIAddress(target, ops.Namespace),
			}
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/restart", clusterUUID, nodeUUID)
			respBody, status, doErr := apiClient.Do(ctx, http.MethodPost, endpoint, body)
			if doErr != nil || status >= 300 {
				if webapi.ClassifyError(doErr, status).Retryable && !migrateDeadlineExceeded(ops) {
					log.Error(doErr, "migrate: restart POST failed, retrying", "status", status)
					return ctrl.Result{RequeueAfter: migrateRequeue}, nil
				}
				return r.failOps(ctx, ops, fmt.Sprintf(
					"restart to target %q rejected (status %d): %s", target, status, string(respBody)))
			}
		}
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Triggered = true
		ops.Status.Message = fmt.Sprintf("restart to %s sent, waiting for node online", target)
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	}

	cur, err := getNodeBackendStatus(ctx, apiClient, clusterUUID, nodeUUID)
	if err != nil || cur != utils.NodeStatusOnline {
		if migrateDeadlineExceeded(ops) {
			return r.failOps(ctx, ops, fmt.Sprintf(
				"node did not reach online after restart to %s (last status %q, err %v)", target, cur, err))
		}
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	}
	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhasePromoting)
}

// migratePromote calls /promote once (guarded by Triggered so a crash after the POST
// does not re-promote) and advances to Rebinding.
func (r *StorageNodeOpsReconciler) migratePromote(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if ops.Status.Triggered {
		// promote already issued in a prior reconcile — do not re-POST.
		return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseRebinding)
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/promote", clusterUUID, sn.Status.UUID)
	respBody, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		if webapi.ClassifyError(err, status).Retryable && !migrateDeadlineExceeded(ops) {
			log.Error(err, "migrate: promote failed, retrying", "status", status)
			return ctrl.Result{RequeueAfter: migrateRequeue}, nil
		}
		return r.failOps(ctx, ops, fmt.Sprintf("promote rejected (status %d): %s", status, string(respBody)))
	}

	log.Info("migrate: promoted relocated node; rebalance started",
		"uuid", sn.Status.UUID, "target", ops.Spec.TargetWorkerNode)
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Triggered = true
	ops.Status.Message = "promote sent; rebinding"
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: migrateStepRequeue}, nil
}

// migrateRebind performs the crash-safe k8s handoff. Ordering is load-bearing:
// migrated-away must be durable on the origin CR BEFORE spec.workerNodes drops the
// origin (else the set controller GCs it and its deletion drains+removes the — now
// relocated — backend node).
func (r *StorageNodeOpsReconciler) migrateRebind(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	target := ops.Spec.TargetWorkerNode
	origin := sn.Spec.WorkerNode

	// 1. Ensure the target CR exists and clear its migration-pending marker so it
	//    resumes normal status sync. (Recreate defensively if it vanished.)
	targetName := storageNodeCRName(sns.Name, target, sn.Spec.SocketID, storageNodeNodeIndex(sn))
	var targetCR simplyblockv1alpha1.StorageNode
	switch err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: ops.Namespace}, &targetCR); {
	case apierrors.IsNotFound(err):
		if createErr := r.Create(ctx, buildMigrationTargetCR(sns, sn, target, targetName)); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			log.Error(createErr, "migrate: failed to recreate target CR during rebind")
		}
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	case err != nil:
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	default:
		if _, pending := targetCR.Annotations[simplyblockv1alpha1.AnnotationMigrationPending]; pending {
			patch := client.MergeFrom(targetCR.DeepCopy())
			delete(targetCR.Annotations, simplyblockv1alpha1.AnnotationMigrationPending)
			if err := r.Patch(ctx, &targetCR, patch); err != nil {
				log.Error(err, "migrate: failed to clear migration-pending, retrying")
				return ctrl.Result{RequeueAfter: migrateRequeue}, nil
			}
		}
	}

	// 2. Mark the origin CR migrated-away and PERSIST it before touching the spec.
	if _, migrated := sn.Annotations[simplyblockv1alpha1.AnnotationMigratedAway]; !migrated {
		patch := client.MergeFrom(sn.DeepCopy())
		if sn.Annotations == nil {
			sn.Annotations = map[string]string{}
		}
		sn.Annotations[simplyblockv1alpha1.AnnotationMigratedAway] = migratedAwayValue
		if err := r.Patch(ctx, sn, patch); err != nil {
			log.Error(err, "migrate: failed to annotate origin CR migrated-away, retrying")
			return ctrl.Result{RequeueAfter: migrateRequeue}, nil
		}
		// Re-enter so the annotation is observably durable before the spec swap.
		return ctrl.Result{RequeueAfter: migrateStepRequeue}, nil
	}

	// 3. Release the ops lock on the origin (before the spec swap that triggers its
	//    GC, so handleDeletion's finalizer removal isn't blocked on activeOpsRef).
	if err := r.releaseLock(ctx, sn, ops.Name); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "migrate: failed to release lock on origin, retrying")
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	}

	// 4. Swap spec.workerNodes (drop origin, add target) + move nodeConfigs. Re-fetch
	//    to patch against the current version.
	var freshSNS simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{Name: sns.Name, Namespace: sns.Namespace}, &freshSNS); err != nil {
		return ctrl.Result{RequeueAfter: migrateRequeue}, nil
	}
	base := client.MergeFrom(freshSNS.DeepCopy())
	if migrateSwapWorkerList(&freshSNS, origin, target) {
		if err := r.Patch(ctx, &freshSNS, base); err != nil {
			log.Error(err, "migrate: failed to swap workerNodes, retrying")
			return ctrl.Result{RequeueAfter: migrateRequeue}, nil
		}
		log.Info("migrate: swapped worker list",
			"removed", origin, "added", target, "workerNodes", freshSNS.Spec.WorkerNodes)
	}

	res, err := r.succeedOps(ctx, ops, sn)
	if err == nil {
		log.Info("migrate: migration completed", "uuid", sn.Status.UUID, "from", origin, "to", target)
	}
	return res, err
}

// migrateDeadlineExceeded reports whether the whole migration has run past its
// overall deadline, measured from ops.Status.StartedAt.
func migrateDeadlineExceeded(ops *simplyblockv1alpha1.StorageNodeOps) bool {
	return ops.Status.StartedAt != nil && time.Since(ops.Status.StartedAt.Time) > migrateOverallDeadline
}

// migrateNewSsdPcie returns spec.newSsdPcie normalised to a non-nil slice so it
// serialises as a JSON array ([]) rather than null.
func migrateNewSsdPcie(ops *simplyblockv1alpha1.StorageNodeOps) []string {
	if ops.Spec.NewSsdPcie == nil {
		return []string{}
	}
	return ops.Spec.NewSsdPcie
}

// storageNodeNodeIndex returns the per-socket node index of a StorageNode (0 when unset).
func storageNodeNodeIndex(sn *simplyblockv1alpha1.StorageNode) int {
	if sn.Spec.NodeIndex != nil {
		return int(*sn.Spec.NodeIndex)
	}
	return 0
}

// storageNodeOrdinal returns the global ordinal (SocketIndex) of a StorageNode (0 when unset).
func storageNodeOrdinal(sn *simplyblockv1alpha1.StorageNode) int {
	if sn.Spec.SocketIndex != nil {
		return int(*sn.Spec.SocketIndex)
	}
	return 0
}

// buildMigrationTargetCR constructs the target-host StorageNode CR for a migration:
// same set/socket/ordinal/overrides as the origin, bound to the target worker, with
// no ownerRef (so the set controller never GCs it) and birth-time annotations that
// make the StorageNodeReconciler adopt the relocated backend UUID instead of POSTing
// a fresh node-add, and suppress provisioning until the migrate op clears them.
func buildMigrationTargetCR(
	sns *simplyblockv1alpha1.StorageNodeSet,
	origin *simplyblockv1alpha1.StorageNode,
	target, name string,
) *simplyblockv1alpha1.StorageNode {
	cr := buildStorageNodeCR(sns, name, target, origin.Spec.SocketID, storageNodeNodeIndex(origin), storageNodeOrdinal(origin))
	cr.Spec.Overrides = origin.Spec.Overrides
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[simplyblockv1alpha1.AnnotationAdoptUUID] = origin.Status.UUID
	cr.Annotations[simplyblockv1alpha1.AnnotationMigrationPending] = migrationPendingValue
	return cr
}

// migrateSwapWorkerList mutates sns.Spec in place to reflect a completed migration:
// removes the origin worker and adds the target in spec.workerNodes, and moves
// spec.nodeConfigs[origin] to [target]. Returns true when anything changed.
func migrateSwapWorkerList(sns *simplyblockv1alpha1.StorageNodeSet, origin, target string) bool {
	changed := false

	out := make([]string, 0, len(sns.Spec.WorkerNodes)+1)
	for _, w := range sns.Spec.WorkerNodes {
		if w == origin {
			changed = true
			continue
		}
		out = append(out, w)
	}
	if !slices.Contains(out, target) {
		out = append(out, target)
		changed = true
	}
	if changed {
		sns.Spec.WorkerNodes = out
	}

	if sns.Spec.NodeConfigs != nil {
		if cfg, ok := sns.Spec.NodeConfigs[origin]; ok {
			if _, exists := sns.Spec.NodeConfigs[target]; !exists {
				sns.Spec.NodeConfigs[target] = cfg
			}
			delete(sns.Spec.NodeConfigs, origin)
			changed = true
		}
	}

	return changed
}
