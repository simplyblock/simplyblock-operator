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
	// replSlotRequeueReplicating is how often the reconciler syncs lastReplicatedAt
	// from the backend while a slot is in the steady-state replicating state.
	replSlotRequeueReplicating = 60 * time.Second
	// replSlotRequeueError is the back-off interval after a backend call fails.
	replSlotRequeueError = 30 * time.Second

	replMsgSlotReplicating = "Replicating"
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

// ReplicationSlotReconciler reconciles ReplicationSlot resources.
// It drives the per-volume state machine: attaching → replicating → (cutover / failover) → detaching.
type ReplicationSlotReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ReplicationSlotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var slot simplyblockv1alpha1.ReplicationSlot
	if err := r.Get(ctx, req.NamespacedName, &slot); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiClient := webapi.NewClient()

	if !slot.DeletionTimestamp.IsZero() {
		return r.reconcileDetach(ctx, &slot, apiClient)
	}

	if !controllerutil.ContainsFinalizer(&slot, utils.FinalizerReplicationSlot) {
		controllerutil.AddFinalizer(&slot, utils.FinalizerReplicationSlot)
		return ctrl.Result{}, r.Update(ctx, &slot)
	}

	// Fetch the owning ReplicationPolicy; fail fast if it is not ready.
	var policy simplyblockv1alpha1.ReplicationPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: slot.Spec.PolicyRef, Namespace: slot.Namespace}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setError(ctx, &slot, fmt.Sprintf("ReplicationPolicy %q not found", slot.Spec.PolicyRef))
		}
		return ctrl.Result{}, fmt.Errorf("get ReplicationPolicy %q: %w", slot.Spec.PolicyRef, err)
	}
	if !policy.Status.Ready {
		log.Info("ReplicationPolicy not ready; waiting", "policy", policy.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Parse the CSI volume handle stored in spec.volumeID: "<clusterID>:<poolID>:<volumeID>"
	clusterID, poolID, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID)
	if !ok {
		return r.setError(ctx, &slot, fmt.Sprintf("invalid VolumeID %q: expected <cluster>:<pool>:<volume>", slot.Spec.VolumeID))
	}

	switch simplyblockv1alpha1.ReplicationSlotState(slot.Status.State) {
	case "": // brand-new slot
		return r.reconcileAttach(ctx, &slot, &policy, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationSlotStateAttaching:
		return r.reconcilePollAttach(ctx, &slot, volumeID)
	case simplyblockv1alpha1.ReplicationSlotStateReplicating:
		return r.reconcileReplicating(ctx, &slot, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationSlotStateCutoverPending,
		simplyblockv1alpha1.ReplicationSlotStateCutoverDone,
		simplyblockv1alpha1.ReplicationSlotStateFailedOver:
		return r.reconcileSyncStatus(ctx, &slot, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationSlotStateDetaching:
		return r.reconcileDetach(ctx, &slot, apiClient)
	case simplyblockv1alpha1.ReplicationSlotStateError:
		log.Info("ReplicationSlot in error state; retrying attach", "slot", slot.Name)
		return r.reconcileAttach(ctx, &slot, &policy, apiClient, clusterID, poolID, volumeID)
	default:
		return r.setError(ctx, &slot, fmt.Sprintf("unknown state %q", slot.Status.State))
	}
}

// reconcileAttach sends PUT /{vol} with replication_policy_id.
// The sbcli PUT is synchronous — attach_policy/replication_start complete before the response
// returns — so a successful 2xx means replication is active and we can transition directly to
// replicating without a polling phase.
func (r *ReplicationSlotReconciler) reconcileAttach(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	policy *simplyblockv1alpha1.ReplicationPolicy,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s",
		clusterID, poolID, volumeID)
	reqBody := map[string]interface{}{
		"replication_policy_id": policy.Status.BackendPolicyID,
	}
	body, status, err := apiClient.Do(ctx, http.MethodPut, endpoint, reqBody)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "PUT volume replication_policy_id failed", "slot", slot.Name)
		return r.setError(ctx, slot, fmt.Sprintf("attach failed: %v", err))
	}

	now := metav1.Now()
	patch := client.MergeFrom(slot.DeepCopy())
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
	slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
	slot.Status.Message = replMsgSlotReplicating
	slot.Status.LastReplicatedAt = &now
	if err := r.Status().Patch(ctx, slot, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}

	r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "Replicating", "Replicating",
		"Volume %s is now replicating to policy %s", volumeID, slot.Spec.PolicyRef)
	return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
}

// reconcilePollAttach handles slots still in the legacy "attaching" state.
// The LVolReplication object only exists on the backend after cutover/failover, so
// polling GET .../replication in this phase always returns 404. Transition immediately.
func (r *ReplicationSlotReconciler) reconcilePollAttach(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	volumeID string,
) (ctrl.Result, error) {
	now := metav1.Now()
	patch := client.MergeFrom(slot.DeepCopy())
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
	slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
	slot.Status.Message = replMsgSlotReplicating
	slot.Status.LastReplicatedAt = &now
	if err := r.Status().Patch(ctx, slot, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "Replicating", "Replicating",
		"Volume %s is now replicating to policy %s", volumeID, slot.Spec.PolicyRef)
	return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
}

// reconcileReplicating syncs status.lastReplicatedAt from the backend and watches
// for external state transitions (cutover, failover).
func (r *ReplicationSlotReconciler) reconcileReplicating(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	status, err := r.fetchReplicationStatus(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET replication failed during sync", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
	}
	if status == nil {
		// The LVolReplication object only exists after a cutover or failover — during normal
		// replication the backend returns 404. This is expected; keep polling.
		return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
	}

	patch := client.MergeFrom(slot.DeepCopy())
	changed := false

	switch status.State {
	case utils.ReplicationBackendStateReplicating:
		// Steady state — update timestamp below.
	case "cutover_pending":
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending)
		slot.Status.Message = "Cutover pending — awaiting replication_commit"
		changed = true
		r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "CutoverPending", "CutoverPending",
			"Volume %s cutover is pending", volumeID)
	case "cutover_done":
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone)
		slot.Status.Message = "Cutover done"
		changed = true
	case "failed_over":
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		slot.Status.TargetNQN = status.TargetNQN
		slot.Status.Message = "Failed over to target cluster"
		changed = true
		r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "FailedOver", "FailedOver",
			"Volume %s has failed over; target NQN: %s", volumeID, status.TargetNQN)
	}

	if ts := parseLastSnapshotAt(status.LastSnapshotAt); ts != nil {
		if slot.Status.LastReplicatedAt == nil || ts.After(slot.Status.LastReplicatedAt.Time) {
			slot.Status.LastReplicatedAt = &metav1.Time{Time: *ts}
			changed = true
		}
	}

	if changed {
		if err := r.Status().Patch(ctx, slot, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
}

// reconcileSyncStatus reflects cutover/failover backend state into slot status.
func (r *ReplicationSlotReconciler) reconcileSyncStatus(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	status, err := r.fetchReplicationStatus(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET replication failed", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
	}
	if status == nil {
		return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
	}

	patch := client.MergeFrom(slot.DeepCopy())
	slot.Status.TargetNQN = status.TargetNQN
	slot.Status.TargetLvolID = status.TargetLvolID

	if status.State == utils.ReplicationBackendStateReplicating {
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
		direction := string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
		if !status.IsSource {
			direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		}
		slot.Status.Direction = direction
		slot.Status.Message = replMsgSlotReplicating
	}

	if err := r.Status().Patch(ctx, slot, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
}

// reconcileDetach sends PUT /{vol} with replication_policy_id=null and removes the
// finalizer so the slot CR can be GC'd.
func (r *ReplicationSlotReconciler) reconcileDetach(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if slot.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateDetaching) {
		patch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateDetaching)
		slot.Status.Message = "Detaching replication policy"
		if err := r.Status().Patch(ctx, slot, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	clusterID, poolID, volumeID, ok := splitVolumeHandle(slot.Spec.VolumeID)
	if !ok {
		log.Info("No valid volume handle; skipping backend detach", "slot", slot.Name)
		return r.removeSlotFinalizer(ctx, slot)
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s",
		clusterID, poolID, volumeID)
	reqBody := map[string]interface{}{
		"replication_policy_id": nil,
	}
	body, status, err := apiClient.Do(ctx, http.MethodPut, endpoint, reqBody)
	if err != nil || (status >= 300 && status != http.StatusNotFound && status != http.StatusConflict) {
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, string(body))
		}
		log.Error(err, "PUT volume replication_policy_id=null failed; retrying", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}

	if status == http.StatusConflict {
		log.Info("PUT replication_policy_id=null returned 409 (cutover in flight); retrying", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "Detached", "Detached",
		"Replication detached for volume %s", volumeID)
	return r.removeSlotFinalizer(ctx, slot)
}

func (r *ReplicationSlotReconciler) removeSlotFinalizer(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(slot, utils.FinalizerReplicationSlot) {
		controllerutil.RemoveFinalizer(slot, utils.FinalizerReplicationSlot)
		return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, slot))
	}
	return ctrl.Result{}, nil
}

func (r *ReplicationSlotReconciler) setError(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	msg string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(slot.DeepCopy())
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateError)
	slot.Status.Message = msg
	if err := r.Status().Patch(ctx, slot, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	r.Recorder.Eventf(slot, nil, corev1.EventTypeWarning, "Error", "Error",
		"ReplicationSlot %s error: %s", slot.Name, msg)
	return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
}

func (r *ReplicationSlotReconciler) fetchReplicationStatus(
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
// "<clusterUUID>:<poolUUID>:<volumeUUID>" into its three components.
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

func (r *ReplicationSlotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationSlot{}).
		Named("replicationslot").
		Complete(r)
}
