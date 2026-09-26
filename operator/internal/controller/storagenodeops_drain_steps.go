// The two drain steps the control plane used to do inside its node delete.
//
// A removal is four things: the node's devices are failed and rebuilt onto peers,
// its volumes are drained off it, the surviving volumes' replica roles are
// reallocated away from it, and then the node record goes. Only the middle one
// was ever the operator's -- the other two happened inside DELETE
// /storage-nodes/{id}, which meant a removal showed "Removing" for as long as
// they took, with no progress, no events, and nothing to fail on but the whole
// delete.
//
// Each step here is a POST that starts the work and a GET that reports it, so
// the phase can be watched and can fail on its own. Both are idempotent on the
// control-plane side: a POST for work already running is a no-op, which is what
// lets a latch-and-poll controller survive its own restarts.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// The device rebuild moves real data and takes minutes, so it is polled far
	// less often than the sub-second steps around it.
	drainRequeueDevices   = 20 * time.Second
)

// drainProgress is what both step endpoints report. Done is authoritative: a
// control plane that cannot count the work still has to say when it is finished.
type drainProgress struct {
	Done      bool   `json:"done"`
	Total     int    `json:"total,omitempty"`
	Completed int    `json:"completed,omitempty"`
	Failed    int    `json:"failed,omitempty"`
	Message   string `json:"message,omitempty"`
}

// fetchDrainProgress GETs a step endpoint and decodes its progress.
func fetchDrainProgress(
	ctx context.Context, apiClient *webapi.Client, endpoint string,
) (drainProgress, error) {
	var p drainProgress
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return p, err
	}
	if status >= 300 {
		return p, fmt.Errorf("status %d", status)
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("decoding progress: %w", err)
	}
	return p, nil
}

// drainMigrateDevices fails the node's devices and rebuilds their data onto its
// peers, before any volume is moved.
//
// The volumes still hosted here keep serving from their replicas while it runs.
// That is slower than being served locally -- measured at roughly three orders
// of magnitude on a 7-node cluster -- so the phase publishes how far along it is
// and how many devices are left, which is the only thing that makes the slowdown
// legible to whoever is watching the removal.
func (r *StorageNodeOpsReconciler) drainMigrateDevices(
	ctx context.Context,
	ops *simplyblockv1alpha1.StorageNodeOps,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeUUID := sn.Status.UUID
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/migrate-devices",
		clusterUUID, nodeUUID)

	if !ops.Status.DevicesTriggered {
		_, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("status %d", status)
			}
			log.Error(err, "drain: failed to start device migration", "node", nodeUUID)
			r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning, "DeviceMigrationStartFailed",
				"DeviceMigrationStartFailed", "could not start device migration on %s: %v", nodeUUID, err)
			return ctrl.Result{RequeueAfter: drainRequeueDevices}, nil
		}

		patch := client.MergeFrom(ops.DeepCopy())
		ops.Status.DevicesTriggered = true
		ops.Status.Message = "MigratingDevices: rebuilding this node's devices onto its peers"
		_ = r.Status().Patch(ctx, ops, patch)

		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "DeviceMigrationStarted",
			"DeviceMigrationStarted",
			"failing and rebuilding node %s devices; its volumes serve from replicas until they are drained",
			nodeUUID)
		r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "DeviceMigrationStarted",
			fmt.Sprintf("failing and rebuilding node %s devices", nodeUUID))
		return ctrl.Result{RequeueAfter: drainRequeueDevices}, nil
	}

	progress, err := fetchDrainProgress(ctx, apiClient, endpoint)
	if err != nil {
		log.Error(err, "drain: failed to read device migration progress", "node", nodeUUID)
		return ctrl.Result{RequeueAfter: drainRequeueDevices}, nil
	}

	patch := client.MergeFrom(ops.DeepCopy())
	ops.Status.DevicesMigrated = progress.Completed
	ops.Status.DevicesTotal = progress.Total
	if progress.Total > 0 {
		ops.Status.Message = fmt.Sprintf("MigratingDevices: %d of %d devices rebuilt",
			progress.Completed, progress.Total)
	} else {
		ops.Status.Message = "MigratingDevices: rebuilding this node's devices onto its peers"
	}
	_ = r.Status().Patch(ctx, ops, patch)

	// A device that cannot be rebuilt is not something to drain volumes past:
	// its data now has one replica fewer everywhere it was used, and moving
	// volumes on top of that widens the exposure instead of ending it.
	if progress.Failed > 0 {
		return r.resumeAndFail(ctx, ops, sn, apiClient, clusterUUID,
			fmt.Sprintf("device migration failed for %d device(s) on %s: %s",
				progress.Failed, nodeUUID, progress.Message))
	}

	if !progress.Done {
		return ctrl.Result{RequeueAfter: drainRequeueDevices}, nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal, "DeviceMigrationCompleted",
		"DeviceMigrationCompleted", "all %d device(s) rebuilt onto peers", progress.Completed)
	r.emitOnStorageNode(ctx, ops, corev1.EventTypeNormal, "DeviceMigrationCompleted",
		fmt.Sprintf("all %d device(s) rebuilt onto peers", progress.Completed))

	// Move the node's own status on with the phase. The control plane stamps
	// the device half itself (that is what /migrate-devices does), but the
	// volume half is driven from here, so nothing else would ever move it --
	// and a node left saying migrating_devices through the whole volume phase
	// makes `sbctl sn list` disagree with this CR about where a removal is.
	//
	// Best-effort: it is a status label, not a precondition, and failing the
	// drain because a cosmetic patch did not land would be worse than the
	// label being briefly stale. The next reconcile re-POSTs it.
	endpoint = fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/migrating-lvols",
		clusterUUID, nodeUUID)
	if _, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, nil); err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d", status)
		}
		log.Error(err, "drain: could not mark the node migrating_lvols (continuing)")
	}

	return r.advanceSubPhase(ctx, ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating)
}

