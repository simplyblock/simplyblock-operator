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
	"k8s.io/client-go/tools/events"
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
	eventReasonImportTargetClusterLookupError  = "ImportTargetClusterLookupError"
	eventReasonImportExportFailed              = "ImportExportFailed"
	eventReasonImportLocationLookupFailed      = "ImportLocationLookupFailed"
	eventReasonImportFailed                    = "ImportFailed"
	eventReasonImportStorageBackupCreateFailed = "ImportStorageBackupCreateFailed"
)

// BackupImportReconciler reconciles a BackupImport object.
type BackupImportReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	APIClient *webapi.Client
}

// backupLocation is where a set of backups lives, in the control plane's own
// wire vocabulary. Exactly the fields of its BackupLocation and no others: the
// import endpoint forbids extras, and the backup-config response this is decoded
// from carries credentials the import must not echo back.
//
// Pointers throughout so an absent field stays absent rather than being sent as
// a zero the control plane would read as a deliberate choice — an unset region
// means "let the SDK resolve it", which is not the same as "".
type backupLocation struct {
	BucketName      string  `json:"bucket_name"`
	Region          *string `json:"region,omitempty"`
	Endpoint        *string `json:"endpoint,omitempty"`
	SecondaryTarget *int    `json:"secondary_target,omitempty"`
	WithCompression *bool   `json:"with_compression,omitempty"`
	SnapshotBackups *bool   `json:"snapshot_backups,omitempty"`
	VerifyTLS       *bool   `json:"verify_tls,omitempty"`
	UsePathStyle    *bool   `json:"use_path_style,omitempty"`
}

// importBackupsRequest carries the manifests and, separately, the bucket they
// describe. A manifest states what its objects are but not how to reach them —
// deliberately, so a replicated bucket imports as itself — so the reader has to
// name the bucket, and here that is the source cluster's own.
type importBackupsRequest struct {
	Metadata json.RawMessage `json:"metadata"`
	Location backupLocation  `json:"location"`
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
		r.Recorder.Eventf(importCR, nil, corev1.EventTypeWarning, eventReasonImportSourceClusterLookupError, eventReasonImportSourceClusterLookupError,
			"Failed to resolve source cluster UUID: %v", err)
		return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
	}
	// Resolve target cluster credentials.
	targetClusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, importCR.Namespace, importCR.Spec.TargetClusterName)
	if err != nil {
		if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending, err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		r.Recorder.Eventf(importCR, nil, corev1.EventTypeWarning, eventReasonImportTargetClusterLookupError, eventReasonImportTargetClusterLookupError,
			"Failed to resolve target cluster UUID: %v", err)
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

		exportedData, err := r.exportBackup(ctx, apiClient, srcClusterUUID, importCR.Spec.SourceBackupID)
		if err != nil {
			if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending,
				fmt.Sprintf("Export failed: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			r.Recorder.Eventf(importCR, nil, corev1.EventTypeWarning, eventReasonImportExportFailed, eventReasonImportExportFailed,
				"Failed to export backup from source cluster: %v", err)
			return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
		}

		// Where those manifests' objects actually are. Read from the source
		// cluster, which is the only party that knows: the manifests describe
		// their objects without saying which bucket holds them.
		location, err := r.fetchBackupLocation(ctx, apiClient, srcClusterUUID)
		if err != nil {
			if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending,
				fmt.Sprintf("Backup location lookup failed: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			r.Recorder.Eventf(importCR, nil, corev1.EventTypeWarning, eventReasonImportLocationLookupFailed, eventReasonImportLocationLookupFailed,
				"Failed to resolve the source cluster's backup location: %v", err)
			return ctrl.Result{RequeueAfter: importReconcileRequeue}, nil
		}

		// Phase: Importing — register backup chain in target cluster.
		if err := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhaseImporting,
			"Importing backup chain into target cluster"); err != nil {
			return ctrl.Result{}, err
		}

		imported, err := r.importBackup(ctx, apiClient, targetClusterUUID, exportedData, location)
		if err != nil {
			if patchErr := r.patchPhase(ctx, importCR, simplyblockv1alpha1.BackupImportPhasePending,
				fmt.Sprintf("Import failed: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			r.Recorder.Eventf(importCR, nil, corev1.EventTypeWarning, eventReasonImportFailed, eventReasonImportFailed,
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
			r.Recorder.Eventf(importCR, nil, corev1.EventTypeWarning, eventReasonImportStorageBackupCreateFailed, eventReasonImportStorageBackupCreateFailed,
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
	srcClusterUUID, backupID string,
) (json.RawMessage, error) {
	params := url.Values{"backup_id": {backupID}}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/export?%s",
		url.PathEscape(srcClusterUUID), params.Encode())
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
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

// fetchBackupLocation reads where a cluster's backups live from its own backup
// configuration.
//
// The source cluster is asked rather than the BackupImport CR because nothing in
// the CR says: StorageCluster's spec.backup has no bucket field, so the bucket is
// derived control-plane side and this endpoint is the only place it is stated.
// The response also carries credentials; they are masked on the wire and dropped
// here regardless, since backupLocation cannot hold them.
func (r *BackupImportReconciler) fetchBackupLocation(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
) (backupLocation, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backup-config", url.PathEscape(clusterUUID))
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return backupLocation{}, err
	}
	if status == http.StatusNotFound {
		return backupLocation{}, fmt.Errorf("source cluster %s has no backup configuration", clusterUUID)
	}
	if status >= 300 {
		return backupLocation{}, fmt.Errorf("get backup config failed: status=%d body=%s", status, string(body))
	}

	var location backupLocation
	if err := json.Unmarshal(body, &location); err != nil {
		return backupLocation{}, fmt.Errorf("unmarshal backup config: %w", err)
	}
	if location.BucketName == "" {
		return backupLocation{}, fmt.Errorf("source cluster %s reports no backup bucket", clusterUUID)
	}
	return location, nil
}

func (r *BackupImportReconciler) importBackup(
	ctx context.Context,
	apiClient *webapi.Client,
	targetClusterUUID string,
	exportedData json.RawMessage,
	location backupLocation,
) (int, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/import", url.PathEscape(targetClusterUUID))
	reqBody := importBackupsRequest{Metadata: exportedData, Location: location}
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, reqBody)
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
