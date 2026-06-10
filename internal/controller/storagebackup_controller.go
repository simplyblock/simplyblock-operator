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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	backupAPIStatusPending    = "pending"
	backupAPIStatusInProgress = "in_progress"
	backupAPIStatusCompleted  = "completed"
	backupAPIStatusFailed     = "failed"
	backupAPIStatusMerging    = "merging"
	backupAPIStatusDeleting   = "deleting"
)

const (
	backupFinalizer        = "storage.simplyblock.io/storagebackup-finalizer"
	backupPendingMessage   = "Waiting for backup metadata from the API"
	backupDeletionRequeue  = 10 * time.Second
	backupProgressRequeue  = 10 * time.Second
	backupReconcileRequeue = 10 * time.Second

	pvcLvolIDAnnotation = "simplybk/lvol-id"
)

// Event reason constants for StorageBackup reconciliation.
// These are emitted as Kubernetes Warning events and are visible
// via `kubectl describe storagebackup <name>` under the Events section.
const (
	// eventReasonBackupClusterLookupError is emitted when the controller cannot
	// resolve the cluster UUID for the target cluster name.
	eventReasonBackupClusterLookupError = "BackupClusterLookupError"

	// eventReasonBackupClusterAuthError is emitted when cluster credentials
	// cannot be retrieved, blocking any authenticated API call.
	eventReasonBackupClusterAuthError = "BackupClusterAuthError"

	// eventReasonBackupSourceResolutionError is emitted when the PVC/PV source
	// cannot be resolved (e.g. PVC not found, not bound, or missing lvol metadata).
	eventReasonBackupSourceResolutionError = "BackupSourceResolutionError"

	// eventReasonBackupPoolLookupError is emitted when the storage pool UUID
	// cannot be resolved from the pool name via the backend API.
	eventReasonBackupPoolLookupError = "BackupPoolLookupError"

	// eventReasonBackupSnapshotCreateFailed is emitted when the snapshot creation
	// API call fails. The event message includes the HTTP status and response body.
	eventReasonBackupSnapshotCreateFailed = "BackupSnapshotCreateFailed"

	// eventReasonBackupCreateFailed is emitted when the backup creation API call
	// fails. The event message includes the HTTP status and response body.
	eventReasonBackupCreateFailed = "BackupCreateFailed"

	// eventReasonBackupListFailed is emitted when the controller cannot list
	// backups from the backend API to track progress.
	eventReasonBackupListFailed = "BackupListFailed"

	// eventReasonBackupDeleteFailed is emitted when the backup deletion API call
	// fails during finalizer processing.
	eventReasonBackupDeleteFailed = "BackupDeleteFailed"

	// eventReasonBackupSnapshotDeleteFailed is emitted when the internal snapshot
	// deletion API call fails during finalizer processing.
	eventReasonBackupSnapshotDeleteFailed = "BackupSnapshotDeleteFailed"
)

// StorageBackupReconciler reconciles a StorageBackup object.
type StorageBackupReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	APIClient *webapi.Client
}

type backupSource struct {
	PVCNamespace string
	PVName       string
	PoolName     string
	LvolID       string
}

type storagePoolAPIResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type snapshotCreateRequest struct {
	Name   string `json:"name"`
	Backup bool   `json:"backup"`
}

type backupCreateRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

type backupAPIResponse struct {
	ID           string              `json:"id"`
	S3ID         int64               `json:"s3_id"`
	LvolID       string              `json:"lvol_id"`
	LvolName     string              `json:"lvol_name"`
	SnapshotID   string              `json:"snapshot_id"`
	SnapshotName string              `json:"snapshot_name"`
	NodeID       string              `json:"node_id"`
	Status       string              `json:"status"`
	PrevBackupID string              `json:"prev_backup_id"`
	Size         int64               `json:"size"`
	AllowedHosts []map[string]string `json:"allowed_hosts"`
	CreatedAt    int64               `json:"created_at"`
	CompletedAt  int64               `json:"completed_at"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// backupContext holds the resolved prerequisites needed across reconciliation phases.
type backupContext struct {
	clusterUUID   string
	clusterSecret string
	apiClient     *webapi.Client
	source        *backupSource
	poolUUID      string
}

func (r *StorageBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	backupCR := &simplyblockv1alpha1.StorageBackup{}
	if err := r.Get(ctx, req.NamespacedName, backupCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !backupCR.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, backupCR)
	}

	// Imported backups (created by BackupImport controller) have their status managed
	// externally. Skip snapshot/backup creation and treat as terminal once Done.
	if backupCR.Spec.SourceClusterUUID != "" {
		if backupCR.Status.Phase == simplyblockv1alpha1.BackupPhaseDone ||
			backupCR.Status.Phase == simplyblockv1alpha1.BackupPhaseFailed {
			return ctrl.Result{}, nil
		}
		// Still pending status patch from BackupImport controller; requeue briefly.
		return ctrl.Result{RequeueAfter: backupReconcileRequeue}, nil
	}

	if !controllerutil.ContainsFinalizer(backupCR, backupFinalizer) {
		controllerutil.AddFinalizer(backupCR, backupFinalizer)
		if err := r.Update(ctx, backupCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Imported CRs (created by StorageBackupSyncReconciler) carry the imported
	// label at creation time; the status patch that populates BackupID and
	// SnapshotID is a separate API call that races with this reconciler. If
	// BackupID is still empty the patch hasn't landed yet — requeue and wait
	// rather than creating a duplicate snapshot/backup in the backend.
	if backupCR.Labels[backupSyncImportedLabel] == "true" && backupCR.Status.BackupID == "" {
		return ctrl.Result{RequeueAfter: backupReconcileRequeue}, nil
	}

	bctx, result, done, err := r.prepareBackupContext(ctx, backupCR)
	if done {
		return result, err
	}

	result, done, err = r.ensureSnapshotAndBackup(ctx, backupCR, bctx)
	if done {
		return result, err
	}

	return r.syncBackupProgress(ctx, backupCR, bctx)
}

// prepareBackupContext resolves all prerequisites (cluster UUID, auth, source, pool UUID)
// and patches their resolved values into status. Returns done=true when the caller should
// return result immediately (either an error or a requeue).
func (r *StorageBackupReconciler) prepareBackupContext(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
) (*backupContext, ctrl.Result, bool, error) {
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName)
	if err != nil {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.Phase = simplyblockv1alpha1.BackupPhasePending
			status.Message = err.Error()
		}); patchErr != nil {
			return nil, ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupClusterLookupError, "Failed to resolve cluster UUID for %s: %v", backupCR.Spec.ClusterName, err)
		return nil, ctrl.Result{RequeueAfter: backupReconcileRequeue}, true, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName)
	if err != nil {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.Phase = simplyblockv1alpha1.BackupPhasePending
			status.ClusterUUID = clusterUUID
			status.Message = err.Error()
		}); patchErr != nil {
			return nil, ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupClusterAuthError, "Failed to get cluster auth for %s: %v", backupCR.Spec.ClusterName, err)
		return nil, ctrl.Result{RequeueAfter: backupReconcileRequeue}, true, nil
	}

	apiClient := r.apiClient()

	source, err := r.resolveBackupSource(ctx, backupCR, clusterUUID)
	if err != nil {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.Phase = simplyblockv1alpha1.BackupPhasePending
			status.ClusterUUID = clusterUUID
			status.Message = err.Error()
		}); patchErr != nil {
			return nil, ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupSourceResolutionError, "Failed to resolve backup source: %v", err)
		return nil, ctrl.Result{RequeueAfter: backupReconcileRequeue}, true, nil
	}

	poolUUID, err := r.lookupPoolUUID(ctx, apiClient, clusterSecret, clusterUUID, source.PoolName)
	if err != nil {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.Phase = simplyblockv1alpha1.BackupPhasePending
			status.ClusterUUID = clusterUUID
			status.PVCNamespace = source.PVCNamespace
			status.PVName = source.PVName
			status.PoolName = source.PoolName
			status.LvolID = source.LvolID
			status.Message = err.Error()
		}); patchErr != nil {
			return nil, ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupPoolLookupError, "Failed to look up pool UUID for %s: %v", source.PoolName, err)
		return nil, ctrl.Result{RequeueAfter: backupReconcileRequeue}, true, nil
	}

	if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
		status.ClusterUUID = clusterUUID
		status.PVCNamespace = source.PVCNamespace
		status.PVName = source.PVName
		status.PoolName = source.PoolName
		status.PoolUUID = poolUUID
		status.LvolID = source.LvolID
		if status.Phase == "" {
			status.Phase = simplyblockv1alpha1.BackupPhasePending
		}
	}); patchErr != nil {
		return nil, ctrl.Result{}, true, patchErr
	}

	return &backupContext{
		clusterUUID:   clusterUUID,
		clusterSecret: clusterSecret,
		apiClient:     apiClient,
		source:        source,
		poolUUID:      poolUUID,
	}, ctrl.Result{}, false, nil
}

// ensureSnapshotAndBackup idempotently creates the internal snapshot and backup
// objects. Returns done=true when the caller should return result immediately.
func (r *StorageBackupReconciler) ensureSnapshotAndBackup(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
	bctx *backupContext,
) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)

	if backupCR.Status.SnapshotName == "" {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.SnapshotName = r.snapshotNameFor(backupCR)
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
	}

	if backupCR.Status.SnapshotID == "" {
		snapshotID, createErr := r.createSnapshot(ctx, bctx.apiClient, bctx.clusterSecret, bctx.clusterUUID, bctx.poolUUID, bctx.source.LvolID, backupCR.Status.SnapshotName)
		if createErr != nil {
			log.Error(createErr, "Failed to create snapshot", "backup", backupCR.Name)
			r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupSnapshotCreateFailed, "Failed to create snapshot: %v", createErr)
			result, err := r.handleAPIError(ctx, backupCR, bctx.clusterUUID, createErr)
			return result, true, err
		}
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.SnapshotID = snapshotID
			status.Message = "Snapshot created; submitting backup request"
			status.Phase = simplyblockv1alpha1.BackupPhasePending
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
	}

	if backupCR.Status.BackupID == "" {
		backupID, createErr := r.createBackup(ctx, bctx.apiClient, bctx.clusterSecret, bctx.clusterUUID, backupCR.Status.SnapshotID)
		if createErr != nil {
			log.Error(createErr, "Failed to create backup", "backup", backupCR.Name, "snapshotID", backupCR.Status.SnapshotID)
			r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupCreateFailed, "Failed to create backup: %v", createErr)
			result, err := r.handleAPIError(ctx, backupCR, bctx.clusterUUID, createErr)
			return result, true, err
		}
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.BackupID = backupID
			status.Message = backupPendingMessage
			status.Phase = simplyblockv1alpha1.BackupPhasePending
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
	}

	return ctrl.Result{}, false, nil
}

// syncBackupProgress polls the API for the current backup state and updates status.
func (r *StorageBackupReconciler) syncBackupProgress(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
	bctx *backupContext,
) (ctrl.Result, error) {
	backups, err := r.listBackups(ctx, bctx.apiClient, bctx.clusterSecret, bctx.clusterUUID)
	if err != nil {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.Message = err.Error()
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupListFailed, "Failed to list backups from API: %v", err)
		return ctrl.Result{RequeueAfter: backupReconcileRequeue}, nil
	}

	backup := findBackupByID(backups, backupCR.Status.BackupID)
	if backup == nil {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.Message = backupPendingMessage
			if status.Phase == "" {
				status.Phase = simplyblockv1alpha1.BackupPhasePending
			}
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: backupProgressRequeue}, nil
	}

	if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
		status.APIStatus = backup.Status
		status.Phase = backupPhaseFromAPIStatus(backup.Status)
		status.Message = fmt.Sprintf("Backup status: %s", backup.Status)
		status.BackupID = backup.ID
		status.S3ID = backup.S3ID
		status.LvolID = backup.LvolID
		status.LvolName = backup.LvolName
		status.SnapshotID = backup.SnapshotID
		status.SnapshotName = backup.SnapshotName
		status.NodeID = backup.NodeID
		status.PrevBackupID = backup.PrevBackupID
		status.Size = backup.Size
		status.AllowedHosts = backup.AllowedHosts
		status.CreatedAt = unixToTimePtr(backup.CreatedAt)
		status.CompletedAt = unixToTimePtr(backup.CompletedAt)
	}); patchErr != nil {
		return ctrl.Result{}, patchErr
	}

	if backupTerminal(backup.Status) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: backupProgressRequeue}, nil
}

func (r *StorageBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageBackup{}).
		Named("storagebackup").
		Complete(r)
}

func (r *StorageBackupReconciler) handleDeletion(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(backupCR, backupFinalizer) {
		return ctrl.Result{}, nil
	}

	// Imported backups live on the source cluster's S3; do not delete them from the backend.
	if backupCR.Spec.SourceClusterUUID != "" {
		controllerutil.RemoveFinalizer(backupCR, backupFinalizer)
		return ctrl.Result{}, r.Update(ctx, backupCR)
	}

	clusterUUID := backupCR.Status.ClusterUUID
	clusterSecret := ""

	if clusterUUID == "" {
		resolvedClusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName)
		if err == nil {
			clusterUUID = resolvedClusterUUID
		}
	}
	if clusterUUID != "" {
		_, secret, err := utils.GetClusterAuth(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName)
		if err == nil {
			clusterSecret = secret
		}
	}

	apiClient := r.apiClient()
	if clusterUUID != "" && clusterSecret != "" && backupCR.Status.LvolID != "" {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/%s", clusterUUID, backupCR.Status.LvolID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
		if err != nil {
			log.Error(err, "Failed to delete backup chain", "backup", backupCR.Name)
			r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupDeleteFailed, "Failed to delete backup chain: %v", err)
			return ctrl.Result{RequeueAfter: backupDeletionRequeue}, nil
		}
		if status >= 300 && !strings.Contains(strings.ToLower(string(body)), "no backups found") {
			log.Info("Backup delete returned non-success status", "status", status, "body", string(body))
			r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupDeleteFailed, "Backup delete returned non-success status %d: %s", status, string(body))
			return ctrl.Result{RequeueAfter: backupDeletionRequeue}, nil
		}
	}

	if clusterUUID != "" && clusterSecret != "" && backupCR.Status.PoolUUID != "" && backupCR.Status.SnapshotID != "" {
		endpoint := fmt.Sprintf(
			"/api/v2/clusters/%s/storage-pools/%s/snapshots/%s/",
			clusterUUID,
			backupCR.Status.PoolUUID,
			backupCR.Status.SnapshotID,
		)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
		if err != nil {
			log.Error(err, "Failed to delete internal snapshot", "backup", backupCR.Name, "snapshotID", backupCR.Status.SnapshotID)
			r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupSnapshotDeleteFailed, "Failed to delete internal snapshot %s: %v", backupCR.Status.SnapshotID, err)
			return ctrl.Result{RequeueAfter: backupDeletionRequeue}, nil
		}
		if status >= 300 && status != http.StatusNotFound {
			log.Info("Snapshot delete returned non-success status", "status", status, "body", string(body))
			r.Recorder.Eventf(backupCR, corev1.EventTypeWarning, eventReasonBackupSnapshotDeleteFailed, "Snapshot delete returned non-success status %d: %s", status, string(body))
			return ctrl.Result{RequeueAfter: backupDeletionRequeue}, nil
		}
	}

	controllerutil.RemoveFinalizer(backupCR, backupFinalizer)
	if err := r.Update(ctx, backupCR); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *StorageBackupReconciler) apiClient() *webapi.Client {
	if r.APIClient != nil {
		return r.APIClient
	}
	return webapi.NewClient()
}

func (r *StorageBackupReconciler) resolveBackupSource(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
	clusterUUID string,
) (*backupSource, error) {
	if backupCR.Spec.PVCRef == nil {
		return nil, fmt.Errorf("spec.pvcRef is required for non-imported StorageBackup resources")
	}

	pvcNamespace := backupCR.Spec.PVCRef.Namespace
	if pvcNamespace == "" {
		pvcNamespace = backupCR.Namespace
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Name: backupCR.Spec.PVCRef.Name, Namespace: pvcNamespace}, pvc); err != nil {
		return nil, fmt.Errorf("get PVC %s/%s: %w", pvcNamespace, backupCR.Spec.PVCRef.Name, err)
	}

	if pvc.Spec.VolumeName == "" {
		return nil, fmt.Errorf("PVC %s/%s is not bound yet", pvcNamespace, pvc.Name)
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return nil, fmt.Errorf("get PV %s: %w", pvc.Spec.VolumeName, err)
	}

	if pv.Spec.CSI == nil {
		return nil, fmt.Errorf("PV %s is not a CSI volume", pv.Name)
	}

	handleClusterUUID, poolNameOrID, handleLvolID, err := parseSimplyblockVolumeHandle(pv.Spec.CSI.VolumeHandle)
	if err != nil {
		return nil, err
	}
	if handleClusterUUID != "" && clusterUUID != "" && handleClusterUUID != clusterUUID {
		return nil, fmt.Errorf(
			"PVC %s/%s belongs to cluster UUID %s but backup targets %s",
			pvcNamespace,
			pvc.Name,
			handleClusterUUID,
			clusterUUID,
		)
	}

	lvolID := pvc.Annotations[pvcLvolIDAnnotation]
	if lvolID == "" {
		lvolID = handleLvolID
	}
	if lvolID == "" {
		return nil, fmt.Errorf("PVC %s/%s does not contain Simplyblock lvol metadata", pvcNamespace, pvc.Name)
	}
	if handleLvolID != "" && handleLvolID != lvolID {
		return nil, fmt.Errorf(
			"PVC %s/%s lvol annotation %s does not match PV volume handle %s",
			pvcNamespace,
			pvc.Name,
			lvolID,
			handleLvolID,
		)
	}

	return &backupSource{
		PVCNamespace: pvcNamespace,
		PVName:       pv.Name,
		PoolName:     poolNameOrID,
		LvolID:       lvolID,
	}, nil
}

func (r *StorageBackupReconciler) lookupPoolUUID(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolNameOrID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("list storage pools failed: status=%d body=%s", status, string(body))
	}

	var pools []storagePoolAPIResponse
	if err := json.Unmarshal(body, &pools); err != nil {
		return "", fmt.Errorf("unmarshal storage pools: %w", err)
	}

	for _, pool := range pools {
		if pool.Name == poolNameOrID || pool.ID == poolNameOrID {
			return pool.ID, nil
		}
	}

	return "", fmt.Errorf("storage pool %q not found in cluster %s", poolNameOrID, clusterUUID)
}

func (r *StorageBackupReconciler) createSnapshot(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	poolUUID string,
	lvolID string,
	snapshotName string,
) (string, error) {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/snapshots",
		clusterUUID,
		poolUUID,
		lvolID,
	)
	body, headers, status, err := apiClient.DoWithHeaders(ctx, clusterSecret, http.MethodPost, endpoint, snapshotCreateRequest{
		Name:   snapshotName,
		Backup: false,
	})
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", apiError{StatusCode: status, Message: fmt.Sprintf("create snapshot failed: body=%s", string(body))}
	}

	snapshotID, err := extractIDFromLocation(headers.Get("Location"))
	if err != nil {
		return "", fmt.Errorf("extract snapshot ID: %w", err)
	}
	return snapshotID, nil
}

func (r *StorageBackupReconciler) createBackup(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	snapshotID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/", clusterUUID)
	body, headers, status, err := apiClient.DoWithHeaders(ctx, clusterSecret, http.MethodPost, endpoint, backupCreateRequest{
		SnapshotID: snapshotID,
	})
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", apiError{StatusCode: status, Message: fmt.Sprintf("create backup failed: body=%s", string(body))}
	}

	backupID := headers.Get("X-Backup-Id")
	if backupID == "" {
		return "", fmt.Errorf("backup API response missing X-Backup-Id header")
	}
	return backupID, nil
}

func (r *StorageBackupReconciler) listBackups(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
) ([]backupAPIResponse, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("list backups failed: status=%d body=%s", status, string(body))
	}

	var backups []backupAPIResponse
	if err := json.Unmarshal(body, &backups); err != nil {
		return nil, fmt.Errorf("unmarshal backups: %w", err)
	}

	return backups, nil
}

func (r *StorageBackupReconciler) handleAPIError(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
	clusterUUID string,
	err error,
) (ctrl.Result, error) {
	var apiErr apiError
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
		if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
			status.ClusterUUID = clusterUUID
			status.Phase = simplyblockv1alpha1.BackupPhaseFailed
			status.Message = apiErr.Message
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}

	if patchErr := r.patchStatus(ctx, backupCR, func(status *simplyblockv1alpha1.StorageBackupStatus) {
		status.ClusterUUID = clusterUUID
		status.Phase = simplyblockv1alpha1.BackupPhasePending
		status.Message = err.Error()
	}); patchErr != nil {
		return ctrl.Result{}, patchErr
	}

	return ctrl.Result{RequeueAfter: backupReconcileRequeue}, nil
}

func (r *StorageBackupReconciler) patchStatus(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
	mutate func(status *simplyblockv1alpha1.StorageBackupStatus),
) error {
	desired := backupCR.Status
	mutate(&desired)
	if reflect.DeepEqual(backupCR.Status, desired) {
		return nil
	}

	patch := client.MergeFrom(backupCR.DeepCopy())
	backupCR.Status = desired
	return r.Status().Patch(ctx, backupCR, patch)
}

func (r *StorageBackupReconciler) snapshotNameFor(backupCR *simplyblockv1alpha1.StorageBackup) string {
	if backupCR.Spec.SnapshotName != "" {
		return backupCR.Spec.SnapshotName
	}
	return fmt.Sprintf("backup-%s", backupCR.Name)
}

func parseSimplyblockVolumeHandle(volumeHandle string) (clusterUUID, poolNameOrID, lvolID string, err error) {
	parts := strings.Split(volumeHandle, ":")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("unexpected Simplyblock CSI volume handle %q", volumeHandle)
	}
	return parts[0], parts[1], parts[2], nil
}

func extractIDFromLocation(location string) (string, error) {
	if location == "" {
		return "", fmt.Errorf("missing Location header")
	}

	parsed, err := url.Parse(location)
	if err != nil {
		return "", err
	}

	id := path.Base(strings.TrimRight(parsed.Path, "/"))
	if id == "." || id == "/" || id == "" {
		return "", fmt.Errorf("unable to derive ID from location %q", location)
	}

	return id, nil
}

func findBackupByID(backups []backupAPIResponse, backupID string) *backupAPIResponse {
	for i := range backups {
		if backups[i].ID == backupID {
			return &backups[i]
		}
	}
	return nil
}

func backupPhaseFromAPIStatus(status string) string {
	switch status {
	case backupAPIStatusPending:
		return simplyblockv1alpha1.BackupPhasePending
	case backupAPIStatusInProgress:
		return simplyblockv1alpha1.BackupPhaseInProgress
	case backupAPIStatusCompleted:
		return simplyblockv1alpha1.BackupPhaseDone
	case backupAPIStatusFailed:
		return simplyblockv1alpha1.BackupPhaseFailed
	case backupAPIStatusMerging:
		return simplyblockv1alpha1.BackupPhaseMerging
	case backupAPIStatusDeleting:
		return simplyblockv1alpha1.BackupPhaseDeleting
	default:
		return simplyblockv1alpha1.BackupPhasePending
	}
}

func backupTerminal(status string) bool {
	return status == backupAPIStatusCompleted || status == backupAPIStatusFailed
}

func unixToTimePtr(ts int64) *metav1.Time {
	if ts <= 0 {
		return nil
	}
	t := metav1.NewTime(time.Unix(ts, 0).UTC())
	return &t
}

type apiError struct {
	StatusCode int
	Message    string
}

func (e apiError) Error() string {
	return e.Message
}
