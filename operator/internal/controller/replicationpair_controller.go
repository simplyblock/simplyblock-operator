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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// replPairRequeueAttaching is how long to wait between polls while the backend
	// is completing the attach (PUT /replication-policy) operation.
	replPairRequeueAttaching = 10 * time.Second
	// replPairRequeueReplicating is how often the reconciler syncs lastReplicatedAt
	// from the backend while a pair is in the steady-state replicating state.
	replPairRequeueReplicating = 60 * time.Second
	// replPairRequeueError is the back-off interval after a backend call fails.
	replPairRequeueError = 30 * time.Second
)

// replVolumeReplicationStatus is the response from
// GET /api/v2/clusters/{cluster}/storage-pools/{pool}/volumes/{vol}/replication.
type replVolumeReplicationStatus struct {
	ReplicationID   string      `json:"replication_id"`
	SourceLvolID    string      `json:"source_lvol_id"`
	TargetLvolID    string      `json:"target_lvol_id"`
	SourceClusterID string      `json:"source_cluster_id"`
	TargetClusterID string      `json:"target_cluster_id"`
	Mode            string      `json:"mode"`
	State           string      `json:"state"`
	Direction       string      `json:"direction"`
	TargetNQN       string      `json:"target_nqn"`
	TargetNsID      int         `json:"target_ns_id"`
	IsSource        bool        `json:"is_source"`
	LastSnapshotAt  interface{} `json:"last_snapshot_at"` // string or null
}

// ReplicationPairReconciler reconciles ReplicationPair resources.
// It drives the state machine: attaching → replicating → (cutover / failover) → detaching → (deleted).
type ReplicationPairReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpairs/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ReplicationPairReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pair simplyblockv1alpha1.ReplicationPair
	if err := r.Get(ctx, req.NamespacedName, &pair); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiClient := webapi.NewClient()

	// Handle deletion: the pair must detach from the backend before the CR is removed.
	if !pair.DeletionTimestamp.IsZero() {
		return r.reconcileDetach(ctx, &pair, apiClient)
	}

	// Ensure finalizer so we can run the backend detach on deletion.
	if !controllerutil.ContainsFinalizer(&pair, utils.FinalizerReplicationPair) {
		controllerutil.AddFinalizer(&pair, utils.FinalizerReplicationPair)
		return ctrl.Result{}, r.Update(ctx, &pair)
	}

	// Fetch the owning ReplicationPolicy; fail fast if it is not ready.
	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: pair.Spec.PolicyRef, Namespace: pair.Namespace}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setError(ctx, &pair, fmt.Sprintf("ReplicationPolicy %q not found", pair.Spec.PolicyRef))
		}
		return ctrl.Result{}, fmt.Errorf("get ReplicationPolicy %q: %w", pair.Spec.PolicyRef, err)
	}
	if !policy.Status.Ready {
		log.Info("ReplicationPolicy not ready; waiting", "policy", policy.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Parse the CSI volume handle stored in spec.volumeID: "<clusterID>:<poolID>:<volumeID>"
	clusterID, poolID, volumeID, ok := splitVolumeHandle(pair.Spec.VolumeID)
	if !ok {
		return r.setError(ctx, &pair, fmt.Sprintf("invalid VolumeID %q: expected <cluster>:<pool>:<volume>", pair.Spec.VolumeID))
	}

	// Dispatch on current state.
	switch simplyblockv1alpha1.ReplicationPairState(pair.Status.State) {
	case "": // brand-new pair
		return r.reconcileAttach(ctx, &pair, &policy, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationPairStateAttaching:
		return r.reconcilePollAttach(ctx, &pair, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationPairStateReplicating:
		return r.reconcileReplicating(ctx, &pair, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationPairStateCutoverPending,
		simplyblockv1alpha1.ReplicationPairStateCutoverDone,
		simplyblockv1alpha1.ReplicationPairStateFailedOver:
		// Sync backend state into status; user-driven transitions happen via ReplicationOps.
		return r.reconcileSyncStatus(ctx, &pair, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationPairStateDetaching:
		// Detaching was triggered externally (e.g. by a ReplicationOps). Drive it
		// to completion and delete the CR when done.
		return r.reconcileDetach(ctx, &pair, apiClient)
	case simplyblockv1alpha1.ReplicationPairStateError:
		// Back off and retry from attaching.
		log.Info("ReplicationPair in error state; retrying attach", "pair", pair.Name)
		return r.reconcileAttach(ctx, &pair, &policy, apiClient, clusterID, poolID, volumeID)
	default:
		return r.setError(ctx, &pair, fmt.Sprintf("unknown state %q", pair.Status.State))
	}
}

// reconcileAttach sends PUT /{vol}/replication-policy and transitions to attaching.
func (r *ReplicationPairReconciler) reconcileAttach(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication-policy",
		clusterID, poolID, volumeID)
	reqBody := map[string]interface{}{
		"policy_id": policy.Status.BackendPolicyID,
	}
	body, status, err := apiClient.Do(ctx, http.MethodPut, endpoint, reqBody)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "PUT replication-policy failed", "pair", pair.Name)
		return r.setError(ctx, pair, fmt.Sprintf("attach failed: %v", err))
	}

	r.Recorder.Eventf(pair, nil, corev1.EventTypeNormal, "Attaching", "Attaching",
		"Attaching volume %s to replication policy %s", volumeID, pair.Spec.PolicyRef)

	patch := client.MergeFrom(pair.DeepCopy())
	pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateAttaching)
	pair.Status.Message = "PUT replication-policy sent; waiting for backend confirmation"
	if err := r.Status().Patch(ctx, pair, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: replPairRequeueAttaching}, nil
}

// reconcilePollAttach polls GET /{vol}/replication and advances to replicating when ready.
func (r *ReplicationPairReconciler) reconcilePollAttach(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	status, err := r.fetchReplicationStatus(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "poll GET replication failed; retrying", "pair", pair.Name)
		return ctrl.Result{RequeueAfter: replPairRequeueAttaching}, nil
	}
	if status == nil {
		// Backend has no replication relationship yet — keep waiting.
		return ctrl.Result{RequeueAfter: replPairRequeueAttaching}, nil
	}

	if status.State != "replicating" {
		log.Info("Waiting for replication to reach replicating state",
			"pair", pair.Name, "backendState", status.State)
		return ctrl.Result{RequeueAfter: replPairRequeueAttaching}, nil
	}

	direction := "source"
	if !status.IsSource {
		direction = "target"
	}

	now := metav1.Now()
	patch := client.MergeFrom(pair.DeepCopy())
	pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateReplicating)
	pair.Status.Direction = direction
	pair.Status.SourceLvolID = status.SourceLvolID
	pair.Status.TargetLvolID = status.TargetLvolID
	pair.Status.Message = "Replicating"
	pair.Status.LastReplicatedAt = &now
	if err := r.Status().Patch(ctx, pair, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	r.Recorder.Eventf(pair, nil, corev1.EventTypeNormal, "Replicating", "Replicating",
		"Volume %s is now replicating to policy %s", volumeID, pair.Spec.PolicyRef)
	return ctrl.Result{RequeueAfter: replPairRequeueReplicating}, nil
}

// reconcileReplicating syncs status.lastReplicatedAt from the backend and watches
// for external state transitions (cutover, failover).
func (r *ReplicationPairReconciler) reconcileReplicating(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	status, err := r.fetchReplicationStatus(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET replication failed during sync", "pair", pair.Name)
		return ctrl.Result{RequeueAfter: replPairRequeueReplicating}, nil
	}
	if status == nil {
		// Replication relationship gone on the backend unexpectedly.
		return r.setError(ctx, pair, "replication relationship no longer exists on backend")
	}

	patch := client.MergeFrom(pair.DeepCopy())
	changed := false

	// Reflect externally-triggered state transitions.
	switch status.State {
	case "replicating":
		// Steady state — update timestamp.
	case "cutover_pending":
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateCutoverPending)
		pair.Status.Message = "Cutover pending — awaiting replication_commit"
		changed = true
		r.Recorder.Eventf(pair, nil, corev1.EventTypeNormal, "CutoverPending", "CutoverPending",
			"Volume %s cutover is pending", volumeID)
	case "cutover_done":
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateCutoverDone)
		pair.Status.Message = "Cutover done"
		changed = true
	case "failed_over":
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateFailedOver)
		pair.Status.Direction = "target"
		pair.Status.TargetNQN = status.TargetNQN
		pair.Status.Message = "Failed over to target cluster"
		changed = true
		r.Recorder.Eventf(pair, nil, corev1.EventTypeNormal, "FailedOver", "FailedOver",
			"Volume %s has failed over; target NQN: %s", volumeID, status.TargetNQN)
	}

	// Update lastReplicatedAt if the backend reports a newer snapshot.
	if ts := parseLastSnapshotAt(status.LastSnapshotAt); ts != nil {
		if pair.Status.LastReplicatedAt == nil || ts.After(pair.Status.LastReplicatedAt.Time) {
			pair.Status.LastReplicatedAt = &metav1.Time{Time: *ts}
			changed = true
		}
	}

	if changed {
		if err := r.Status().Patch(ctx, pair, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	return ctrl.Result{RequeueAfter: replPairRequeueReplicating}, nil
}

// reconcileSyncStatus reflects cutover/failover backend state into CR status.
func (r *ReplicationPairReconciler) reconcileSyncStatus(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	status, err := r.fetchReplicationStatus(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET replication failed", "pair", pair.Name)
		return ctrl.Result{RequeueAfter: replPairRequeueReplicating}, nil
	}
	if status == nil {
		return ctrl.Result{RequeueAfter: replPairRequeueReplicating}, nil
	}

	patch := client.MergeFrom(pair.DeepCopy())
	pair.Status.TargetNQN = status.TargetNQN
	pair.Status.TargetLvolID = status.TargetLvolID

	// If the backend moved back to replicating (e.g. after failback), follow it.
	if status.State == "replicating" {
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateReplicating)
		direction := "source"
		if !status.IsSource {
			direction = "target"
		}
		pair.Status.Direction = direction
		pair.Status.Message = "Replicating"
	}

	if err := r.Status().Patch(ctx, pair, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: replPairRequeueReplicating}, nil
}

// reconcileDetach calls DELETE /{vol}/replication-policy and removes the finalizer
// so the pair CR can be GC'd.
func (r *ReplicationPairReconciler) reconcileDetach(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Transition to detaching state if not already there.
	if pair.Status.State != string(simplyblockv1alpha1.ReplicationPairStateDetaching) {
		patch := client.MergeFrom(pair.DeepCopy())
		pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateDetaching)
		pair.Status.Message = "Detaching replication policy"
		if err := r.Status().Patch(ctx, pair, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Nothing to call if we never got a volume handle (e.g. pair was never attached).
	clusterID, poolID, volumeID, ok := splitVolumeHandle(pair.Spec.VolumeID)
	if !ok {
		log.Info("No valid volume handle; skipping backend detach", "pair", pair.Name)
		return r.removePairFinalizer(ctx, pair)
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication-policy",
		clusterID, poolID, volumeID)
	body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
	if err != nil || (status >= 300 && status != http.StatusNotFound && status != http.StatusConflict) {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "DELETE replication-policy failed; retrying", "pair", pair.Name)
		return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
	}

	// 409 means a cutover is in flight; wait for it to settle.
	if status == http.StatusConflict {
		log.Info("DELETE replication-policy returned 409 (cutover in flight); retrying", "pair", pair.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	r.Recorder.Eventf(pair, nil, corev1.EventTypeNormal, "Detached", "Detached",
		"Replication detached for volume %s", volumeID)

	return r.removePairFinalizer(ctx, pair)
}

// removePairFinalizer removes the FinalizerReplicationPair and returns, allowing
// the pair CR to be garbage-collected.
func (r *ReplicationPairReconciler) removePairFinalizer(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(pair, utils.FinalizerReplicationPair) {
		controllerutil.RemoveFinalizer(pair, utils.FinalizerReplicationPair)
		return ctrl.Result{}, r.Update(ctx, pair)
	}
	return ctrl.Result{}, nil
}

// setError writes status.state = error and status.message.
func (r *ReplicationPairReconciler) setError(
	ctx context.Context,
	pair *simplyblockv1alpha1.ReplicationPair,
	msg string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(pair.DeepCopy())
	pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateError)
	pair.Status.Message = msg
	if err := r.Status().Patch(ctx, pair, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.Recorder.Eventf(pair, nil, corev1.EventTypeWarning, "Error", "Error",
		"ReplicationPair %s error: %s", pair.Name, msg)
	return ctrl.Result{RequeueAfter: replPairRequeueError}, nil
}

// fetchReplicationStatus calls GET /{vol}/replication and returns the parsed status,
// or nil if the backend returns 404 (no relationship established yet).
func (r *ReplicationPairReconciler) fetchReplicationStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (*replVolumeReplicationStatus, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication",
		clusterID, poolID, volumeID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status >= 300 {
		return nil, fmt.Errorf("status %d: %s", status, string(body))
	}
	var rs replVolumeReplicationStatus
	if err := json.Unmarshal(body, &rs); err != nil {
		return nil, fmt.Errorf("unmarshal replication status: %w", err)
	}
	return &rs, nil
}

// splitVolumeHandle parses a simplyblock CSI volume handle
// "<clusterUUID>:<poolUUID>:<volumeUUID>" into its components.
func splitVolumeHandle(handle string) (clusterID, poolID, volumeID string, ok bool) {
	parts := strings.SplitN(handle, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// parseLastSnapshotAt attempts to parse the last_snapshot_at field, which may be
// a RFC3339 string or null.
func parseLastSnapshotAt(v interface{}) *time.Time {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

func (r *ReplicationPairReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationPair{}).
		Named("replicationpair").
		Complete(r)
}
