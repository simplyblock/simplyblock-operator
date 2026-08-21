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
	"sort"
	"strings"
	"time"

	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
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
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
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

// reconcileCutoverPending handles the cutover_pending state.
//
// The backend task is holding for REPL_CUTOVER_PRECONNECT_WAIT_SEC after setting
// cutover_pending, giving us a window to pre-connect the target NVMe paths on every
// node that has the volume mounted. When the backend's deadline expires it proceeds
// with the ANA flip regardless, so this is best-effort on the operator side.
func (r *ReplicationSlotReconciler) reconcileCutoverPending(
	ctx context.Context,
	slot *simplyblockv1alpha1.ReplicationSlot,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Re-check backend state; the backend may have already cut over before we ran.
	status, err := r.fetchReplicationStatus(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET replication failed in cutover_pending", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}
	if status == nil || status.State != "cutover_pending" {
		// Backend already advanced; sync status normally.
		return r.reconcileSyncStatus(ctx, slot, apiClient, clusterID, poolID, volumeID)
	}

	// Fetch connection strings. During cutover_pending the backend returns both
	// target and source paths (target first). EnsureMigrationPaths is idempotent
	// for already-connected source paths, so passing all 4 is safe.
	conns, err := r.fetchVolumeConnections(ctx, apiClient, clusterID, poolID, volumeID)
	if err != nil {
		log.Error(err, "GET volume/connect failed in cutover_pending", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}
	if len(conns) == 0 {
		// No connections returned; backend may not have populated them yet.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	jobName := replSlotPreconnectJobName(volumeID)

	// Check if the preconnect Job already exists.
	var existingJob batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: slot.Namespace, Name: jobName}, &existingJob); err == nil {
		// Job exists — check its terminal state.
		for _, c := range existingJob.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				log.Info("Preconnect job succeeded", "job", jobName)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				log.Info("Preconnect job failed (backend will proceed anyway)", "job", jobName)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
		// Still running.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get preconnect job %q: %w", jobName, err)
	}

	// Find the node running a pod that has this volume mounted.
	node, err := r.findPreconnectConsumerNode(ctx, clusterID, volumeID)
	if err != nil {
		log.Error(err, "Cannot resolve consuming node for preconnect", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}
	if node == "" {
		// No active consumer; nothing to pre-connect.
		log.Info("No active consumer for preconnect; skipping", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	image, err := r.resolvePreconnectImage(ctx, slot.Namespace, clusterID)
	if err != nil {
		log.Error(err, "Cannot resolve rebalancer image for preconnect", "slot", slot.Name)
		return ctrl.Result{RequeueAfter: replSlotRequeueError}, nil
	}

	connsJSON, err := json.Marshal(conns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal connections for preconnect job: %w", err)
	}

	job := r.buildPreconnectJob(slot, jobName, node, image, string(connsJSON))
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create preconnect job %q: %w", jobName, err)
	}

	log.Info("Preconnect job created", "job", jobName, "node", node,
		"connections", len(conns))
	r.Recorder.Eventf(slot, nil, corev1.EventTypeNormal, "PreconnectStarted", "PreconnectStarted",
		"Connecting target NVMe paths on node %s before cutover of volume %s", node, volumeID)

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// replSlotPreconnectJobName returns a stable, unique Job name for a volume's preconnect Job.
func replSlotPreconnectJobName(volumeID string) string {
	s := strings.ReplaceAll(volumeID, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return "replslot-preconnect-" + s
}

// backendVolumeConnection is the JSON shape of one entry from GET .../volumes/{id}/connect.
// The backend serialises NvmeConnectEntry with hyphenated field names (alias_generator).
type backendVolumeConnection struct {
	Transport      string `json:"transport"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	NQN            string `json:"nqn"`
	NrIoQueues     int    `json:"nr-io-queues"`
	ReconnectDelay int    `json:"reconnect-delay"`
	CtrlLossTmo    int    `json:"ctrl-loss-tmo"`
	FastIOFailTmo  int    `json:"fast-io-fail-tmo"`
	KeepAliveTmo   int    `json:"keep-alive-tmo"`
}

// fetchVolumeConnections calls GET .../volumes/{id}/connect and returns the paths
// as volumemigration.Connection objects ready for the preconnect Job.
func (r *ReplicationSlotReconciler) fetchVolumeConnections(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterID, poolID, volumeID string,
) ([]vmigration.Connection, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/connect",
		clusterID, poolID, volumeID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("GET connect: status %d: %s", status, string(body))
	}

	var raw []backendVolumeConnection
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse connect response: %w", err)
	}

	out := make([]vmigration.Connection, len(raw))
	for i, c := range raw {
		out[i] = vmigration.Connection{
			NQN:            c.NQN,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.Transport,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			CtrlLossTmo:    c.CtrlLossTmo,
			FastIOFailTmo:  c.FastIOFailTmo,
			KeepAliveTmo:   c.KeepAliveTmo,
		}
	}
	return out, nil
}

// findPreconnectConsumerNode finds the Kubernetes node name running a pod that
// has the given volume's PVC mounted. Returns "" when no active consumer exists.
func (r *ReplicationSlotReconciler) findPreconnectConsumerNode(
	ctx context.Context,
	clusterID, volumeID string,
) (string, error) {
	// Find the PersistentVolume whose CSI handle encodes this volume.
	var pvList corev1.PersistentVolumeList
	if err := r.apiReader.List(ctx, &pvList); err != nil {
		return "", fmt.Errorf("list PersistentVolumes: %w", err)
	}

	var pvName, pvcName, pvcNamespace string
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil {
			continue
		}
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 || parts[2] != volumeID {
			continue
		}
		if pv.Spec.ClaimRef == nil {
			continue
		}
		pvName = pv.Name
		pvcName = pv.Spec.ClaimRef.Name
		pvcNamespace = pv.Spec.ClaimRef.Namespace
		break
	}
	if pvName == "" {
		return "", nil // no PV for this volume
	}

	// Find a Running pod that uses the PVC.
	var podList corev1.PodList
	if err := r.apiReader.List(ctx, &podList, client.InNamespace(pvcNamespace)); err != nil {
		return "", fmt.Errorf("list pods in %s: %w", pvcNamespace, err)
	}

	var nodes []string
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				nodes = append(nodes, pod.Spec.NodeName)
				break
			}
		}
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return "", nil
	}
	return nodes[0], nil
}

// resolvePreconnectImage returns the simplyblock-rebalancer image from the
// StorageCluster, falling back to the default image when none is configured.
func (r *ReplicationSlotReconciler) resolvePreconnectImage(
	ctx context.Context,
	namespace, clusterUUID string,
) (string, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID != clusterUUID {
			continue
		}
		vm := cr.Spec.VolumeMigrationSettings
		if vm != nil && vm.RebalancerImage != nil && *vm.RebalancerImage != "" {
			return *vm.RebalancerImage, nil
		}
		break
	}
	return defaultRebalancerImage, nil
}

// buildPreconnectJob creates the Job that connects target NVMe paths on the
// consuming node before the cutover ANA flip.
func (r *ReplicationSlotReconciler) buildPreconnectJob(
	slot *simplyblockv1alpha1.ReplicationSlot,
	jobName, hostname, image, connsJSON string,
) *batchv1.Job {
	privileged := true
	readOnly := true
	ttl := int32(3600)
	deadline := int64(120)
	backoffLimit := int32(1)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: slot.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(slot, simplyblockv1alpha1.GroupVersion.WithKind("ReplicationSlot")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": hostname},
					HostNetwork:   true,
					Volumes: []corev1.Volume{
						{
							Name: "host-sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/sys"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "replication-preconnect",
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"simplyblock-rebalancer", "--mode=replication-preconnect"},
							Env: []corev1.EnvVar{
								{Name: "REPL_CONNECTIONS", Value: connsJSON},
								{Name: "VMIG_SYS_ROOT", Value: "/host/sys"},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-sys", MountPath: "/host/sys", ReadOnly: readOnly},
							},
						},
					},
				},
			},
		},
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
