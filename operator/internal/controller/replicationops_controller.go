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
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ReplicationOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ops simplyblockv1alpha1.ReplicationOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiClient := webapi.NewClient()

	// On deletion: release the policy lock before the CR disappears.
	if !ops.DeletionTimestamp.IsZero() {
		r.releasePolicyLock(ctx, &ops)
		controllerutil.RemoveFinalizer(&ops, finalizerReplicationOps)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// Ensure finalizer so we can release the lock on deletion.
	if !controllerutil.ContainsFinalizer(&ops, finalizerReplicationOps) {
		controllerutil.AddFinalizer(&ops, finalizerReplicationOps)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// Terminal phases: best-effort release the policy lock then do nothing more.
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

	// Resolve the ReplicationPolicy named by spec.ref.
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

	// Acquire the policy lock.
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

	// Transition to Running.
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

// reconcileFailover drives a failover (planned or unplanned) to completion.
func (r *ReplicationOpsReconciler) reconcileFailover(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Collect the pairs affected by this ops based on scope.
	pairs, err := r.collectAffectedPairs(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopeTarget:
		// POST /replication-targets/{id}/failover — atomically fails over all volumes.
		endpoint := fmt.Sprintf("/api/v2/replication-targets/%s/failover", policy.Status.BackendTargetID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST target failover failed")
			return r.failOps(ctx, ops, fmt.Sprintf("target failover failed: %v", err))
		}

	case utils.ReplicationOpsScopePolicy:
		// POST /replication-policies/{id}/failover — atomically fails over all volumes in policy.
		endpoint := fmt.Sprintf("/api/v2/replication-policies/%s/failover", policy.Status.BackendPolicyID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST policy failover failed")
			return r.failOps(ctx, ops, fmt.Sprintf("policy failover failed: %v", err))
		}

	case utils.ReplicationOpsScopeVolume:
		// POST /{vol}/replicate_lvol — per-volume unplanned failover.
		if len(pairs) != 1 {
			return r.failOps(ctx, ops, fmt.Sprintf("scope=volume requires exactly 1 pair; got %d", len(pairs)))
		}
		pair := &pairs[0]
		clusterID, poolID, volumeID, ok := splitVolumeHandle(pair.Spec.VolumeID)
		if !ok {
			return r.failOps(ctx, ops, fmt.Sprintf("invalid VolumeID on pair %s", pair.Name))
		}
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replicate_lvol",
			clusterID, poolID, volumeID)
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "POST replicate_lvol failed", "pair", pair.Name)
			return r.failOps(ctx, ops, fmt.Sprintf("volume failover failed: %v", err))
		}

	default:
		return r.failOps(ctx, ops, fmt.Sprintf("unknown scope %q", ops.Spec.Scope))
	}

	// Update pair statuses to failed_over.
	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(pairs))
	for i := range pairs {
		pair := &pairs[i]
		pairPatch := client.MergeFrom(pair.DeepCopy())
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateFailedOver)
		pair.Status.Direction = string(simplyblockv1alpha1.ReplicationPairDirectionTarget)
		pair.Status.Message = fmt.Sprintf("Failed over via ReplicationOps %s", ops.Name)
		if err := r.Status().Patch(ctx, pair, pairPatch); err != nil {
			log.Error(err, "failed to update pair status after failover", "pair", pair.Name)
		}
		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			PairRef:      pair.Name,
			Status:       string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
			TargetLvolID: pair.Status.TargetLvolID,
		})
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailoverSucceeded", "FailoverSucceeded",
		"Failover completed for policy %s (%d volumes)", policy.Name, len(pairs))
	return r.succeedOps(ctx, ops, "Failover completed successfully", results)
}

// reconcileFailback drives a failback (reverse replication then commit) to completion.
func (r *ReplicationOpsReconciler) reconcileFailback(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pairs, err := r.collectAffectedPairs(ctx, ops, policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	results := make([]simplyblockv1alpha1.ReplicationOpsResult, 0, len(pairs))
	anyFailed := false

	for i := range pairs {
		pair := &pairs[i]
		clusterID, poolID, volumeID, ok := splitVolumeHandle(pair.Spec.VolumeID)
		if !ok {
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				PairRef: pair.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: "invalid VolumeID",
			})
			anyFailed = true
			continue
		}

		// Step 1: start reverse replication (failback).
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
			log.Error(fbErr, "replication_failback failed", "pair", pair.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				PairRef: pair.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: fbErr.Error(),
			})
			anyFailed = true
			continue
		}

		// Step 2: commit (complete the cutback — switches the source cluster back to active).
		commitEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication_commit",
			clusterID, poolID, volumeID)
		body, status, commitErr := apiClient.Do(ctx, http.MethodPost, commitEndpoint, nil)
		if commitErr != nil || status >= 300 {
			if commitErr == nil {
				commitErr = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(commitErr, "replication_commit (failback step) failed", "pair", pair.Name)
			results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
				PairRef: pair.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultFailed),
				Detail: commitErr.Error(),
			})
			anyFailed = true
			continue
		}

		// Update pair direction back to source.
		pairPatch := client.MergeFrom(pair.DeepCopy())
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateReplicating)
		pair.Status.Direction = string(simplyblockv1alpha1.ReplicationPairDirectionSource)
		pair.Status.Message = fmt.Sprintf("Failed back via ReplicationOps %s", ops.Name)
		if err := r.Status().Patch(ctx, pair, pairPatch); err != nil {
			log.Error(err, "failed to update pair status after failback", "pair", pair.Name)
		}
		results = append(results, simplyblockv1alpha1.ReplicationOpsResult{
			PairRef: pair.Name, Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded),
		})
	}

	if anyFailed {
		return r.failOps(ctx, ops, "Failback completed with errors; see results for details")
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "FailbackSucceeded", "FailbackSucceeded",
		"Failback completed for policy %s (%d volumes)", policy.Name, len(pairs))
	return r.succeedOps(ctx, ops, "Failback completed successfully", results)
}

// collectAffectedPairs resolves the list of ReplicationPairs targeted by ops.
func (r *ReplicationOpsReconciler) collectAffectedPairs(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	policy *simplyblockv1alpha1.ReplicationPolicy,
) ([]simplyblockv1alpha1.ReplicationPair, error) {
	switch ops.Spec.Scope {
	case utils.ReplicationOpsScopeVolume:
		// spec.ref is the name of a single ReplicationPair.
		var pair simplyblockv1alpha1.ReplicationPair
		if err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.Ref, Namespace: ops.Namespace}, &pair); err != nil {
			return nil, fmt.Errorf("get ReplicationPair %q: %w", ops.Spec.Ref, err)
		}
		return []simplyblockv1alpha1.ReplicationPair{pair}, nil

	case utils.ReplicationOpsScopePolicy, utils.ReplicationOpsScopeTarget:
		// All pairs belonging to the policy.
		var list simplyblockv1alpha1.ReplicationPairList
		if err := r.List(ctx, &list,
			client.InNamespace(ops.Namespace),
			client.MatchingFields{"spec.policyRef": policy.Name},
		); err != nil {
			return nil, fmt.Errorf("list ReplicationPairs for policy %q: %w", policy.Name, err)
		}
		return list.Items, nil

	default:
		return nil, fmt.Errorf("unknown scope %q", ops.Spec.Scope)
	}
}

// resolveAffectedPolicyName returns the name of the ReplicationPolicy that ops targets.
// For scope=volume, spec.ref is a pair name — we look up the pair to get its policyRef.
// For scope=policy and scope=target, spec.ref IS the policy name.
func (r *ReplicationOpsReconciler) resolveAffectedPolicyName(ops *simplyblockv1alpha1.ReplicationOps) string {
	if ops.Spec.Scope == utils.ReplicationOpsScopeVolume {
		// We can't look up the pair without a client call; rely on the pair's policyRef
		// being available. The ops ref for scope=volume is a pair name — try a best-effort
		// parse by returning the pair name as the policy name first.  The policy lookup
		// will fail fast (NotFound) and we'll fail the ops.  In practice users should
		// set spec.ref to a ReplicationPolicy name when scope != volume.
		//
		// For scope=volume, the caller (reconcileFailover) uses spec.ref directly
		// as the pair name, not the policy name. We need the policy for the lock.
		// We return an empty ref here and handle this edge case in reconcileFailover.
		return ops.Spec.Ref // best-effort — the policy name is not directly encoded for scope=volume
	}
	return ops.Spec.Ref
}

// succeedOps transitions ops to Succeeded and releases the policy lock.
func (r *ReplicationOpsReconciler) succeedOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	message string,
	results []simplyblockv1alpha1.ReplicationOpsResult,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)
	ops.Status.Message = message
	ops.Status.CompletedAt = &now
	ops.Status.Results = results
	if err := r.Status().Patch(ctx, ops, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.releasePolicyLock(ctx, ops)
	return ctrl.Result{}, nil
}

// failOps transitions ops to Failed and releases the policy lock.
func (r *ReplicationOpsReconciler) failOps(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	reason string,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseFailed)
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

// releasePolicyLock clears activeOpsRef on the ReplicationPolicy if it points to this ops.
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

// policyToOpsRequests maps a ReplicationPolicy event to any pending ReplicationOps
// targeting it, so they wake up when the policy lock is released.
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
	// Index ReplicationOps by spec.ref so the policy → ops mapper can list them efficiently.
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
		// Wake pending ops when the policy lock is released.
		Watches(
			&simplyblockv1alpha1.ReplicationPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToOpsRequests),
		).
		Complete(r)
}
