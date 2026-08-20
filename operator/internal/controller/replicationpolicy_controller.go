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
	"math"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// replPolicyRequeueInterval is how often the ReplicationPolicy reconciler re-syncs
// its status (pair count and readiness) when already steady.
const replPolicyRequeueInterval = 30 * time.Second

// replicationTargetEntry is a single item from GET /api/v2/replication/targets.
type replicationTargetEntry struct {
	ID              string `json:"id"`
	TargetClusterID string `json:"target_cluster_id"`
	TargetName      string `json:"target_name"`
}

// replicationPolicyEntry is a single item from GET /api/v2/replication/policies.
type replicationPolicyEntry struct {
	ID   string `json:"id"`
	Name string `json:"policy_name"`
}

// idResponse is the envelope for POST calls that return {"id": "..."}.
type idResponse struct {
	ID string `json:"id"`
}

// ReplicationPolicyReconciler reconciles ReplicationPolicy resources.
type ReplicationPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

func (r *ReplicationPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, policy.Namespace, policy.Spec.ClusterName)
	if err != nil {
		log.Error(err, "failed to resolve local cluster UUID", "clusterName", policy.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	if !policy.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &policy, apiClient, clusterUUID)
	}

	// Ensure our finalizer is present so we can clean up backend resources on deletion.
	if !controllerutil.ContainsFinalizer(&policy, utils.FinalizerReplicationPolicy) {
		controllerutil.AddFinalizer(&policy, utils.FinalizerReplicationPolicy)
		return ctrl.Result{}, r.Update(ctx, &policy)
	}

	// Step 1: ensure the backend ReplicationTarget for spec.target exists.
	// Multiple ReplicationPolicy CRs pointing at the same remote cluster share one target.
	if policy.Status.BackendTargetID == "" {
		targetID, err := r.ensureBackendTarget(ctx, &policy, apiClient, clusterUUID)
		if err != nil {
			log.Error(err, "failed to ensure backend ReplicationTarget", "target", policy.Spec.Target)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		patch := client.MergeFrom(policy.DeepCopy())
		policy.Status.BackendTargetID = targetID
		if err := r.Status().Patch(ctx, &policy, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Step 2: ensure the backend ReplicationPolicy (one per CR) exists.
	if policy.Status.BackendPolicyID == "" {
		policyID, err := r.ensureBackendPolicy(ctx, &policy, apiClient, clusterUUID)
		if err != nil {
			log.Error(err, "failed to ensure backend ReplicationPolicy", "policy", policy.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		patch := client.MergeFrom(policy.DeepCopy())
		policy.Status.BackendPolicyID = policyID
		if err := r.Status().Patch(ctx, &policy, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Step 3: sync pair count and mark ready.
	var pairList simplyblockv1alpha1.ReplicationPairList
	if err := r.List(ctx, &pairList,
		client.InNamespace(policy.Namespace),
		client.MatchingFields{"spec.policyRef": policy.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ReplicationPairs for policy %q: %w", policy.Name, err)
	}

	patch := client.MergeFrom(policy.DeepCopy())
	policy.Status.PairCount = int32(len(pairList.Items))
	policy.Status.Ready = true
	if err := r.Status().Patch(ctx, &policy, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("ReplicationPolicy ready",
		"policy", policy.Name,
		"backendTargetID", policy.Status.BackendTargetID,
		"backendPolicyID", policy.Status.BackendPolicyID,
		"pairs", policy.Status.PairCount)

	return ctrl.Result{RequeueAfter: replPolicyRequeueInterval}, nil
}

// reconcileDelete handles cleanup when a ReplicationPolicy CR is deleted:
// - blocks deletion while any ReplicationPair CRs still exist
// - deletes the owned backend ReplicationPolicy
// - deletes the shared backend ReplicationTarget only if no sibling CR still references it
func (r *ReplicationPolicyReconciler) reconcileDelete(
	ctx context.Context,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
	clusterUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Block until all owned pairs are gone.
	var pairList simplyblockv1alpha1.ReplicationPairList
	if err := r.List(ctx, &pairList,
		client.InNamespace(policy.Namespace),
		client.MatchingFields{"spec.policyRef": policy.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ReplicationPairs: %w", err)
	}
	if len(pairList.Items) > 0 {
		log.Info("Waiting for ReplicationPairs to be deleted before removing policy",
			"policy", policy.Name, "pairCount", len(pairList.Items))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Delete the backend ReplicationPolicy.
	if policy.Status.BackendPolicyID != "" {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/policies/%s", clusterUUID, policy.Status.BackendPolicyID)
		body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
		if err != nil || (status >= 300 && status != http.StatusNotFound) {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "failed to delete backend ReplicationPolicy", "id", policy.Status.BackendPolicyID)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		log.Info("Deleted backend ReplicationPolicy", "id", policy.Status.BackendPolicyID)
	}

	// Delete the backend ReplicationTarget only if no other ReplicationPolicy in this
	// namespace still references the same remote cluster.
	if policy.Status.BackendTargetID != "" {
		if shouldDelete, err := r.isTargetOrphaned(ctx, policy); err != nil {
			log.Error(err, "failed to check whether ReplicationTarget is shared")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		} else if shouldDelete {
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/targets/%s", clusterUUID, policy.Status.BackendTargetID)
			body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
			if err != nil || (status >= 300 && status != http.StatusNotFound) {
				if err == nil {
					err = fmt.Errorf("status %d: %s", status, string(body))
				}
				// A 400 from the backend means a policy still references the target.
				// This should not happen because we deleted our backend policy above, but
				// treat it as transient rather than fatal.
				log.Error(err, "failed to delete backend ReplicationTarget", "id", policy.Status.BackendTargetID)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			log.Info("Deleted backend ReplicationTarget", "id", policy.Status.BackendTargetID)
		} else {
			log.Info("ReplicationTarget is still referenced by sibling policies, keeping it",
				"targetID", policy.Status.BackendTargetID)
		}
	}

	controllerutil.RemoveFinalizer(policy, utils.FinalizerReplicationPolicy)
	return ctrl.Result{}, r.Update(ctx, policy)
}

// isTargetOrphaned returns true if no other ReplicationPolicy CR in the namespace
// references the same spec.target cluster as the given policy.
func (r *ReplicationPolicyReconciler) isTargetOrphaned(
	ctx context.Context,
	policy *simplyblockv1alpha1.ReplicationPolicy,
) (bool, error) {
	var siblings simplyblockv1alpha1.ReplicationPolicyList
	if err := r.List(ctx, &siblings, client.InNamespace(policy.Namespace)); err != nil {
		return false, err
	}
	for i := range siblings.Items {
		s := &siblings.Items[i]
		if s.Name == policy.Name {
			continue
		}
		if s.DeletionTimestamp.IsZero() && s.Spec.Target == policy.Spec.Target {
			return false, nil
		}
	}
	return true, nil
}

// ensureBackendTarget checks for an existing backend ReplicationTarget for
// policy.Spec.Target; creates one if absent.  Returns the target's backend UUID.
// Shared: multiple ReplicationPolicy CRs pointing at the same cluster reuse
// the same backend target.
func (r *ReplicationPolicyReconciler) ensureBackendTarget(
	ctx context.Context,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
	clusterUUID string,
) (string, error) {
	// List existing targets and look for one whose target_cluster_id matches.
	listEndpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/targets", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, listEndpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return "", fmt.Errorf("list replication targets: %w", err)
	}
	var targets []replicationTargetEntry
	if err := json.Unmarshal(body, &targets); err != nil {
		return "", fmt.Errorf("unmarshal replication/targets response: %w", err)
	}
	for _, t := range targets {
		if t.TargetClusterID == policy.Spec.Target {
			return t.ID, nil
		}
	}

	// Not found — create a new one.
	reqBody := map[string]interface{}{
		"target_name":       fmt.Sprintf("simplyblock-repl-%s", policy.Spec.Target),
		"target_cluster_id": policy.Spec.Target,
	}
	body, status, err = apiClient.Do(ctx, http.MethodPost, listEndpoint, reqBody)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return "", fmt.Errorf("create replication target: %w", err)
	}
	var created idResponse
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("unmarshal create-target response: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("create replication target: empty id in response: %s", string(body))
	}
	return created.ID, nil
}

// ensureBackendPolicy checks for an existing backend ReplicationPolicy with a
// name matching policy.Name; creates one if absent.  Returns the policy's backend UUID.
func (r *ReplicationPolicyReconciler) ensureBackendPolicy(
	ctx context.Context,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
	clusterUUID string,
) (string, error) {
	// List existing backend policies and look for one with a matching name.
	listEndpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/policies", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, listEndpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return "", fmt.Errorf("list replication policies: %w", err)
	}
	var policies []replicationPolicyEntry
	if err := json.Unmarshal(body, &policies); err != nil {
		return "", fmt.Errorf("unmarshal replication/policies response: %w", err)
	}
	for _, p := range policies {
		if p.Name == policy.Name {
			return p.ID, nil
		}
	}

	// Not found — create a new one.
	intervalMin, err := parseDurationToMinutes(policy.Spec.Interval)
	if err != nil {
		// Fallback: the kubebuilder default guarantees a non-empty value; if
		// parsing fails for any reason, default to 5 minutes.
		intervalMin = 5
	}
	reqBody := map[string]interface{}{
		"policy_name":     policy.Name,
		"target_id":       policy.Status.BackendTargetID,
		"interval_min":    intervalMin,
		"mode":            policy.Spec.Mode,
		"keep_replicated": policy.Spec.KeepReplicated,
	}
	body, status, err = apiClient.Do(ctx, http.MethodPost, listEndpoint, reqBody)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		return "", fmt.Errorf("create replication policy: %w", err)
	}
	var created idResponse
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("unmarshal create-policy response: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("create replication policy: empty id in response: %s", string(body))
	}
	return created.ID, nil
}

// parseDurationToMinutes converts a Go duration string (e.g. "5m", "1h") to
// whole minutes, clamped to a minimum of 1.
func parseDurationToMinutes(s string) (int, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	minutes := int(math.Round(d.Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	return minutes, nil
}

func (r *ReplicationPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index ReplicationPairs by spec.policyRef so the reconciler can quickly count
	// owned pairs without a full list scan.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.ReplicationPair{},
		"spec.policyRef",
		func(obj client.Object) []string {
			pair := obj.(*simplyblockv1alpha1.ReplicationPair)
			return []string{pair.Spec.PolicyRef}
		},
	); err != nil {
		// Return nil if the index already exists (e.g. registered by another controller).
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationPolicy{}).
		Named("replicationpolicy").
		Watches(&simplyblockv1alpha1.ReplicationPair{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				pair := obj.(*simplyblockv1alpha1.ReplicationPair)
				if pair.Spec.PolicyRef == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Name:      pair.Spec.PolicyRef,
					Namespace: obj.GetNamespace(),
				}}}
			},
		)).
		Complete(r)
}
