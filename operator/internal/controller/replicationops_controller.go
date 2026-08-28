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
	"strings"
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

const (
	finalizerReplicationOps = "storage.simplyblock.io/replicationops-finalizer"
)

// ReplicationOpsReconciler reconciles ReplicationOps resources.
// It drives one-shot user-triggered failover and failback operations to completion.
type ReplicationOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ReplicationOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ops simplyblockv1alpha1.ReplicationOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiClient := webapi.NewClient()

	if !ops.DeletionTimestamp.IsZero() {
		r.releasePolicyLock(ctx, &ops)
		controllerutil.RemoveFinalizer(&ops, finalizerReplicationOps)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	if !controllerutil.ContainsFinalizer(&ops, finalizerReplicationOps) {
		controllerutil.AddFinalizer(&ops, finalizerReplicationOps)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	switch ops.Status.Phase {
	case string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded),
		string(simplyblockv1alpha1.ReplicationOpsPhaseFailed):
		r.releaseLock(ctx, &ops)
		if controllerutil.ContainsFinalizer(&ops, finalizerReplicationOps) {
			controllerutil.RemoveFinalizer(&ops, finalizerReplicationOps)
			return ctrl.Result{}, r.Update(ctx, &ops)
		}
		return ctrl.Result{}, nil
	}

	// scope=target uses a pair-level lock and collects slots across all policies
	// on the pair. All other scopes use a policy-level lock.
	if ops.Spec.Scope == utils.ReplicationOpsScopeTarget {
		return r.reconcileTargetScope(ctx, &ops, apiClient)
	}

	policyName, err := r.resolveAffectedPolicyName(ctx, &ops)
	if err != nil {
		return r.failOps(ctx, &ops, err.Error())
	}
	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ops.Namespace}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOps(ctx, &ops, fmt.Sprintf("ReplicationPolicy %q not found", policyName))
		}
		return ctrl.Result{}, fmt.Errorf("get ReplicationPolicy %q: %w", policyName, err)
	}

	// Mutual exclusion: only one active ReplicationOps per policy at a time.
	if policy.Status.ActiveOpsRef != "" && policy.Status.ActiveOpsRef != ops.Name {
		log.Info("Another ReplicationOps is active for this policy; waiting",
			"ops", ops.Name, "activeOps", policy.Status.ActiveOpsRef)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if policy.Status.ActiveOpsRef != ops.Name {
		base := policy.DeepCopy()
		policy.Status.ActiveOpsRef = ops.Name
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, &policy, patch); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("set activeOpsRef: %w", err)
		}
	}

	if ops.Status.Phase == "" || ops.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhasePending) {
		now := metav1.Now()
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseRunning)
		ops.Status.StartedAt = &now
		if err := r.Status().Patch(ctx, &ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	switch ops.Spec.Action {
	case "failover":
		return r.reconcileFailover(ctx, &ops, &policy, apiClient)
	case "failback":
		return r.reconcileFailback(ctx, &ops, &policy, apiClient)
	case "migration":
		return r.reconcileMigration(ctx, &ops, &policy, apiClient)
	default:
		return r.failOps(ctx, &ops, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
}

// reconcileTargetScope handles scope=target: locks at the ReplicationPair level
// and drives the action across all slots from all policies that reference the pair.
func (r *ReplicationOpsReconciler) reconcileTargetScope(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.Ref, Namespace: ops.Namespace}, &pair); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOps(ctx, ops, fmt.Sprintf("ReplicationPair %q not found", ops.Spec.Ref))
		}
		return ctrl.Result{}, fmt.Errorf("get ReplicationPair %q: %w", ops.Spec.Ref, err)
	}

	// Mutual exclusion: only one scope=target ReplicationOps active per pair.
	if pair.Status.ActiveOpsRef != "" && pair.Status.ActiveOpsRef != ops.Name {
		log.Info("Another ReplicationOps holds the pair lock; waiting",
			"ops", ops.Name, "activeOps", pair.Status.ActiveOpsRef, "pair", pair.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if pair.Status.ActiveOpsRef != ops.Name {
		base := pair.DeepCopy()
		pair.Status.ActiveOpsRef = ops.Name
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, &pair, patch); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("set pair activeOpsRef: %w", err)
		}
	}

	if ops.Status.Phase == "" || ops.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhasePending) {
		now := metav1.Now()
		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseRunning)
		ops.Status.StartedAt = &now
		if err := r.Status().Patch(ctx, ops, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	switch ops.Spec.Action {
	case "failover":
		return r.reconcileTargetFailover(ctx, ops, &pair, apiClient)
	case "failback":
		return r.failOps(ctx, ops, "scope=target failback is not supported; use scope=policy")
	default:
		return r.failOps(ctx, ops, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
}

// reconcileTargetFailover fails over all slots across every policy on the pair.
func (r *ReplicationOpsReconciler) reconcileTargetFailover(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, ops.Namespace, pair.Spec.SourceCluster)
	if err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	slots, err := r.collectSlotsForPair(ctx, pair.Name, ops.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.setSubphase(ctx, ops, "TriggeringTargetFailover")

	type backendLvolResult struct {
		LvolID     string `json:"lvol_id"`
		Status     string `json:"status"`
		Detail     string `json:"detail"`
		TargetLvol string `json:"target_lvol_id"`
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/targets/%s/failover", clusterUUID, pair.Status.BackendTargetID)
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "POST target failover failed")
		return r.failOps(ctx, ops, fmt.Sprintf("target failover failed: %v", err))
	}

	var backendResults []backendLvolResult
	_ = json.Unmarshal(body, &backendResults)

	var failures []string
	for _, br := range backendResults {
		if br.Status == "failed" {
			failures = append(failures, fmt.Sprintf("%s: %s", br.LvolID, br.Detail))
		}
	}
	if len(failures) > 0 {
		return r.failOps(ctx, ops, fmt.Sprintf("failover failed for %d volume(s): %s",
			len(failures), strings.Join(failures, "; ")))
	}

	backendByLvol := make(map[string]backendLvolResult, len(backendResults))
	for _, br := range backendResults {
		backendByLvol[br.LvolID] = br
	}

	r.setSubphase(ctx, ops, "UpdatingSlotStatuses")

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(slots))
	for i := range slots {
		slot := &slots[i]
		slotPatch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		slot.Status.Message = fmt.Sprintf("Failed over via ReplicationOps %s (scope=target)", ops.Name)
		if br, ok := backendByLvol[slot.Spec.VolumeID]; ok && br.TargetLvol != "" {
			slot.Status.TargetLvolID = br.TargetLvol
		}
		if err := r.Status().Patch(ctx, slot, slotPatch); err != nil {
			log.Error(err, "failed to update slot status after target failover", "slot", slot.Name)
		}
		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			SlotRef:      slot.Name,
			Status:       string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
			TargetLvolID: slot.Status.TargetLvolID,
		})
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailoverSucceeded", "FailoverSucceeded",
		"Target failover completed for pair %s (%d volumes across all policies)", pair.Name, len(slots))
	return r.succeedOps(ctx, ops, "Target failover completed successfully", results)
}

// reconcileFailover drives a failover to completion.
func (r *ReplicationOpsReconciler) reconcileFailover(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Resolve the pair and source cluster UUID — needed for all backend failover endpoints.
	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Spec.PairRef, Namespace: policy.Namespace}, &pair); err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("get ReplicationPair %q: %v", policy.Spec.PairRef, err))
	}
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, policy.Namespace, pair.Spec.SourceCluster)
	if err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	slots, err := r.collectAffectedSlots(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.setSubphase(ctx, ops, "TriggeringFailover")

	// backendLvolResults holds per-volume results returned by the backend
	// failover endpoint. The backend always returns HTTP 200; actual failures
	// are encoded in the status field of each result entry.
	type backendLvolResult struct {
		LvolID     string `json:"lvol_id"`
		Status     string `json:"status"` // "failed_over" | "failed" | "skipped"
		Detail     string `json:"detail"`
		TargetLvol string `json:"target_lvol_id"`
	}

	var backendResults []backendLvolResult

	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopePolicy:
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/policies/%s/failover", clusterUUID, policy.Status.BackendPolicyID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST policy failover failed")
			return r.failOps(ctx, ops, fmt.Sprintf("policy failover failed: %v", err))
		}
		_ = json.Unmarshal(body, &backendResults)

		// Backend always returns 200; check per-volume status for failures.
		var failures []string
		for _, r := range backendResults {
			if r.Status == "failed" {
				failures = append(failures, fmt.Sprintf("%s: %s", r.LvolID, r.Detail))
			}
		}
		if len(failures) > 0 {
			return r.failOps(ctx, ops, fmt.Sprintf("failover failed for %d volume(s): %s",
				len(failures), strings.Join(failures, "; ")))
		}

	case utils.ReplicationOpsScopeVolume:
		if len(slots) != 1 {
			return r.failOps(ctx, ops, fmt.Sprintf("scope=volume requires exactly 1 slot; got %d", len(slots)))
		}
		slot := &slots[0]
		clusterID, poolID, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID)
		if !ok {
			return r.failOps(ctx, ops, fmt.Sprintf("invalid VolumeID on slot %s", slot.Name))
		}
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/failover",
			clusterID, poolID, volumeID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST replication/failover failed", "slot", slot.Name)
			return r.failOps(ctx, ops, fmt.Sprintf("volume failover failed: %v", err))
		}

	default:
		return r.failOps(ctx, ops, fmt.Sprintf("unknown scope %q", ops.Spec.Scope))
	}

	// Build a map of backend results by lvol ID for quick lookup.
	backendByLvol := make(map[string]backendLvolResult, len(backendResults))
	for _, br := range backendResults {
		backendByLvol[br.LvolID] = br
	}

	r.setSubphase(ctx, ops, "UpdatingSlotStatuses")

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(slots))
	for i := range slots {
		slot := &slots[i]
		slotPatch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		slot.Status.Message = fmt.Sprintf("Failed over via ReplicationOps %s", ops.Name)
		// Populate TargetLvolID from the backend response when available.
		if br, ok := backendByLvol[slot.Spec.VolumeID]; ok && br.TargetLvol != "" {
			slot.Status.TargetLvolID = br.TargetLvol
		}
		if err := r.Status().Patch(ctx, slot, slotPatch); err != nil {
			log.Error(err, "failed to update slot status after failover", "slot", slot.Name)
		}
		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			SlotRef:      slot.Name,
			Status:       string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
			TargetLvolID: slot.Status.TargetLvolID,
		})
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailoverSucceeded", "FailoverSucceeded",
		"Failover completed for policy %s (%d volumes)", policy.Name, len(slots))
	return r.succeedOps(ctx, ops, "Failover completed successfully", results)
}

// reconcileFailback drives a live failback to completion.
//
// After failover the active volume lives on the target cluster. The two-step
// sequence mirrors how migration works:
//  1. POST .../replication/failback — syncs the final snapshot from target back
//     to the source volume.
//  2. POST .../replication/commit — queues an async backend job that fetches
//     the source connection string and switches ANA so the source becomes the
//     active path again (202 Accepted; the job runs asynchronously).
//
// Phases:
//   - CommitPhase (subphase != "WaitingForSlots"): POST failback + commit per
//     slot, mark slots cutover_pending, advance subphase to WaitingForSlots.
//   - WaitPhase (subphase == "WaitingForSlots"): poll slots until every slot
//     is replicating/source, then succeed. This means Succeeded only appears
//     once the ANA flip and UUID swap are complete end-to-end.
//
// No source cluster shutdown is required.
func (r *ReplicationOpsReconciler) reconcileFailback(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if ops.Status.Subphase == "WaitingForSlots" {
		return r.reconcileFailbackWait(ctx, ops, policy)
	}

	slots, err := r.collectAffectedSlots(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(slots))
	anyFailed := false

	// replicationRelationship holds the fields we need from
	// GET /api/v2/clusters/{c}/replication/relationships/{vol}
	type replicationRelationship struct {
		TargetLvolID    string `json:"target_lvol_id"`
		TargetClusterID string `json:"target_cluster_id"`
		TargetPoolID    string `json:"target_pool_id"`
	}

	for i := range slots {
		slot := &slots[i]
		clusterID, _, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID)
		if !ok {
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: "invalid VolumeID",
			})
			anyFailed = true
			continue
		}

		// Resolve the target cluster, pool, and volume via the replication
		// relationship. After failover the active volume is on the target cluster;
		// the failback and commit endpoints must be called there, not on the source.
		relEndpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/relationships/%s", clusterID, volumeID)
		relBody, relStatus, relErr := apiClient.Do(ctx, http.MethodGet, relEndpoint, nil)
		if relErr != nil || relStatus >= 300 {
			if relErr == nil {
				relErr = fmt.Errorf("status %d: %s", relStatus, string(relBody))
			}
			log.Error(relErr, "fetch replication relationship failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: relErr.Error(),
			})
			anyFailed = true
			continue
		}
		var rel replicationRelationship
		if err := json.Unmarshal(relBody, &rel); err != nil {
			log.Error(err, "parse replication relationship failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: fmt.Sprintf("parse relationship: %v", err),
			})
			anyFailed = true
			continue
		}
		if rel.TargetLvolID == "" || rel.TargetClusterID == "" || rel.TargetPoolID == "" {
			detail := fmt.Sprintf("incomplete relationship: target_lvol=%q target_cluster=%q target_pool=%q",
				rel.TargetLvolID, rel.TargetClusterID, rel.TargetPoolID)
			log.Error(nil, detail, "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: detail,
			})
			anyFailed = true
			continue
		}

		r.setSubphase(ctx, ops, "StartingFailback")

		// Step 1: sync the final snapshot from the target back to the source volume.
		fbBody := map[string]interface{}{}
		if ops.Spec.SourceClusterID != "" {
			fbBody["source_cluster_id"] = ops.Spec.SourceClusterID
		}
		fbEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/failback",
			rel.TargetClusterID, rel.TargetPoolID, rel.TargetLvolID)
		body, status, fbErr := apiClient.Do(ctx, http.MethodPost, fbEndpoint, fbBody)
		if fbErr != nil || status >= 300 {
			if fbErr == nil {
				fbErr = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(fbErr, "replication/failback failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: fbErr.Error(),
			})
			anyFailed = true
			continue
		}

		r.setSubphase(ctx, ops, "CommittingFailback")

		// Step 2: queue the async commit job that fetches the source connection
		// string and switches ANA to make the source the active path again.
		// Accepts 202 — the actual ANA switch happens asynchronously on the backend.
		commitEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/commit",
			rel.TargetClusterID, rel.TargetPoolID, rel.TargetLvolID)
		var commitBody map[string]interface{}
		if ops.Spec.DeleteSource {
			commitBody = map[string]interface{}{"delete_source": true}
		}
		_, status, commitErr := apiClient.Do(ctx, http.MethodPost, commitEndpoint, commitBody)
		if commitErr != nil || (status != 202 && status >= 300) {
			if commitErr == nil {
				commitErr = fmt.Errorf("status %d", status)
			}
			log.Error(commitErr, "replication/commit (failback) failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: commitErr.Error(),
			})
			anyFailed = true
			continue
		}

		// Commit accepted — mark the slot cutover_pending so the slot controller
		// begins polling. Store the target volume handle so reconcileCutoverPending
		// polls the target cluster (where the task runs) rather than the source
		// cluster (which only sees the old failed_over state).
		metaPatch := client.MergeFrom(slot.DeepCopy())
		if slot.Annotations == nil {
			slot.Annotations = make(map[string]string)
		}
		slot.Annotations[annotFailbackTarget] = fmt.Sprintf("%s:%s:%s",
			rel.TargetClusterID, rel.TargetPoolID, rel.TargetLvolID)
		if err := r.Patch(ctx, slot, metaPatch); err != nil {
			log.Error(err, "failed to set failback-target annotation", "slot", slot.Name)
		}

		slotPatch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending)
		slot.Status.Message = fmt.Sprintf("Failback commit queued via ReplicationOps %s", ops.Name)
		if err := r.Status().Patch(ctx, slot, slotPatch); err != nil {
			log.Error(err, "failed to update slot status to cutover_pending", "slot", slot.Name)
		}

		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
		})
	}

	if anyFailed {
		return r.failOps(ctx, ops, "Failback failed for one or more volumes; see results for details")
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailbackCommitQueued", "FailbackCommitQueued",
		"Failback commit queued for policy %s (%d volumes); waiting for slots to reach replicating/source",
		policy.Name, len(slots))
	r.setSubphase(ctx, ops, "WaitingForSlots")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileFailbackWait polls slots until every one is replicating/source, then
// marks the ReplicationOps Succeeded. It is entered on every reconcile when
// subphase == "WaitingForSlots".
func (r *ReplicationOpsReconciler) reconcileFailbackWait(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	slots, err := r.collectAffectedSlots(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(slots))
	allDone := true
	for i := range slots {
		slot := &slots[i]
		if slot.Status.State == string(simplyblockv1alpha1.ReplicationSlotStateReplicating) &&
			slot.Status.Direction == string(simplyblockv1alpha1.ReplicationSlotDirectionSource) {
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name,
				Status:  string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
			})
		} else {
			allDone = false
		}
	}

	if !allDone {
		log.Info("Failback: waiting for slots to reach replicating/source",
			"ops", ops.Name, "total", len(slots), "done", len(results))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailbackComplete", "FailbackComplete",
		"Failback complete for policy %s (%d volumes); all slots replicating/source",
		policy.Name, len(slots))
	return r.succeedOps(ctx, ops, "Failback complete; all volumes are replicating/source", results)
}

// reconcileMigration drives a planned online cutover (mode=migration).
// For each slot it calls POST .../replication/commit which queues an async
// task on the backend. The backend transitions the slot through:
//
//	replicating → cutover_pending (both paths served) → cutover_done (target only).
//
// The ReplicationOps succeeds as soon as all commit calls are accepted (202).
// The slot state transitions to cutover_done asynchronously; the test script
// should wait on slot state independently of the ops phase.
func (r *ReplicationOpsReconciler) reconcileMigration(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Spec.PairRef, Namespace: policy.Namespace}, &pair); err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("get ReplicationPair %q: %v", policy.Spec.PairRef, err))
	}
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, policy.Namespace, pair.Spec.SourceCluster)
	if err != nil {
		return r.failOps(ctx, ops, fmt.Sprintf("resolve cluster UUID: %v", err))
	}

	slots, err := r.collectAffectedSlots(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.setSubphase(ctx, ops, "TriggeringCommit")

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(slots))
	anyFailed := false

	for i := range slots {
		slot := &slots[i]
		clusterID, poolID, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID)
		if !ok {
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: "invalid VolumeID",
			})
			anyFailed = true
			continue
		}
		_ = clusterID // volume is on the source cluster identified by clusterUUID

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/commit",
			clusterUUID, poolID, volumeID)
		var commitBody interface{}
		if ops.Spec.DeleteSource {
			commitBody = map[string]interface{}{"delete_source": true}
		}
		_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, commitBody)
		if err != nil || (status != 202 && status >= 300) {
			if err == nil {
				err = fmt.Errorf("status %d", status)
			}
			log.Error(err, "POST replication/commit failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: fmt.Sprintf("commit failed: %v", err),
			})
			anyFailed = true
			continue
		}

		// Commit accepted — mark slot as cutover_pending immediately.
		// The backend task drives it to cutover_done asynchronously.
		slotPatch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending)
		slot.Status.Message = fmt.Sprintf("Cutover commit queued via ReplicationOps %s", ops.Name)
		if err := r.Status().Patch(ctx, slot, slotPatch); err != nil {
			log.Error(err, "failed to update slot status to cutover_pending", "slot", slot.Name)
		}

		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
		})
	}

	if anyFailed {
		return r.failOps(ctx, ops, "Migration commit failed for one or more volumes; see results for details")
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "MigrationCommitQueued", "MigrationCommitQueued",
		"Cutover commit queued for policy %s (%d volumes); slots transitioning to cutover_done", policy.Name, len(slots))
	return r.succeedOps(ctx, ops, "Migration cutover commit accepted; slots transitioning to cutover_done", results)
}

// collectAffectedSlots resolves the list of ReplicationSlots targeted by ops.
func (r *ReplicationOpsReconciler) collectAffectedSlots(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
) ([]simplyblockv1alpha1.ReplicationSlot, error) {
	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopeVolume:
		var slot simplyblockv1alpha1.ReplicationSlot
		if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.Ref, Namespace: ops.Namespace}, &slot); err != nil {
			return nil, fmt.Errorf("get ReplicationSlot %q: %w", ops.Spec.Ref, err)
		}
		return []simplyblockv1alpha1.ReplicationSlot{slot}, nil

	case utils.ReplicationOpsScopePolicy:
		var list simplyblockv1alpha1.ReplicationSlotList
		if err := r.List(ctx, &list,
			client.InNamespace(ops.Namespace),
			client.MatchingFields{"spec.policyRef": policy.Name},
		); err != nil {
			return nil, fmt.Errorf("list ReplicationSlots for policy %q: %w", policy.Name, err)
		}
		return list.Items, nil

	default:
		return nil, fmt.Errorf("unknown scope %q", ops.Spec.Scope)
	}
}

// collectSlotsForPair returns all ReplicationSlots that belong to any policy
// referencing the given pair.
func (r *ReplicationOpsReconciler) collectSlotsForPair(
	ctx context.Context,
	pairName string,
	namespace string,
) ([]simplyblockv1alpha1.ReplicationSlot, error) {
	var policies simplyblockv1alpha1.ReplicationPolicyList
	if err := r.List(ctx, &policies,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.pairRef": pairName},
	); err != nil {
		return nil, fmt.Errorf("list policies for pair %q: %w", pairName, err)
	}

	var all []simplyblockv1alpha1.ReplicationSlot
	for _, pol := range policies.Items {
		var slots simplyblockv1alpha1.ReplicationSlotList
		if err := r.List(ctx, &slots,
			client.InNamespace(namespace),
			client.MatchingFields{"spec.policyRef": pol.Name},
		); err != nil {
			return nil, fmt.Errorf("list slots for policy %q: %w", pol.Name, err)
		}
		all = append(all, slots.Items...)
	}
	return all, nil
}

// releaseLock releases whichever lock this ops holds: pair-level for scope=target,
// policy-level for all other scopes.
func (r *ReplicationOpsReconciler) releaseLock(ctx context.Context, ops *simplyblockv1alpha1.ReplicationOps) {
	if ops.Spec.Scope == utils.ReplicationOpsScopeTarget {
		r.releasePairLock(ctx, ops)
	} else {
		r.releasePolicyLock(ctx, ops)
	}
}

// releasePairLock clears pair.Status.ActiveOpsRef when it matches this ops.
func (r *ReplicationOpsReconciler) releasePairLock(ctx context.Context, ops *simplyblockv1alpha1.ReplicationOps) {
	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.Ref, Namespace: ops.Namespace}, &pair); err != nil {
		return
	}
	if pair.Status.ActiveOpsRef != ops.Name {
		return
	}
	patch := client.MergeFrom(pair.DeepCopy())
	pair.Status.ActiveOpsRef = ""
	_ = r.Status().Patch(ctx, &pair, patch)
}

// resolveAffectedPolicyName returns the ReplicationPolicy name targeted by ops.
// For scope=volume, ref is a ReplicationSlot name; the policy is read from the
// slot's spec.policyRef. For all other scopes, ref IS the policy name.
func (r *ReplicationOpsReconciler) resolveAffectedPolicyName(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
) (string, error) {
	if ops.Spec.Scope != utils.ReplicationOpsScopeVolume {
		return ops.Spec.Ref, nil
	}
	var slot simplyblockv1alpha1.ReplicationSlot
	if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.Ref, Namespace: ops.Namespace}, &slot); err != nil {
		return "", fmt.Errorf("scope=volume: get ReplicationSlot %q: %w", ops.Spec.Ref, err)
	}
	return slot.Spec.PolicyRef, nil
}

// setSubphase updates status.subphase without blocking on error.
func (r *ReplicationOpsReconciler) setSubphase(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	subphase string,
) {
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Subphase = subphase
	_ = r.Status().Patch(ctx, ops, patch)
}

func (r *ReplicationOpsReconciler) succeedOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	message string,
	results []simplyblockv1alpha1.ReplicationOpsResult,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)
	ops.Status.Subphase = ""
	ops.Status.Message = message
	ops.Status.CompletedAt = &now
	ops.Status.Results = results
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.releaseLock(ctx, ops)
	return ctrl.Result{}, nil
}

func (r *ReplicationOpsReconciler) failOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	reason string,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseFailed)
	ops.Status.Subphase = ""
	ops.Status.Message = reason
	ops.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "Failed", "Failed",
		"ReplicationOps %s failed: %s", ops.Name, reason)
	r.releaseLock(ctx, ops)
	return ctrl.Result{}, nil
}

func (r *ReplicationOpsReconciler) releasePolicyLock(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
) {
	policyName, err := r.resolveAffectedPolicyName(ctx, ops)
	if err != nil || policyName == "" {
		return
	}
	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: ops.Namespace}, &policy); err != nil {
		return
	}
	if policy.Status.ActiveOpsRef != ops.Name {
		return
	}
	patch := client.MergeFrom(policy.DeepCopy())
	policy.Status.ActiveOpsRef = ""
	_ = r.Status().Patch(ctx, &policy, patch)
}

func (r *ReplicationOpsReconciler) policyToOpsRequests(ctx context.Context, obj client.Object) []ctrl.Request {
	policy := obj.(*simplyblockv1alpha1.ReplicationPolicy)
	var opsList simplyblockv1alpha1.ReplicationOpsList
	if err := r.List(ctx, &opsList,
		client.InNamespace(policy.Namespace),
		client.MatchingFields{"spec.ref": policy.Name},
	); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, ops := range opsList.Items {
		if ops.Status.Phase == "" ||
			ops.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhasePending) ||
			ops.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhaseRunning) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Name:      ops.Name,
				Namespace: ops.Namespace,
			}})
		}
	}
	return reqs
}

func (r *ReplicationOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.ReplicationOps{},
		"spec.ref",
		func(obj client.Object) []string {
			ops := obj.(*simplyblockv1alpha1.ReplicationOps)
			return []string{ops.Spec.Ref}
		},
	); err != nil {
		return fmt.Errorf("index ReplicationOps.spec.ref: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationOps{}).
		Named("replicationops").
		Watches(
			&simplyblockv1alpha1.ReplicationPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToOpsRequests),
		).
		Complete(r)
}
