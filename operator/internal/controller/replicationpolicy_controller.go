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
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

func (r *ReplicationPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the ReplicationPair that owns the cluster-pair configuration.
	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Spec.PairRef, Namespace: policy.Namespace}, &pair); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "ReplicationPair not found", "pairRef", policy.Spec.PairRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get ReplicationPair %q: %w", policy.Spec.PairRef, err)
	}
	if !pair.Status.Ready {
		log.Info("ReplicationPair not ready yet; waiting", "pair", pair.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Resolve the source cluster UUID for backend API calls.
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, policy.Namespace, pair.Spec.SourceCluster)
	if err != nil {
		log.Error(err, "failed to resolve source cluster UUID", "sourceCluster", pair.Spec.SourceCluster)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	if !policy.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &policy, apiClient, clusterUUID)
	}

	if !controllerutil.ContainsFinalizer(&policy, utils.FinalizerReplicationPolicy) {
		controllerutil.AddFinalizer(&policy, utils.FinalizerReplicationPolicy)
		return ctrl.Result{}, r.Update(ctx, &policy)
	}

	// Ensure the backend ReplicationPolicy exists, referencing the pair's backend target.
	if policy.Status.BackendPolicyID == "" {
		policyID, err := r.ensureBackendPolicy(ctx, &policy, &pair, apiClient, clusterUUID)
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

	// Sync slot count and mark ready.
	var slotList simplyblockv1alpha1.ReplicationSlotList
	if err := r.List(ctx, &slotList,
		client.InNamespace(policy.Namespace),
		client.MatchingFields{"spec.policyRef": policy.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ReplicationSlots for policy %q: %w", policy.Name, err)
	}

	patch := client.MergeFrom(policy.DeepCopy())
	policy.Status.SlotCount = int32(len(slotList.Items))
	policy.Status.Ready = true
	if err := r.Status().Patch(ctx, &policy, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("ReplicationPolicy ready",
		"policy", policy.Name,
		"backendPolicyID", policy.Status.BackendPolicyID,
		"slots", policy.Status.SlotCount)

	return ctrl.Result{RequeueAfter: replPolicyRequeueInterval}, nil
}

// reconcileDelete handles cleanup when a ReplicationPolicy CR is deleted:
// blocks deletion while any ReplicationSlot CRs still exist, then deletes the
// backend ReplicationPolicy (the backend target is managed by the ReplicationPair).
func (r *ReplicationPolicyReconciler) reconcileDelete(
	ctx context.Context,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
	clusterUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var slotList simplyblockv1alpha1.ReplicationSlotList
	if err := r.List(ctx, &slotList,
		client.InNamespace(policy.Namespace),
		client.MatchingFields{"spec.policyRef": policy.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ReplicationSlots: %w", err)
	}
	if len(slotList.Items) > 0 {
		log.Info("Waiting for ReplicationSlots to be deleted before removing policy",
			"policy", policy.Name, "slotCount", len(slotList.Items))
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

	// The backend ReplicationTarget lifecycle is managed by the ReplicationPair CR;
	// we do not touch it here.

	controllerutil.RemoveFinalizer(policy, utils.FinalizerReplicationPolicy)
	return ctrl.Result{}, r.Update(ctx, policy)
}

// ensureBackendPolicy checks for an existing backend ReplicationPolicy with a name
// matching policy.Name; creates one if absent. Returns the policy's backend UUID.
func (r *ReplicationPolicyReconciler) ensureBackendPolicy(
	ctx context.Context,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
	clusterUUID string,
) (string, error) {
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

	// Not found — create a new one using the pair's backend target ID.
	intervalMin, err := parseDurationToMinutes(policy.Spec.Interval)
	if err != nil {
		intervalMin = 5
	}
	reqBody := map[string]interface{}{
		"policy_name":     policy.Name,
		"target_id":       pair.Status.BackendTargetID,
		"interval_min":    intervalMin,
		"mode":            policy.Spec.Mode,
		"keep_replicated": policy.Spec.SnapshotRetention,
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
	// Index ReplicationSlots by spec.policyRef so the reconciler can quickly count slots.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.ReplicationSlot{},
		"spec.policyRef",
		func(obj client.Object) []string {
			slot := obj.(*simplyblockv1alpha1.ReplicationSlot)
			return []string{slot.Spec.PolicyRef}
		},
	); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationPolicy{}).
		Named("replicationpolicy").
		// Wake the policy when a slot is added/removed so slotCount stays current.
		Watches(&simplyblockv1alpha1.ReplicationSlot{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				slot := obj.(*simplyblockv1alpha1.ReplicationSlot)
				if slot.Spec.PolicyRef == "" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Name:      slot.Spec.PolicyRef,
					Namespace: obj.GetNamespace(),
				}}}
			},
		)).
		// Wake the policy when the ReplicationPair becomes ready (backendTargetID available).
		Watches(&simplyblockv1alpha1.ReplicationPair{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				// Enqueue all policies that reference this pair.
				var list simplyblockv1alpha1.ReplicationPolicyList
				if err := r.List(ctx, &list,
					client.InNamespace(obj.GetNamespace()),
					client.MatchingFields{"spec.pairRef": obj.GetName()},
				); err != nil {
					return nil
				}
				reqs := make([]reconcile.Request, 0, len(list.Items))
				for _, p := range list.Items {
					reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
						Name:      p.Name,
						Namespace: p.Namespace,
					}})
				}
				return reqs
			},
		)).
		Complete(r)
}
