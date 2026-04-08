package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	restoreFinalizer      = "simplyblock.restore.finalizer"
	restorePollInterval   = 10 * time.Second
	restorePhaseCompleted = "completed"
	restorePhaseFailed    = "failed"
)

// SimplyBlockRestoreReconciler reconciles a SimplyBlockRestore object.
type SimplyBlockRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockrestores/finalizers,verbs=update

func (r *SimplyBlockRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	restoreCR := &simplyblockv1alpha1.SimplyBlockRestore{}
	if err := r.Get(ctx, req.NamespacedName, restoreCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !restoreCR.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(restoreCR, restoreFinalizer) {
			controllerutil.RemoveFinalizer(restoreCR, restoreFinalizer)
			if err := r.Update(ctx, restoreCR); err != nil {
				log.Error(err, "Failed to remove restore finalizer")
				return ctrl.Result{RequeueAfter: restorePollInterval}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(restoreCR, restoreFinalizer) {
		controllerutil.AddFinalizer(restoreCR, restoreFinalizer)
		if err := r.Update(ctx, restoreCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, restoreCR.Namespace, restoreCR.Spec.ClusterName)
	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing", "cluster", restoreCR.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, restoreCR.Namespace, restoreCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	poolUUID, err := utils.ResolvePoolUUID(ctx, r.Client, restoreCR.Namespace, restoreCR.Spec.ClusterName, restoreCR.Spec.PoolName)
	if err != nil {
		log.Info("Pool UUID not ready yet, requeuing", "pool", restoreCR.Spec.PoolName)
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	backupCR := &simplyblockv1alpha1.SimplyBlockBackup{}
	if err := r.Get(ctx, client.ObjectKey{Name: restoreCR.Spec.Source.BackupRef.Name, Namespace: restoreCR.Namespace}, backupCR); err != nil {
		log.Error(err, "Failed to fetch referenced backup")
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	if backupCR.Status.BackupID == "" {
		return r.patchRestoreStatus(ctx, restoreCR, func(status *simplyblockv1alpha1.SimplyBlockRestoreStatus) {
			status.Phase = "pending"
			status.Message = fmt.Sprintf("waiting for backup %q to produce backupID", backupCR.Name)
		}, restorePollInterval)
	}
	if backupCR.Status.Phase != backupPhaseCompleted {
		return r.patchRestoreStatus(ctx, restoreCR, func(status *simplyblockv1alpha1.SimplyBlockRestoreStatus) {
			status.Phase = "pending"
			status.Message = fmt.Sprintf("waiting for backup %q to complete", backupCR.Name)
			status.BackupID = backupCR.Status.BackupID
		}, restorePollInterval)
	}

	apiClient := webapi.NewClient()

	targetVolumeID := restoreCR.Status.TargetVolumeID
	if targetVolumeID == "" {
		if existing, err := fetchVolumeByName(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, restoreCR.Spec.Target.VolumeName); err == nil && existing != nil {
			targetVolumeID = existing.ID
		}
	}

	if targetVolumeID == "" {
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/restore", clusterUUID)
		body, statusCode, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, map[string]any{
			"backup_id": backupCR.Status.BackupID,
			"lvol_name": restoreCR.Spec.Target.VolumeName,
			"pool":      restoreCR.Spec.PoolName,
		})
		if err != nil || statusCode >= 300 {
			log.Error(err, "Failed to start restore", "status", statusCode, "response", string(body))
			return r.patchRestoreStatus(ctx, restoreCR, func(status *simplyblockv1alpha1.SimplyBlockRestoreStatus) {
				status.Phase = restorePhaseFailed
				status.Message = fmt.Sprintf("restore request failed: %s", string(body))
				status.BackupID = backupCR.Status.BackupID
			}, 0)
		}

		var payload struct {
			LvolID string `json:"lvol_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Error(err, "Failed to parse restore response")
			return ctrl.Result{RequeueAfter: restorePollInterval}, nil
		}

		return r.patchRestoreStatus(ctx, restoreCR, func(status *simplyblockv1alpha1.SimplyBlockRestoreStatus) {
			status.Phase = "restoring"
			status.Message = ""
			status.BackupID = backupCR.Status.BackupID
			status.TargetVolumeID = payload.LvolID
			status.TargetVolumeName = restoreCR.Spec.Target.VolumeName
			if status.StartedAt == nil {
				now := metav1.Now()
				status.StartedAt = &now
			}
		}, restorePollInterval)
	}

	volume, err := fetchVolumeByID(ctx, apiClient, clusterSecret, clusterUUID, poolUUID, targetVolumeID)
	if err != nil {
		log.Error(err, "Failed to fetch restore target volume")
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}
	if volume == nil {
		return r.patchRestoreStatus(ctx, restoreCR, func(status *simplyblockv1alpha1.SimplyBlockRestoreStatus) {
			status.Phase = restorePhaseFailed
			status.Message = "restored target volume not found"
			status.BackupID = backupCR.Status.BackupID
		}, 0)
	}

	requeueAfter := restorePollInterval
	phase := "restoring"
	if volume.Status == "online" {
		phase = restorePhaseCompleted
		requeueAfter = 0
	} else if volume.Status == "deleted" {
		phase = restorePhaseFailed
		requeueAfter = 0
	}

	return r.patchRestoreStatus(ctx, restoreCR, func(status *simplyblockv1alpha1.SimplyBlockRestoreStatus) {
		status.Phase = phase
		status.Message = ""
		status.BackupID = backupCR.Status.BackupID
		status.TargetVolumeID = volume.ID
		status.TargetVolumeName = volume.Name
		if status.StartedAt == nil {
			now := metav1.Now()
			status.StartedAt = &now
		}
		if phase == restorePhaseCompleted {
			now := metav1.Now()
			status.CompletedAt = &now
		}
	}, requeueAfter)
}

func (r *SimplyBlockRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockRestore{}).
		Named("simplyblockrestore").
		Complete(r)
}

func (r *SimplyBlockRestoreReconciler) patchRestoreStatus(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.SimplyBlockRestore,
	mutate func(*simplyblockv1alpha1.SimplyBlockRestoreStatus),
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	patch := client.MergeFrom(restoreCR.DeepCopy())
	mutate(&restoreCR.Status)
	restoreCR.Status.ObservedGeneration = restoreCR.Generation
	if err := r.Status().Patch(ctx, restoreCR, patch); err != nil {
		return ctrl.Result{RequeueAfter: restorePollInterval}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}
