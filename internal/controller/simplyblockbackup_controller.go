package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

const (
	backupFinalizer      = "simplyblock.backup.finalizer"
	backupPollInterval   = 10 * time.Second
	backupPhaseCompleted = "completed"
	backupPhaseFailed    = "failed"
)

// SimplyBlockBackupReconciler reconciles a SimplyBlockBackup object.
type SimplyBlockBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockbackups/finalizers,verbs=update

func (r *SimplyBlockBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	backupCR := &simplyblockv1alpha1.SimplyBlockBackup{}
	if err := r.Get(ctx, req.NamespacedName, backupCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !backupCR.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(backupCR, backupFinalizer) {
			controllerutil.RemoveFinalizer(backupCR, backupFinalizer)
			if err := r.Update(ctx, backupCR); err != nil {
				log.Error(err, "Failed to remove backup finalizer")
				return ctrl.Result{RequeueAfter: backupPollInterval}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(backupCR, backupFinalizer) {
		controllerutil.AddFinalizer(backupCR, backupFinalizer)
		if err := r.Update(ctx, backupCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName)
	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing", "cluster", backupCR.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: backupPollInterval}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: backupPollInterval}, nil
	}

	poolUUID, err := utils.ResolvePoolUUID(ctx, r.Client, backupCR.Namespace, backupCR.Spec.ClusterName, backupCR.Spec.PoolName)
	if err != nil {
		log.Info("Pool UUID not ready yet, requeuing", "pool", backupCR.Spec.PoolName)
		return ctrl.Result{RequeueAfter: backupPollInterval}, nil
	}

	apiClient := webapi.NewClient()

	volume, err := fetchVolumeByName(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, backupCR.Spec.Source.VolumeRef.Name)
	if err != nil {
		log.Error(err, "Failed to resolve source volume")
		return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
			status.Phase = "error"
			status.Message = err.Error()
		}, backupPollInterval)
	}
	if volume == nil {
		msg := fmt.Sprintf("volume %q not found in pool %q", backupCR.Spec.Source.VolumeRef.Name, backupCR.Spec.PoolName)
		log.Info(msg)
		return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
			status.Phase = "pending"
			status.Message = msg
		}, backupPollInterval)
	}

	snapshotName := backupCR.Spec.Snapshot.Name
	if snapshotName == "" {
		snapshotName = generatedBackupSnapshotName(backupCR.Name, string(backupCR.UID))
	}

	if backupCR.Status.SourceVolumeID != volume.ID ||
		backupCR.Status.SourceVolumeName != volume.Name ||
		backupCR.Status.SnapshotName != snapshotName {
		if res, err := r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
			status.SourceVolumeID = volume.ID
			status.SourceVolumeName = volume.Name
			status.SnapshotName = snapshotName
		}, 0); err != nil || res.RequeueAfter != 0 {
			return res, err
		}
	}

	if backupCR.Status.SnapshotID == "" {
		snapshots, err := fetchSnapshotsForVolume(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, volume.ID)
		if err != nil {
			log.Error(err, "Failed to list snapshots")
			return ctrl.Result{RequeueAfter: backupPollInterval}, nil
		}
		if snapshot := findSnapshotByName(snapshots, snapshotName); snapshot != nil {
			return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
				status.Phase = "snapshot_ready"
				status.Message = ""
				status.SnapshotID = snapshot.ID
				status.SnapshotName = snapshot.Name
			}, 0)
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/snapshots", clusterUUID, poolUUID, volume.ID)
		body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, map[string]any{
			"name":   snapshotName,
			"backup": false,
		})
		if err != nil || statusCode >= 300 {
			log.Error(err, "Failed to create snapshot", "status", statusCode, "response", string(body))
			return ctrl.Result{RequeueAfter: backupPollInterval}, nil
		}

		return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
			status.Phase = "snapshot_pending"
			status.Message = "snapshot created, waiting for discovery"
		}, backupPollInterval)
	}

	backups, err := fetchBackups(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		log.Error(err, "Failed to list backups")
		return ctrl.Result{RequeueAfter: backupPollInterval}, nil
	}

	if backupCR.Status.BackupID == "" {
		if existing := findBackupBySnapshotID(backups, backupCR.Status.SnapshotID); existing != nil {
			return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
				applyBackupStatus(status, existing)
			}, backupRequeueForPhase(existing.Status))
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/", clusterUUID)
		resp, err := apiClient.DoDetailed(ctx, clusterSecret, http.MethodPost, endpoint, map[string]any{
			"snapshot_id": backupCR.Status.SnapshotID,
		})
		if err != nil || resp.Status >= 300 {
			if strings.Contains(strings.ToLower(string(resp.Body)), "already has a backup") {
				backups, listErr := fetchBackups(ctx, apiClient, clusterSecret, clusterUUID)
				if listErr == nil {
					if existing := findBackupBySnapshotID(backups, backupCR.Status.SnapshotID); existing != nil {
						return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
							applyBackupStatus(status, existing)
						}, backupRequeueForPhase(existing.Status))
					}
				}
			}
			log.Error(err, "Failed to create backup", "status", resp.Status, "response", string(resp.Body))
			return ctrl.Result{RequeueAfter: backupPollInterval}, nil
		}

		backupID := resp.Headers.Get("X-Backup-Id")
		return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
			status.Phase = "pending"
			status.Message = ""
			status.BackupID = backupID
		}, backupPollInterval)
	}

	var current *backupAPIResponse
	for i := range backups {
		if backups[i].ID == backupCR.Status.BackupID {
			current = &backups[i]
			break
		}
	}
	if current == nil {
		log.Info("Backup not visible in list yet", "backupID", backupCR.Status.BackupID)
		return ctrl.Result{RequeueAfter: backupPollInterval}, nil
	}

	if current.Status == backupPhaseCompleted && !backupCR.Spec.Snapshot.Retain && !backupCR.Status.SnapshotDeleted && backupCR.Status.SnapshotID != "" {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/snapshots/%s/", clusterUUID, poolUUID, backupCR.Status.SnapshotID)
		body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
		if err != nil || (statusCode != http.StatusNoContent && statusCode != http.StatusNotFound) {
			log.Error(err, "Failed to delete temporary snapshot", "status", statusCode, "response", string(body))
			return ctrl.Result{RequeueAfter: backupPollInterval}, nil
		}
		return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
			applyBackupStatus(status, current)
			status.SnapshotDeleted = true
		}, 0)
	}

	return r.patchBackupStatus(ctx, backupCR, func(status *simplyblockv1alpha1.SimplyBlockBackupStatus) {
		applyBackupStatus(status, current)
	}, backupRequeueForPhase(current.Status))
}

func (r *SimplyBlockBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockBackup{}).
		Named("simplyblockbackup").
		Complete(r)
}

func (r *SimplyBlockBackupReconciler) patchBackupStatus(
	ctx context.Context,
	backupCR *simplyblockv1alpha1.SimplyBlockBackup,
	mutate func(*simplyblockv1alpha1.SimplyBlockBackupStatus),
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	patch := client.MergeFrom(backupCR.DeepCopy())
	mutate(&backupCR.Status)
	backupCR.Status.ObservedGeneration = backupCR.Generation
	if err := r.Status().Patch(ctx, backupCR, patch); err != nil {
		return ctrl.Result{RequeueAfter: backupPollInterval}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func applyBackupStatus(status *simplyblockv1alpha1.SimplyBlockBackupStatus, current *backupAPIResponse) {
	status.Phase = current.Status
	status.Message = ""
	status.BackupID = current.ID
	s3ID := current.S3ID
	status.S3ID = &s3ID
	status.PreviousBackupID = current.PrevBackupID
	status.CreatedAt = unixTimePtr(current.CreatedAt)
	status.CompletedAt = unixTimePtr(current.CompletedAt)
}

func backupRequeueForPhase(phase string) time.Duration {
	switch phase {
	case backupPhaseCompleted, backupPhaseFailed:
		return 0
	default:
		return backupPollInterval
	}
}
