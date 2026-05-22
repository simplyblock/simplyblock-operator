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
	"time"

	corev1 "k8s.io/api/core/v1"
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
	backupSyncImportedLabel = "storage.simplyblock.io/imported"
	backupSyncRequeue       = 60 * time.Second
)

// StorageBackupSyncReconciler watches StorageCluster objects and creates
// StorageBackup CRs for any backups that exist in the backend but have no
// matching CR in Kubernetes.
//
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type StorageBackupSyncReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	APIClient *webapi.Client
}

func (r *StorageBackupSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	clusterCR := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, clusterCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if clusterCR.Status.UUID == "" {
		return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
	}

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Info("Skipping backup sync — cannot get cluster auth",
			"cluster", clusterCR.Name, "reason", err.Error())
		return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
	}

	apiClient := r.apiClient()

	// Use StorageBackupReconciler's listBackups so the HTTP logic stays in one place.
	delegate := &StorageBackupReconciler{Client: r.Client, Scheme: r.Scheme, APIClient: apiClient}
	backendBackups, err := delegate.listBackups(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		log.Error(err, "Failed to list backend backups", "cluster", clusterCR.Name)
		return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
	}
	if len(backendBackups) == 0 {
		return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
	}

	// Build a set of backend backup IDs that are already tracked by a CR.
	var existingCRs simplyblockv1alpha1.StorageBackupList
	if err := r.List(ctx, &existingCRs, client.InNamespace(clusterCR.Namespace)); err != nil {
		log.Error(err, "Failed to list existing StorageBackup CRs")
		return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
	}
	trackedIDs := make(map[string]struct{}, len(existingCRs.Items))
	for _, cr := range existingCRs.Items {
		if cr.Status.BackupID != "" {
			trackedIDs[cr.Status.BackupID] = struct{}{}
		}
	}

	// Build a reverse map: lvolID → PVC (name, namespace) from bound PVCs in
	// the cluster's namespace.
	lvolToPVC, err := r.buildLvolToPVCMap(ctx, clusterCR.Namespace, clusterUUID)
	if err != nil {
		log.Error(err, "Failed to build lvol→PVC map")
		return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
	}

	for i := range backendBackups {
		bp := &backendBackups[i]

		if _, tracked := trackedIDs[bp.ID]; tracked {
			continue
		}

		pvcEntry, found := lvolToPVC[bp.LvolID]
		pvcName, pvcNamespace := pvcEntry[0], pvcEntry[1]
		if !found {
			log.Info("Skipping backend backup — no matching PVC found for lvol",
				"backupID", bp.ID, "lvolID", bp.LvolID)
			continue
		}

		imported := &simplyblockv1alpha1.StorageBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bp.ID,
				Namespace: clusterCR.Namespace,
				Labels: map[string]string{
					backupSyncImportedLabel: "true",
				},
			},
			Spec: simplyblockv1alpha1.StorageBackupSpec{
				ClusterName: clusterCR.Name,
				PVCRef: &simplyblockv1alpha1.PersistentVolumeClaimRef{
					Name:      pvcName,
					Namespace: pvcNamespace,
				},
				SnapshotName: bp.SnapshotName,
			},
		}

		if err := r.Create(ctx, imported); err != nil {
			log.Error(err, "Failed to create StorageBackup CR for imported backup",
				"backupID", bp.ID, "cluster", clusterCR.Name)
			r.Recorder.Eventf(clusterCR, "Warning", "StorageBackupImportFailed",
				"Failed to import backend backup %q: %v", bp.ID, err)
			continue
		}

		// Pre-populate status so the StorageBackupReconciler knows this backup
		// already exists in the backend and skips creating a new snapshot/backup.
		patch := client.MergeFrom(imported.DeepCopy())
		imported.Status = simplyblockv1alpha1.StorageBackupStatus{
			Phase:        backupPhaseFromAPIStatus(bp.Status),
			APIStatus:    bp.Status,
			Message:      "Imported from storage cluster",
			ClusterUUID:  clusterUUID,
			PVCNamespace: pvcNamespace,
			LvolID:       bp.LvolID,
			LvolName:     bp.LvolName,
			SnapshotID:   bp.SnapshotID,
			SnapshotName: bp.SnapshotName,
			BackupID:     bp.ID,
			S3ID:         bp.S3ID,
			NodeID:       bp.NodeID,
			PrevBackupID: bp.PrevBackupID,
			Size:         bp.Size,
			AllowedHosts: bp.AllowedHosts,
			CreatedAt:    unixToTimePtr(bp.CreatedAt),
			CompletedAt:  unixToTimePtr(bp.CompletedAt),
		}
		if err := r.Status().Patch(ctx, imported, patch); err != nil {
			log.Error(err, "Failed to patch status for imported StorageBackup CR",
				"backupID", bp.ID)
		}

		log.Info("Imported backend backup as StorageBackup CR",
			"backupID", bp.ID, "pvc", pvcName, "cluster", clusterCR.Name)
		r.Recorder.Eventf(clusterCR, "Normal", "StorageBackupImported",
			"Imported backend backup %q (PVC %s) as StorageBackup CR", bp.ID, pvcName)
	}

	return ctrl.Result{RequeueAfter: backupSyncRequeue}, nil
}

func (r *StorageBackupSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageCluster{}).
		Named("storagebackupsync").
		Complete(r)
}

// buildLvolToPVCMap scans all bound PVCs in the given namespace and returns a
// map from Simplyblock lvol UUID to (pvcName, pvcNamespace).
// Only PVCs whose CSI volume handle belongs to the expected cluster are included.
func (r *StorageBackupSyncReconciler) buildLvolToPVCMap(
	ctx context.Context,
	namespace string,
	clusterUUID string,
) (map[string][2]string, error) {
	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	result := make(map[string][2]string)

	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if pvc.Spec.VolumeName == "" || pvc.DeletionTimestamp != nil {
			continue
		}

		pv := &corev1.PersistentVolume{}
		if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			continue
		}
		if pv.Spec.CSI == nil {
			continue
		}

		handleCluster, _, lvolID, err := parseSimplyblockVolumeHandle(pv.Spec.CSI.VolumeHandle)
		if err != nil || lvolID == "" {
			continue
		}
		if handleCluster != "" && clusterUUID != "" && handleCluster != clusterUUID {
			continue
		}

		// Prefer the annotation when present, but reject a mismatch — a stale or
		// mis-set annotation would associate this backup with the wrong PVC.
		// This mirrors the validation in StorageBackupReconciler.resolveBackupSource.
		if ann := pvc.Annotations[pvcLvolIDAnnotation]; ann != "" {
			if ann != lvolID {
				logf.FromContext(ctx).Info("Skipping PVC — lvol annotation does not match PV volume handle",
					"pvc", pvc.Name, "annotation", ann, "handle", lvolID)
				continue
			}
		}

		result[lvolID] = [2]string{pvc.Name, pvc.Namespace}
	}

	return result, nil
}

func (r *StorageBackupSyncReconciler) apiClient() *webapi.Client {
	if r.APIClient != nil {
		return r.APIClient
	}
	return webapi.NewClient()
}
