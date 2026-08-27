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

	batchv1 "k8s.io/api/batch/v1"
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
	// 15s gives a reasonable chance of catching the cutover_pending state before
	// the backend's REPL_CUTOVER_PROCEED_TIMEOUT_SEC safety timeout fires.
	replSlotRequeueReplicating = 15 * time.Second
	// replSlotRequeueError is the back-off interval after a backend call fails.
	replSlotRequeueError = 30 * time.Second

	replMsgSlotReplicating = "Replicating"

	// annotCutoverProceedSignaled is set on a slot once the operator has called
	// POST .../replication/cutover-proceed. Prevents repeated calls (and log
	// spam) while the backend transitions from cutover_pending to cutover_done.
	annotCutoverProceedSignaled      = "replication.simplyblock.io/cutover-proceed-signaled"
	annotCutoverProceedSignaledValue = "true"

	// annotFailbackTarget stores the target cluster volume handle
	// ("<clusterID>:<poolID>:<volumeID>") set during reconcileFailback. During
	// failback the cutover_pending task runs on the target cluster, not the
	// source, so reconcileCutoverPending must poll the target cluster and call
	// cutover-proceed there rather than on the source cluster.
	annotFailbackTarget = "replication.simplyblock.io/failback-target"

	backendStateCutoverPending = "cutover_pending"
	backendStateCutoverDone    = "cutover_done"
	backendStateFailedOver     = "failed_over"
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
	// apiReader is an uncached reader for consumer-pod lookups; a stale cache
	// could miss a running pod and skip preconnect on an actively-used volume.
	apiReader client.Reader
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationslots/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=replicationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
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
	case simplyblockv1alpha1.ReplicationSlotStateCutoverPending:
		return r.reconcileCutoverPending(ctx, &slot, apiClient, clusterID, poolID, volumeID)
	case simplyblockv1alpha1.ReplicationSlotStateCutoverDone,
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
	case backendStateCutoverPending:
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending)
		slot.Status.Message = "Cutover pending — awaiting replication_commit"
		changed = true
		r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "CutoverPending", "CutoverPending",
			"Volume %s cutover is pending", volumeID)
	case backendStateCutoverDone:
		if !status.IsSource {
			// This volume is the TARGET of a completed failback — it is live again as
			// the source. Treat identically to reconcileSyncStatus so both paths agree.
			slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
			slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
			slot.Status.Message = replMsgSlotReplicating
		} else {
			slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone)
			slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
			slot.Status.Message = "Cutover done"
		}
		changed = true
	case backendStateFailedOver:
		if !status.IsSource {
			// Failback completed — IO returned to this volume; treat as replicating/source.
			slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
			slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
			slot.Status.Message = replMsgSlotReplicating
		} else {
			slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
			slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
			slot.Status.TargetNQN = status.TargetNQN
			slot.Status.Message = "Failed over to target cluster"
			r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "FailedOver", "FailedOver",
				"Volume %s has failed over; target NQN: %s", volumeID, status.TargetNQN)
		}
		changed = true
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
		switch status.State {
		case backendStateCutoverPending:
			// The backend's REPL_CUTOVER_PROCEED_TIMEOUT_SEC safety timer (≈30 s) is
			// shorter than even the 15 s poll interval. Jump directly into the cutover
			// handler in this same iteration so the preconnect Job is created before
			// the window expires.
			return r.reconcileCutoverPending(ctx, slot, apiClient, clusterID, poolID, volumeID)
		case backendStateCutoverDone:
			// The 30 s cutover_pending window already closed before we polled.
			// Attempt a late preconnect so the CSI node gets target NVMe paths even
			// after the ANA flip — this limits IO downtime to ctrl_loss_tmo instead
			// of indefinite path loss.
			r.reconcilePreconnect(ctx, slot, apiClient, clusterID, poolID, volumeID)
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

	switch status.State {
	case utils.ReplicationBackendStateReplicating:
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
		direction := string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
		if !status.IsSource {
			direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		}
		slot.Status.Direction = direction
		slot.Status.Message = replMsgSlotReplicating
	case backendStateCutoverPending:
		// The backend entered cutover_pending during a failback reverse cutover.
		// Advance the slot so reconcileCutoverPending can run the preconnect job
		// and signal cutover-proceed before the ANA flip.
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending)
		slot.Status.Message = "Cutover pending — failback reverse cutover in progress"
	case backendStateCutoverDone, backendStateFailedOver:
		if !status.IsSource {
			// This volume is the TARGET of a completed failback: it received the
			// replicated data and the ANA flip moved IO back here. From the slot's
			// perspective the volume is live on the source cluster again.
			slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
			slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
			slot.Status.Message = replMsgSlotReplicating
			log.Info("Failback cutover detected: slot transitioning back to replicating/source",
				"slot", slot.Name, "backendState", status.State)
		}
	}

	if err := r.Status().Patch(ctx, slot, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	if status.State == backendStateCutoverPending {
		return r.reconcileCutoverPending(ctx, slot, apiClient, clusterID, poolID, volumeID)
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

// reconcileCutoverPending handles the cutover_pending state.
//
// The backend task suspends waiting for POST .../replication/cutover-proceed.
// This reconciler pre-connects the target NVMe paths via a preconnect Job, then
// signals the backend via callCutoverProceed so the ANA flip happens only after
// the target controllers are already connected. A safety timeout on the backend
// side (REPL_CUTOVER_PROCEED_TIMEOUT_SEC) lets cutover proceed even if the
// operator is unavailable.
func (r *ReplicationSlotReconciler) reconcileCutoverPending(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// For failback the cutover task runs on the target cluster, not the source.
	// reconcileFailback stores the target volume handle in annotFailbackTarget so
	// we poll and signal the right cluster instead of the source cluster (which
	// only sees the old failed_over state and would immediately revert the slot).
	proceedClusterID, proceedPoolID, proceedVolumeID := clusterID, poolID, volumeID
	if fbTarget := slot.Annotations[annotFailbackTarget]; fbTarget != "" {
		if tc, tp, tv, ok := splitVolumeHandle(fbTarget); ok {
			proceedClusterID, proceedPoolID, proceedVolumeID = tc, tp, tv
		}
	}

	// Re-check backend state; the backend may have already cut over before we ran.
	status, err := r.fetchReplicationStatus(ctx, apiClient, proceedClusterID, proceedPoolID, proceedVolumeID)
	if err != nil {
		log.Error(err, "GET replication failed in cutover_pending", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}
	if status == nil || status.State != backendStateCutoverPending {
		// Backend advanced past cutover_pending before the preconnect Job could be
		// created (safety timer fired, or we missed the window entirely). Attempt a
		// late preconnect so the CSI node gets target NVMe paths even after the ANA
		// flip — this limits IO downtime to ctrl_loss_tmo instead of indefinite.
		if status != nil && status.State == backendStateCutoverDone {
			r.reconcilePreconnect(ctx, slot, apiClient, clusterID, poolID, volumeID)
		}
		return r.applyAdvancedBackendStateForFailback(ctx, slot, status, proceedClusterID != clusterID)
	}

	// Fetch connection strings from the source cluster volume. During cutover_pending
	// the backend returns both paths (the ones to pre-connect first). For migration
	// these are target paths; for failback these are source paths — both correct.
	rawConns, err := apiClient.GetVolumeConnections(ctx, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET volume/connect failed in cutover_pending", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}
	conns := lvolConnRespToVmigConns(rawConns)
	if len(conns) == 0 {
		// No connections returned; backend may not have populated them yet.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	jobName := replSlotPreconnectJobName(volumeID)

	// Check if the preconnect Job already exists.
	var existingJob batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: slot.Namespace, Name: jobName}, &existingJob); err == nil {
		// Job exists — check its terminal state. We always check, even when the
		// annotation is already set, so that a previously-failed callCutoverProceed
		// is retried rather than silently dropped: the backend waits indefinitely for
		// the proceed signal, so a missed call leaves the slot permanently stuck.
		for _, c := range existingJob.Status.Conditions {
			if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
				// Claim the signal by writing the annotation before the API call.
				// Two concurrent reconciles may both see the job complete before
				// either patch lands; the one whose patch wins becomes the sole
				// caller of callCutoverProceed — the loser's patch fails (conflict),
				// and it falls back to the 5 s wait.
				result, patchErr := r.markCutoverProceedSignaled(ctx, slot)
				if patchErr != nil {
					// Conflict: another reconcile already claimed it.
					return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
				}
				if c.Type == batchv1.JobComplete {
					log.Info("Preconnect job succeeded; signalling backend to proceed", "job", jobName)
				} else {
					log.Info("Preconnect job failed; signalling backend to proceed anyway", "job", jobName)
				}
				if err := r.callCutoverProceed(ctx, apiClient, proceedClusterID, proceedPoolID, proceedVolumeID); err != nil {
					log.Error(err, "POST cutover-proceed failed", "slot", slot.Name)
				}
				// Delete the job immediately now that the signal is sent — don't
				// leave completed jobs accumulating when there are many PVCs.
				if delErr := r.Delete(ctx, &existingJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
					log.Error(delErr, "Failed to delete preconnect job after signal", "job", jobName)
				}
				return result, nil
			}
		}
		// Job is still running or pending — wait for it.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get preconnect job %q: %w", jobName, err)
	}

	// Job not found — either the signal was already sent and the job was deleted,
	// or the job was never created yet. If we already signalled, don't create a new
	// job: just wait for the backend to advance (a new job can't help at this point).
	if slot.Annotations[annotCutoverProceedSignaled] == annotCutoverProceedSignaledValue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Find the node running a pod that has this volume mounted.
	node, err := findConsumerNode(ctx, r.apiReader, volumeID)
	if err != nil {
		log.Error(err, "Cannot resolve consuming node for preconnect", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}
	if node == "" {
		if slot.Annotations[annotCutoverProceedSignaled] != annotCutoverProceedSignaledValue {
			// No active consumer; nothing to pre-connect — signal immediately.
			log.Info("No active consumer for preconnect; signalling backend to proceed", "slot", slot.Name)
			if err := r.callCutoverProceed(ctx, apiClient, proceedClusterID, proceedPoolID, proceedVolumeID); err != nil {
				log.Error(err, "POST cutover-proceed failed (no consumer)", "slot", slot.Name)
				return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
			}
			return r.markCutoverProceedSignaled(ctx, slot)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	image, err := resolveRebalancerImage(ctx, r.Client, slot.Namespace, clusterID)
	if err != nil {
		log.Error(err, "Cannot resolve rebalancer image for preconnect", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}

	connsJSON, err := json.Marshal(conns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal connections for preconnect job: %w", err)
	}

	job := buildRebalancerJob(rebalancerJobParams{
		Name:          jobName,
		Namespace:     slot.Namespace,
		OwnerRef:      *metav1.NewControllerRef(slot, simplyblockv1alpha1.GroupVersion.WithKind("ReplicationSlot")),
		Hostname:      node,
		Image:         image,
		ContainerName: "replication-preconnect",
		Mode:          "replication-preconnect",
		Env: []corev1.EnvVar{
			{Name: "REPL_CONNECTIONS", Value: string(connsJSON)},
			{Name: "VMIG_SYS_ROOT", Value: "/host/sys"},
		},
		BackoffLimit: 1,
		TTL:          150,
		Deadline:     120,
	})
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create preconnect job %q: %w", jobName, err)
	}

	log.Info("Preconnect job created", "job", jobName, "node", node,
		"connections", len(conns))
	r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "PreconnectStarted", "PreconnectStarted",
		"Connecting target NVMe paths on node %s before cutover of volume %s", node, volumeID)

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// reconcilePreconnect creates the preconnect Job that connects target NVMe paths
// on the consumer node. Called both during cutover_pending and as a fallback when
// the backend has already flipped to cutover_done (the "late" case — ANA already
// happened, limiting IO downtime to ctrl_loss_tmo instead of indefinitely).
// Errors are logged but not returned — the caller still advances slot state.
func (r *ReplicationSlotReconciler) reconcilePreconnect(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) {
	log := logf.FromContext(ctx)
	jobName := replSlotPreconnectJobName(volumeID)

	// Skip if the Job was already created (e.g. by a concurrent reconcile).
	var existing batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: slot.Namespace, Name: jobName}, &existing); err == nil {
		return
	}

	rawConns, err := apiClient.GetVolumeConnections(ctx, clusterID, poolID, volumeID)
	if err != nil || len(rawConns) == 0 {
		log.Error(err, "Preconnect: cannot fetch connections", "slot", slot.Name)
		return
	}
	conns := lvolConnRespToVmigConns(rawConns)
	node, err := findConsumerNode(ctx, r.apiReader, volumeID)
	if err != nil || node == "" {
		return // no active consumer; nothing to connect
	}
	image, err := resolveRebalancerImage(ctx, r.Client, slot.Namespace, clusterID)
	if err != nil {
		log.Error(err, "Preconnect: cannot resolve rebalancer image", "slot", slot.Name)
		return
	}
	connsJSON, err := json.Marshal(conns)
	if err != nil {
		return
	}
	job := buildRebalancerJob(rebalancerJobParams{
		Name:          jobName,
		Namespace:     slot.Namespace,
		OwnerRef:      *metav1.NewControllerRef(slot, simplyblockv1alpha1.GroupVersion.WithKind("ReplicationSlot")),
		Hostname:      node,
		Image:         image,
		ContainerName: "replication-preconnect",
		Mode:          "replication-preconnect",
		Env: []corev1.EnvVar{
			{Name: "REPL_CONNECTIONS", Value: string(connsJSON)},
			{Name: "VMIG_SYS_ROOT", Value: "/host/sys"},
		},
		BackoffLimit: 1,
		TTL:          150,
		Deadline:     120,
	})
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		log.Error(err, "Late preconnect: job creation failed", "slot", slot.Name)
		return
	}
	log.Info("Late preconnect job created after ANA flip (cutover_done)",
		"job", jobName, "node", node, "connections", len(conns))
	r.Recorder.Eventf(slot, nil, corev1.EventTypeWarning, "LatePreconnect", "LatePreconnect",
		"Connecting target NVMe paths after ANA flip on node %s (preconnect window was missed)", node)
}

// replSlotPreconnectJobName returns a stable, unique Job name for a volume's preconnect Job.
func replSlotPreconnectJobName(volumeID string) string {
	s := strings.ReplaceAll(volumeID, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return "replslot-preconnect-" + s
}

// markCutoverProceedSignaled sets annotCutoverProceedSignaled on the slot so
// subsequent reconciles skip the callCutoverProceed call while waiting for the
// backend to transition from cutover_pending to cutover_done.
func (r *ReplicationSlotReconciler) markCutoverProceedSignaled(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
) (ctrl.Result, error) {
	patch := client.MergeFrom(slot.DeepCopy())
	if slot.Annotations == nil {
		slot.Annotations = map[string]string{}
	}
	slot.Annotations[annotCutoverProceedSignaled] = annotCutoverProceedSignaledValue
	if err := r.Patch(ctx, slot, patch); err != nil {
		// Return the error so callers that patch-before-signal can detect the
		// conflict and skip the API call (another reconcile already owns it).
		return ctrl.Result{Requeue: true}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// callCutoverProceed signals the backend that target NVMe paths are connected
// and the ANA flip may proceed. It is idempotent: the backend ignores duplicate
// signals once cutover_proceed is already true.
func (r *ReplicationSlotReconciler) callCutoverProceed(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) error {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/replication/cutover-proceed",
		clusterID, poolID, volumeID)
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	// 200/204 = signalled; 404 = no cutover_pending record (already advanced).
	// The Flask backend may return 200 OK rather than 204 No Content.
	if status == http.StatusOK || status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("POST cutover-proceed: status %d: %s", status, string(body))
}

// applyAdvancedBackendState transitions the slot from cutover_pending to whatever
// state the backend now reports. reconcileSyncStatus cannot be used here because it
// only transitions to replicating — calling it from cutover_pending would leave the
// slot permanently stuck.
func (r *ReplicationSlotReconciler) applyAdvancedBackendState(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	status *replVolumeReplicationStatus,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if status == nil {
		// LVolReplication not found yet — backend may still be creating it; retry.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	patch := client.MergeFrom(slot.DeepCopy())
	switch status.State {
	case backendStateCutoverDone:
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		slot.Status.Message = "Cutover done"
		slot.Status.TargetNQN = status.TargetNQN
		slot.Status.TargetLvolID = status.TargetLvolID
		log.Info("Cutover completed; advancing slot state to cutover_done", "slot", slot.Name)
	case backendStateFailedOver:
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionTarget)
		slot.Status.Message = "Failed over to target cluster"
		slot.Status.TargetNQN = status.TargetNQN
		slot.Status.TargetLvolID = status.TargetLvolID
		log.Info("Slot failed over; advancing slot state to failed_over", "slot", slot.Name)
		r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "FailedOver", "FailedOver",
			"Volume failed over; target NQN: %s", status.TargetNQN)
	default:
		slot.Status.TargetNQN = status.TargetNQN
		slot.Status.TargetLvolID = status.TargetLvolID
		log.Info("Backend advanced from cutover_pending to unexpected state",
			"slot", slot.Name, "backendState", status.State)
	}
	if err := r.Status().Patch(ctx, slot, patch); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
}

// applyAdvancedBackendStateForFailback is the failback-aware variant called from
// reconcileCutoverPending when the slot was initiated by a failback ReplicationOps.
// We poll the TARGET cluster (where the task runs), so cutover_done/is_source=true
// means the ANA flip moved IO back to the original source — the slot is done and
// should return to replicating/source. For non-failback paths (isFailback=false)
// it delegates to the regular applyAdvancedBackendState.
func (r *ReplicationSlotReconciler) applyAdvancedBackendStateForFailback(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	status *replVolumeReplicationStatus,
	isFailback bool,
) (ctrl.Result, error) {
	if !isFailback {
		return r.applyAdvancedBackendState(ctx, slot, status)
	}

	log := logf.FromContext(ctx)

	if status == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	switch status.State {
	case backendStateCutoverDone, backendStateFailedOver:
		// Failback ANA flip complete — IO is back on the original source cluster.
		patch := client.MergeFrom(slot.DeepCopy())
		slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
		slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
		slot.Status.Message = replMsgSlotReplicating
		// Clear the failback annotation; it is no longer needed.
		delete(slot.Annotations, annotFailbackTarget)
		delete(slot.Annotations, annotCutoverProceedSignaled)
		log.Info("Failback cutover complete; slot returning to replicating/source", "slot", slot.Name)
		r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "FailbackComplete", "FailbackComplete",
			"Failback cutover done; volume is live on source cluster again")
		if err := r.Status().Patch(ctx, slot, patch); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: replSlotRequeueReplicating}, nil
	default:
		// Still in an intermediate state (e.g. backend not yet cutover_done); wait.
		log.Info("Failback: unexpected backend state while waiting for cutover_done",
			"slot", slot.Name, "backendState", status.State)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *ReplicationSlotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.apiReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ReplicationSlot{}).
		Owns(&batchv1.Job{}).
		Named("replicationslot").
		Complete(r)
}
