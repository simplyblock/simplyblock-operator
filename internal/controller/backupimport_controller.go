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
	"net/url"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	importReconcileRequeue = 10 * time.Second
)

const (
	eventReasonImportSourceClusterLookupError  = "ImportSourceClusterLookupError"
	eventReasonImportSourceClusterAuthError    = "ImportSourceClusterAuthError"
	eventReasonImportTargetClusterLookupError  = "ImportTargetClusterLookupError"
	eventReasonImportTargetClusterAuthError    = "ImportTargetClusterAuthError"
	eventReasonImportExportFailed              = "ImportExportFailed"
	eventReasonImportFailed                    = "ImportFailed"
	eventReasonImportStorageBackupCreateFailed = "ImportStorageBackupCreateFailed"
)

// BackupImportReconciler reconciles a BackupImport object.
type BackupImportReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	APIClient *webapi.Client
}

type importBackupsRequest struct {
	Metadata json.RawMessage `json:"metadata"`
}

type importBackupsResponse struct {
	Imported int `json:"imported"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backupimports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backupimports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backupimports/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *BackupImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	importCR := &simplyblockv1alpha1.BackupImport{}
	if err := r.Get(ctx, req.NamespacedName, importCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if importCR.Status.Phase == simplyblockv1alpha1.BackupImportPhaseDone ||
		importCR.Status.Phase == simplyblockv1alpha1.BackupImportPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Resolve source cluster credentials.
	srcClusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, importCR.Namespace, importCR.Spec.SourceClusterName)
	if err != nil {
		if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportSourceClusterLookupError,
			"Failed to resolve source cluster UUID: %v", err)
		return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
	}
	_, srcSecret, err := utils.GetClusterAuth(ctx, r.Client, importCR.Namespace, importCR.Spec.SourceClusterName)
	if err != nil {
		if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportSourceClusterAuthError,
			"Failed to get source cluster auth: %v", err)
		return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
	}

	// Resolve target cluster credentials.
	targetClusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, importCR.Namespace, importCR.Spec.TargetClusterName)
	if err != nil {
		if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportTargetClusterLookupError,
			"Failed to resolve target cluster UUID: %v", err)
		return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
	}
	_, targetSecret, err := utils.GetClusterAuth(ctx, r.Client, importCR.Namespace, importCR.Spec.TargetClusterName)
	if err != nil {
		if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportTargetClusterAuthError,
			"Failed to get target cluster auth: %v", err)
		return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
	}

	// Persist resolved UUIDs once.
	if importCR.Status.SourceClusterUUID == "" || importCR.Status.TargetClusterUUID == "" {
		if err := r.patchStatus(ctx, importCR, func(s *simplyblockv1alpha1.BackupImportStatus) {
			s.SourceClusterUUID = srcClusterUUID
			s.TargetClusterUUID = targetClusterUUID
			if s.Phase == "" {
				s.Phase = simplyblockv1alpha1.BackupImportPhasePending
			}
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	apiClient := r.apiClient()

	// Phase: Exporting — fetch backup chain from source cluster.
	if importCR.Status.ImportedBackupID == "" {
		if err := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhaseExporting,
			"Exporting backup chain from source cluster"); err != nil {
			return ctrl.Result{}, err
		}

		exportedData, err := r.exportBackup(ctx, apiClient, srcSecret, srcClusterUUID, importCR.Spec.SourceBackupID)
		if err != nil {
			if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending,
				fmt.Sprintf("Export failed: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportExportFailed,
				"Failed to export backup from source cluster: %v", err)
			return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
		}

		// Phase: Importing — register backup chain in target cluster.
		if err := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhaseImporting,
			"Importing backup chain into target cluster"); err != nil {
			return ctrl.Result{}, err
		}

		imported, err := r.importBackup(ctx, apiClient, targetSecret, targetClusterUUID, exportedData)
		if err != nil {
			if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending,
				fmt.Sprintf("Import failed: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportFailed,
				"Failed to import backup into target cluster: %v", err)
			return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
		}

		logf.FromContext(ctx).Info("Backup chain imported", "count", imported, "backupID", importCR.Spec.SourceBackupID)

		if err := r.patchStatus(ctx, importCR, func(s *simplyblockv1alpha1.BackupImportStatus) {
			s.ImportedBackupID = importCR.Spec.SourceBackupID
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Phase: create StorageBackup CR to represent the imported backup.
	if importCR.Status.StorageBackupRef == "" {
		backupCRName := fmt.Sprintf("%s-imported", importCR.Name)
		if err := r.ensureStorageBackupCR(ctx, importCR, backupCRName, srcClusterUUID); err != nil {
			if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending,
				fmt.Sprintf("Failed to create StorageBackup CR: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			r.Recorder.Eventf(importCR, corev1.EventTypeWarning, eventReasonImportStorageBackupCreateFailed,
				"Failed to create StorageBackup CR: %v", err)
			return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
		}
		if err := r.patchStatus(ctx, importCR, func(s *simplyblockv1alpha1.BackupImportStatus) {
			s.StorageBackupRef = backupCRName
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	return ctrl.Result{}, r.patchStatus(ctx, importCR, func(s *simplyblockv1alpha1.BackupImportStatus) {
		s.Phase = simplyblockv1alpha1.BackupImportPhaseDone
		s.Message = fmt.Sprintf("Backup imported; StorageBackup CR: %s", importCR.Status.StorageBackupRef)
		s.CompletedAt = &now
	})
}

func (r *BackupImportReconciler) exportBackup(
	ctx context.Context,
	apiClient *webapi.Client,
	srcSecret, srcClusterUUID, backupID string,
) (json.RawMessage, error) {
	params := url.Values{"backup_id": {backupID}}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/export?%s",
		url.PathEscape(srcClusterUUID), params.Encode())
	body, status, err := apiClient.Do(ctx, srcSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("backup %s not found on source cluster", backupID)
	}
	if status >= 300 {
		return nil, fmt.Errorf("export API failed: status=%d body=%s", status, string(body))
	}
	// Validate it's a non-empty JSON array.
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("unmarshal export response: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("backup %s has no completed backups to export", backupID)
	}
	return body, nil
}

func (r *BackupImportReconciler) importBackup(
	ctx context.Context,
	apiClient *webapi.Client,
	targetSecret, targetClusterUUID string,
	exportedData json.RawMessage,
) (int, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/import", url.PathEscape(targetClusterUUID))
	reqBody := importBackupsRequest{Metadata: exportedData}
	body, status, err := apiClient.Do(ctx, targetSecret, http.MethodPost, endpoint, reqBody)
	if err != nil {
		return 0, err
	}
	if status >= 300 {
		return 0, fmt.Errorf("import API failed: status=%d body=%s", status, string(body))
	}
	var resp importBackupsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("unmarshal import response: %w", err)
	}
	return resp.Imported, nil
}

func (r *BackupImportReconciler) ensureStorageBackupCR(
	ctx context.Context,
	importCR *simplyblockv1alpha1.BackupImport,
	name, srcClusterUUID string,
) error {
	existing := &simplyblockv1alpha1.StorageBackup{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: importCR.Namespace}, existing)
	if err == nil {
		return nil // already exists
	}
	if !kerrors.IsNotFound(err) {
		return fmt.Errorf("get StorageBackup %s: %w", name, err)
	}

	backupCR := &simplyblockv1alpha1.StorageBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: importCR.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(importCR, simplyblockv1alpha1.GroupVersion.WithKind("BackupImport")),
			},
		},
		Spec: simplyblockv1alpha1.StorageBackupSpec{
			ClusterName:       importCR.Spec.TargetClusterName,
			SourceClusterUUID: srcClusterUUID,
		},
	}
	if err := r.Create(ctx, backupCR); err != nil {
		return fmt.Errorf("create StorageBackup %s: %w", name, err)
	}

	// Patch status so BackupRestore can read BackupID and SourceClusterUUID.
	if err := r.patchStorageBackupStatus(ctx, backupCR, importCR.Spec.SourceBackupID, srcClusterUUID, importCR.Status.TargetClusterUUID); err != nil {
		return fmt.Errorf("patch StorageBackup status: %w", err)
	}
	return nil
}

func (r *BackupImportReconciler) patchStorageBackupStatus(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.StorageBackup,
	backupID, srcClusterUUID, targetClusterUUID string,
) error {
	patch := client.MergeFrom(backupCR.DeepCopy())
	backupCR.Status.Phase = simplyblockv1alpha1.BackupPhaseDone
	backupCR.Status.BackupID = backupID
	backupCR.Status.SourceClusterUUID = srcClusterUUID
	backupCR.Status.ClusterUUID = targetClusterUUID
	backupCR.Status.Message = fmt.Sprintf("Imported from cluster %s", srcClusterUUID)
	return r.Status().Patch(ctx, backupCR, patch)
}

func (r *BackupImportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.BackupImport{}).
		Named("backupimport").
		Complete(r)
}

func (r *BackupImportReconciler) apiClient() *webapi.Client {
	if r.APIClient != nil {
		return r.APIClient
	}
	return webapi.NewClient()
}

func (r *BackupImportReconciler) patchPhase(
	ctx context.Context,
	importCR *simplyblockv1alpha1.BackupImport,
	phase, message string,
) error {
	return r.patchStatus(ctx, importCR, func(s *simplyblockv1alpha1.BackupImportStatus) {
		s.Phase = phase
		s.Message = message
	})
}

func (r *BackupImportReconciler) patchStatus(
	ctx context.Context,
	importCR *simplyblockv1alpha1.BackupImport,
	mutate func(*simplyblockv1alpha1.BackupImportStatus),
) error {
	desired := importCR.Status
	mutate(&desired)
	if reflect.DeepEqual(importCR.Status, desired) {
		return nil
	}
	patch := client.MergeFrom(importCR.DeepCopy())
	importCR.Status = desired
	return r.Status().Patch(ctx, importCR, patch)
}
