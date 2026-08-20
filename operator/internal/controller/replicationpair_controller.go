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
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	replPairSyncInterval = 60 * time.Second
	replPairRequeueError = 30 * time.Second
)

// ReplicationPairReconciler reconciles ReplicationPair resources.
// It ensures the backend ReplicationTarget exists for the configured source→target cluster
// pair and manages its lifecycle. The backend target ID is stored in status.backendTargetID
// for ReplicationPolicy resources to reference.
type ReplicationPairReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

func (r *ReplicationPairReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, req.NamespacedName, &pair); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiClient := webapi.NewClient()

	if !pair.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &pair, apiClient)
	}

	if !controllerutil.ContainsFinalizer(&pair, utils.FinalizerReplicationPair) {
		controllerutil.AddFinalizer(&pair, utils.FinalizerReplicationPair)
		return ctrl.Result{}, r.Update(ctx, &pair)
	}

	// Resolve cluster UUIDs from StorageCluster names (or pass-through if already a UUID).
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, pair.Namespace, pair.Spec.SourceCluster)
	if err != nil {
		log.Error(err, "failed to resolve source cluster UUID", "sourceCluster", pair.Spec.SourceCluster)
		return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
	}
	targetUUID, err := utils.ResolveClusterUUID(ctx, r.Client, pair.Namespace, pair.Spec.TargetCluster)
	if err != nil {
		log.Error(err, "failed to resolve target cluster UUID", "targetCluster", pair.Spec.TargetCluster)
		return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
	}

	// Ensure the backend ReplicationTarget exists.
	if pair.Status.BackendTargetID == "" {
		targetID, err := r.ensureBackendTarget(ctx, &pair, apiClient, clusterUUID, targetUUID)
		if err != nil {
			log.Error(err, "failed to ensure backend ReplicationTarget", "pair", pair.Name)
			patch := client.MergeFrom(pair.DeepCopy())
			pair.Status.Ready = false
			pair.Status.Message = fmt.Sprintf("failed to create backend ReplicationTarget: %v", err)
			_ = r.Status().Patch(ctx, &pair, patch)
			return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
		}
		patch := client.MergeFrom(pair.DeepCopy())
		pair.Status.BackendTargetID = targetID
		pair.Status.Ready = true
		pair.Status.Message = "ReplicationTarget ready"
		if err := r.Status().Patch(ctx, &pair, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Info("ReplicationPair ready", "pair", pair.Name, "backendTargetID", targetID)
	} else if !pair.Status.Ready {
		patch := client.MergeFrom(pair.DeepCopy())
		pair.Status.Ready = true
		pair.Status.Message = "ReplicationTarget ready"
		_ = r.Status().Patch(ctx, &pair, patch)
	}

	return ctrl.Result{RequeueAfter: replPairSyncInterval}, nil
}

// reconcileDelete blocks deletion while any ReplicationPolicy still references this pair,
// then deletes the backend ReplicationTarget before removing the finalizer.
func (r *ReplicationPairReconciler) reconcileDelete(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Block while policies still reference this pair.
	var policies simplyblockv1alpha1.ReplicationPolicyList
	if err := r.List(ctx, &policies,
		client.InNamespace(pair.Namespace),
		client.MatchingFields{"spec.pairRef": pair.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ReplicationPolicies for pair %q: %w", pair.Name, err)
	}
	if len(policies.Items) > 0 {
		log.Info("Waiting for ReplicationPolicies to be deleted before removing pair",
			"pair", pair.Name, "policyCount", len(policies.Items))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Delete the backend ReplicationTarget.
	if pair.Status.BackendTargetID != "" {
		clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, pair.Namespace, pair.Spec.SourceCluster)
		if err != nil {
			log.Error(err, "failed to resolve source cluster UUID for target deletion", "pair", pair.Name)
			return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
		}
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/replication/targets/%s", clusterUUID, pair.Status.BackendTargetID)
		body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
		if err != nil || (status >= 300 && status != http.StatusNotFound) {
			if err == nil {
				err = fmt.Errorf("status %d: %s", status, string(body))
			}
			log.Error(err, "failed to delete backend ReplicationTarget", "id", pair.Status.BackendTargetID)
			return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
		}
		log.Info("Deleted backend ReplicationTarget", "id", pair.Status.BackendTargetID)
	}

	controllerutil.RemoveFinalizer(pair, utils.FinalizerReplicationPair)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, pair))
}

// ensureBackendTarget checks for an existing backend ReplicationTarget for this pair's
// target cluster; creates one if absent. Returns the target's backend UUID.
func (r *ReplicationPairReconciler) ensureBackendTarget(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
	clusterUUID string,
	targetUUID string,
) (string, error) {
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
		if t.TargetClusterID == targetUUID {
			return t.ID, nil
		}
	}

	// Not found — create.
	reqBody := map[string]interface{}{
		"target_name":       fmt.Sprintf("simplyblock-repl-%s", pair.Spec.TargetCluster),
		"target_cluster_id": targetUUID,
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

func (r *ReplicationPairReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index ReplicationPolicies by spec.pairRef so deletion can quickly check dependents.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.ReplicationPolicy{},
		"spec.pairRef",
		func(obj client.Object) []string {
			policy := obj.(*simplyblockv1alpha1.ReplicationPolicy)
			return []string{policy.Spec.PairRef}
		},
	); err != nil {
		return fmt.Errorf("index ReplicationPolicy.spec.pairRef: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationPair{}).
		Named("replicationpair").
		Complete(r)
}
