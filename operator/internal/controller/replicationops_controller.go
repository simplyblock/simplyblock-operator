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
		r.releasePolicyLock(ctx, &ops)
		if controllerutil.ContainsFinalizer(&ops, finalizerReplicationOps) {
			controllerutil.RemoveFinalizer(&ops, finalizerReplicationOps)
			return ctrl.Result{}, r.Update(ctx, &ops)
		}
		return ctrl.Result{}, nil
	}

	policyName := r.resolveAffectedPolicyName(&ops)
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
	default:
		return r.failOps(ctx, &ops, fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
}

// reconcileFailover drives a failover to completion.
func (r *ReplicationOpsReconciler) reconcileFailover(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	slots, err := r.collectAffectedSlots(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.setSubphase(ctx, ops, "TriggeringFailover")

	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopeTarget:
		endpoint := fmt.Sprintf("/api/v2/replication/targets/%s/failover", policy.Status.BackendPolicyID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST target failover failed")
			return r.failOps(ctx, ops, fmt.Sprintf("target failover failed: %v", err))
		}

	case utils.ReplicationOpsScopePolicy:
		endpoint := fmt.Sprintf("/api/v2/replication/policies/%s/failover", policy.Status.BackendPolicyID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST policy failover failed")
			return r.failOps(ctx, ops, fmt.Sprintf("policy failover failed: %v", err))
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

	r.setSubphase(ctx, ops, "UpdatingSlotStatuses")

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(slots))
	for i := range slots {
		slot := &slots[i]
		slotPatch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		slot.Status.Message = fmt.Sprintf("Failed over via ReplicationOps %s", ops.Name)
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

// reconcileFailback drives a failback to completion.
func (r *ReplicationOpsReconciler) reconcileFailback(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	slots, err := r.collectAffectedSlots(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

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

		r.setSubphase(ctx, ops, "StartingFailback")

		fbBody := map[string]interface{}{}
		if ops.Spec.SourceClusterID != "" {
			fbBody["source_cluster_id"] = ops.Spec.SourceClusterID
		}
		fbEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication_failback",
			clusterID, poolID, volumeID)
		body, status, fbErr := apiClient.Do(ctx, http.MethodPost, fbEndpoint, fbBody)
		if fbErr != nil || status >= 300 {
			if fbErr == nil {
				fbErr = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(fbErr, "replication_failback failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: fbErr.Error(),
			})
			anyFailed = true
			continue
		}

		r.setSubphase(ctx, ops, "CommittingFailback")

		commitEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication_commit",
			clusterID, poolID, volumeID)
		body, status, commitErr := apiClient.Do(ctx, http.MethodPost, commitEndpoint, nil)
		if commitErr != nil || status >= 300 {
			if commitErr == nil {
				commitErr = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(commitErr, "replication_commit (failback step) failed", "slot", slot.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: commitErr.Error(),
			})
			anyFailed = true
			continue
		}

		slotPatch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
		slot.Status.Message = fmt.Sprintf("Failed back via ReplicationOps %s", ops.Name)
		if err := r.Status().Patch(ctx, slot, slotPatch); err != nil {
			log.Error(err, "failed to update slot status after failback", "slot", slot.Name)
		}
		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			SlotRef: slot.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
		})
	}

	if anyFailed {
		return r.failOps(ctx, ops, "Failback completed with errors; see results for details")
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailbackSucceeded", "FailbackSucceeded",
		"Failback completed for policy %s (%d volumes)", policy.Name, len(slots))
	return r.succeedOps(ctx, ops, "Failback completed successfully", results)
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

	case utils.ReplicationOpsScopePolicy, utils.ReplicationOpsScopeTarget:
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

// resolveAffectedPolicyName returns the ReplicationPolicy name targeted by ops.
func (r *ReplicationOpsReconciler) resolveAffectedPolicyName(ops *simplyblockv1alpha1.ReplicationOps) string {
	return ops.Spec.Ref
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
	r.releasePolicyLock(ctx, ops)
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
	r.releasePolicyLock(ctx, ops)
	return ctrl.Result{}, nil
}

func (r *ReplicationOpsReconciler) releasePolicyLock(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
) {
	policyName := r.resolveAffectedPolicyName(ops)
	if policyName == "" {
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
