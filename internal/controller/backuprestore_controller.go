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
	"reflect"
	"strconv"
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
	restoreProgressRequeue  = 10 * time.Second
	restoreReconcileRequeue = 10 * time.Second
	// lvolStatusFailed is the backend status string returned by the lvol polling API when a restore task fails.
	lvolStatusFailed = "failed"
)

const (
	eventReasonRestoreBackupNotFound     = "RestoreBackupNotFound"
	eventReasonRestoreBackupNotReady     = "RestoreBackupNotReady"
	eventReasonRestoreClusterLookupError = "RestoreClusterLookupError"
	eventReasonRestoreClusterAuthError   = "RestoreClusterAuthError"
	eventReasonRestorePoolLookupError    = "RestorePoolLookupError"
	eventReasonRestoreAPIFailed          = "RestoreAPIFailed"
	eventReasonRestoreLvolPollFailed     = "RestoreLvolPollFailed"
	eventReasonRestorePVCreateFailed     = "RestorePVCreateFailed"
	eventReasonRestorePVCCreateFailed    = "RestorePVCCreateFailed"
	eventReasonRestoreInvalidSpec        = "RestoreInvalidSpec"
)

// BackupRestoreReconciler reconciles a BackupRestore object.
type BackupRestoreReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	APIClient *webapi.Client
}

type restoreAPIRequest struct {
	BackupID     string `json:"backup_id"`
	LvolName     string `json:"lvol_name"`
	Pool         string `json:"pool"`
	TargetNodeID string `json:"target_node_id,omitempty"`
}

type sourceSwitchRequest struct {
	SourceClusterID string `json:"source_cluster_id"`
}

type backupSourceAPIResponse struct {
	SourceClusterID string `json:"source_cluster_id"`
	IsLocal         bool   `json:"is_local"`
	Active          bool   `json:"active"`
}

type restoreAPIResponse struct {
	LvolID string `json:"lvol_id"`
}

type restoreLvolStatusResponse struct {
	Status string `json:"status"`
}

type restoreVolumeConnectResponse struct {
	NQN            string `json:"nqn"`
	ReconnectDelay int    `json:"reconnect-delay"`
	NrIoQueues     int    `json:"nr-io-queues"`
	CtrlLossTmo    int    `json:"ctrl-loss-tmo"`
	Port           int    `json:"port"`
	TargetType     string `json:"transport"`
	IP             string `json:"ip"`
	NSID           int    `json:"ns_id"`
	HostIface      string `json:"host-iface,omitempty"`
}

type restoreCSIConnection struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backuprestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backuprestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=backuprestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *BackupRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	restoreCR := &simplyblockv1alpha1.BackupRestore{}
	if err := r.Get(ctx, req.NamespacedName, restoreCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if restoreCR.Status.Phase == simplyblockv1alpha1.RestorePhaseDone ||
		restoreCR.Status.Phase == simplyblockv1alpha1.RestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	clusterUUID, clusterSecret, res, done, err := r.resolveClusterAuth(ctx, restoreCR)
	if done {
		return res, err
	}

	apiClient := r.apiClient()

	if res, done, err = r.reconcileBackupAndPool(ctx, restoreCR, clusterUUID, clusterSecret, apiClient); done {
		return res, err
	}

	// For cross-cluster restores, source-switch the target cluster before submitting the restore task.
	if isCrossCluster(restoreCR) && restoreCR.Status.SourceSwitchedAt == nil {
		if res, done, err = r.reconcileSourceSwitch(ctx, restoreCR, clusterUUID, clusterSecret, apiClient); done {
			return res, err
		}
	}

	if res, done, err = r.reconcileRestoreTask(ctx, restoreCR, clusterUUID, clusterSecret, apiClient); done {
		return res, err
	}

	if restoreCR.Status.Phase == simplyblockv1alpha1.RestorePhaseInProgress {
		if res, done, err = r.reconcileInProgress(ctx, restoreCR, clusterUUID, clusterSecret, apiClient); done {
			return res, err
		}
	}

	// After the lvol comes online, switch the target cluster back to its local bucket.
	if restoreCR.Status.Phase == simplyblockv1alpha1.RestorePhaseSwitchingSourceLocal {
		if res, done, err = r.reconcileSourceSwitchLocal(ctx, restoreCR, clusterUUID, clusterSecret, apiClient); done {
			return res, err
		}
	}

	if restoreCR.Status.Phase == simplyblockv1alpha1.RestorePhasePVCBinding {
		return r.reconcilePVCBinding(ctx, restoreCR, clusterUUID, clusterSecret)
	}

	return ctrl.Result{}, nil
}

func (r *BackupRestoreReconciler) resolveClusterAuth(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
) (clusterUUID, clusterSecret string, res ctrl.Result, done bool, err error) {
	clusterUUID, err = utils.ResolveClusterUUID(ctx, r.Client, restoreCR.Namespace, restoreCR.Spec.ClusterName)
	if err != nil {
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhasePending
			s.Message = err.Error()
		}); patchErr != nil {
			return "", "", ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreClusterLookupError,
			"Failed to resolve cluster UUID for %s: %v", restoreCR.Spec.ClusterName, err)
		return "", "", ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}

	_, clusterSecret, err = utils.GetClusterAuth(ctx, r.Client, restoreCR.Namespace, restoreCR.Spec.ClusterName)
	if err != nil {
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhasePending
			s.Message = err.Error()
		}); patchErr != nil {
			return "", "", ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreClusterAuthError,
			"Failed to get cluster auth for %s: %v", restoreCR.Spec.ClusterName, err)
		return "", "", ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}

	return clusterUUID, clusterSecret, ctrl.Result{}, false, nil
}

func (r *BackupRestoreReconciler) reconcileBackupAndPool(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
) (ctrl.Result, bool, error) {
	backup := &simplyblockv1alpha1.StorageBackup{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      restoreCR.Spec.BackupRef.Name,
		Namespace: restoreCR.Namespace,
	}, backup); err != nil {
		msg := fmt.Sprintf("StorageBackup %q not found: %v", restoreCR.Spec.BackupRef.Name, err)
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhasePending
			s.Message = msg
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreBackupNotFound, "%s", msg)
		return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}

	if backup.Status.Phase != simplyblockv1alpha1.BackupPhaseDone {
		msg := fmt.Sprintf("StorageBackup %q is not ready (phase=%s)", backup.Name, backup.Status.Phase)
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhasePending
			s.Message = msg
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreBackupNotReady, "%s", msg)
		return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}

	// Validate storage request before the restore API call so we fail fast on spec errors.
	// Guard on RestoredLvolID so we don't re-evaluate once the backend task is already running.
	if restoreCR.Status.RestoredLvolID == "" {
		storageReq := restoreCR.Spec.PVCTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
		var specErr string
		switch {
		case storageReq.IsZero():
			specErr = "spec.pvcTemplate.spec.resources.requests.storage must be set"
		case backup.Status.Size > 0 && storageReq.Value() < backup.Status.Size:
			specErr = fmt.Sprintf(
				"requested storage %s (%d bytes) is less than backup size %d bytes",
				storageReq.String(), storageReq.Value(), backup.Status.Size,
			)
		}
		if specErr != "" {
			if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
				s.Phase = simplyblockv1alpha1.RestorePhaseFailed
				s.Message = specErr
			}); patchErr != nil {
				return ctrl.Result{}, true, patchErr
			}
			r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreInvalidSpec, "%s", specErr)
			return ctrl.Result{}, true, nil
		}
	}

	if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
		s.ClusterUUID = clusterUUID
		s.BackupID = backup.Status.BackupID
		s.SourceLvolID = backup.Status.LvolID
		s.SourceClusterUUID = backup.Status.SourceClusterUUID
		if s.Phase == "" {
			s.Phase = simplyblockv1alpha1.RestorePhasePending
		}
	}); patchErr != nil {
		return ctrl.Result{}, true, patchErr
	}

	if restoreCR.Status.PoolName != "" {
		return ctrl.Result{}, false, nil
	}

	poolName, poolUUID, err := r.resolvePool(ctx, apiClient, clusterSecret, clusterUUID, restoreCR, backup)
	if err != nil {
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Message = err.Error()
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestorePoolLookupError,
			"Failed to resolve pool: %v", err)
		return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}
	if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
		s.PoolName = poolName
		s.PoolUUID = poolUUID
	}); patchErr != nil {
		return ctrl.Result{}, true, patchErr
	}

	return ctrl.Result{}, false, nil
}

func (r *BackupRestoreReconciler) reconcileRestoreTask(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
) (ctrl.Result, bool, error) {
	if restoreCR.Status.RestoredLvolID != "" {
		return ctrl.Result{}, false, nil
	}

	log := logf.FromContext(ctx)
	lvolName := fmt.Sprintf("restore-%s", restoreCR.UID)
	lvolID, err := r.callRestoreAPI(ctx, apiClient, clusterSecret, clusterUUID,
		restoreCR.Status.BackupID, lvolName, restoreCR.Status.PoolName, restoreCR.Spec.TargetNode)
	if err != nil {
		log.Error(err, "Restore API call failed", "restore", restoreCR.Name)
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreAPIFailed,
			"Restore API call failed: %v", err)
		res, err := r.handleRestoreAPIError(ctx, restoreCR, err)
		return res, true, err
	}

	now := metav1.Now()
	if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
		s.RestoredLvolID = lvolID
		s.StartedAt = &now
		s.Phase = simplyblockv1alpha1.RestorePhaseInProgress
		s.Message = "Restore task submitted; waiting for lvol to come online"
	}); patchErr != nil {
		return ctrl.Result{}, true, patchErr
	}

	return ctrl.Result{}, false, nil
}

func (r *BackupRestoreReconciler) reconcileInProgress(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
) (ctrl.Result, bool, error) {
	lvolStatus, err := r.pollLvolStatus(ctx, apiClient, clusterSecret,
		clusterUUID, restoreCR.Status.PoolUUID, restoreCR.Status.RestoredLvolID)
	if err != nil {
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Message = fmt.Sprintf("Failed to poll lvol status: %v", err)
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreLvolPollFailed,
			"Failed to poll lvol status: %v", err)
		return ctrl.Result{RequeueAfter: restoreProgressRequeue}, true, nil
	}

	switch lvolStatus {
	case utils.NodeStatusOnline:
		nextPhase := simplyblockv1alpha1.RestorePhasePVCBinding
		nextMsg := "Restore complete; creating PV and PVC"
		if isCrossCluster(restoreCR) {
			nextPhase = simplyblockv1alpha1.RestorePhaseSwitchingSourceLocal
			nextMsg = "Restore complete; switching target cluster back to local backup source"
		}
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = nextPhase
			s.Message = nextMsg
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		return ctrl.Result{}, false, nil
	case lvolStatusFailed:
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhaseFailed
			s.Message = "Backend restore task failed"
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		return ctrl.Result{}, true, nil
	default:
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Message = fmt.Sprintf("Waiting for lvol restore; current status: %s", lvolStatus)
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		return ctrl.Result{RequeueAfter: restoreProgressRequeue}, true, nil
	}
}

func (r *BackupRestoreReconciler) reconcilePVCBinding(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	clusterUUID string,
	clusterSecret string,
) (ctrl.Result, error) {
	pvcName, pvcNamespace := r.resolvedPVCNamespacedName(restoreCR)

	if restoreCR.Status.PVName == "" {
		pvName := fmt.Sprintf("restore-%s", restoreCR.UID)
		if err := r.ensurePV(ctx, restoreCR, pvName, pvcName, pvcNamespace, clusterUUID, clusterSecret); err != nil {
			r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestorePVCreateFailed,
				"Failed to create PV: %v", err)
			if isNonRetryableCreateError(err) {
				if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
					s.Phase = simplyblockv1alpha1.RestorePhaseFailed
					s.Message = fmt.Sprintf("Failed to create PV: %v", err)
				}); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				return ctrl.Result{}, nil
			}
			if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
				s.Message = fmt.Sprintf("Failed to create PV: %v", err)
			}); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, nil
		}
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.PVName = pvName
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
	}

	if restoreCR.Status.PVCName == "" {
		if err := r.ensurePVC(ctx, restoreCR, pvcName, pvcNamespace); err != nil {
			r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestorePVCCreateFailed,
				"Failed to create PVC: %v", err)
			if isNonRetryableCreateError(err) {
				if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
					s.Phase = simplyblockv1alpha1.RestorePhaseFailed
					s.Message = fmt.Sprintf("Failed to create PVC: %v", err)
				}); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				return ctrl.Result{}, nil
			}
			if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
				s.Message = fmt.Sprintf("Failed to create PVC: %v", err)
			}); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, nil
		}
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.PVCName = pvcName
			s.PVCNamespace = pvcNamespace
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: pvcNamespace}, pvc); err != nil {
		return ctrl.Result{RequeueAfter: restoreProgressRequeue}, nil
	}

	if pvc.Status.Phase == corev1.ClaimBound {
		now := metav1.Now()
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhaseDone
			s.Message = fmt.Sprintf("PVC %s/%s is bound", pvcNamespace, pvcName)
			s.CompletedAt = &now
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: restoreProgressRequeue}, nil
}

func isCrossCluster(restoreCR *simplyblockv1alpha1.BackupRestore) bool {
	return restoreCR.Status.SourceClusterUUID != "" &&
		restoreCR.Status.SourceClusterUUID != restoreCR.Status.ClusterUUID
}

func (r *BackupRestoreReconciler) reconcileSourceSwitch(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)

	// Guard: check current active source to detect concurrent cross-cluster restores.
	activeSrc, err := r.getActiveBackupSource(ctx, apiClient, clusterSecret, clusterUUID)
	if err != nil {
		// Non-fatal: proceed if the sources endpoint is unavailable.
		log.Info("Could not read active backup source; proceeding with source-switch", "err", err)
	} else if activeSrc != "" && activeSrc != clusterUUID && activeSrc != restoreCR.Status.SourceClusterUUID {
		msg := fmt.Sprintf("target cluster is already source-switched to %s; retry when that cross-cluster restore completes", activeSrc)
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhaseFailed
			s.Message = msg
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreAPIFailed, "%s", msg)
		return ctrl.Result{}, true, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/source-switch", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, sourceSwitchRequest{
		SourceClusterID: restoreCR.Status.SourceClusterUUID,
	})
	if err != nil || status >= 300 {
		msg := fmt.Sprintf("source-switch to %s failed", restoreCR.Status.SourceClusterUUID)
		if err != nil {
			msg = fmt.Sprintf("%s: %v", msg, err)
		} else {
			msg = fmt.Sprintf("%s: status=%d body=%s", msg, status, string(body))
		}
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Message = msg
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreAPIFailed, "%s", msg)
		return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}

	now := metav1.Now()
	if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
		s.Phase = simplyblockv1alpha1.RestorePhaseSwitchingSource
		s.SourceSwitchedAt = &now
		s.Message = fmt.Sprintf("Switched to source cluster %s; submitting restore task", restoreCR.Status.SourceClusterUUID)
	}); patchErr != nil {
		return ctrl.Result{}, true, patchErr
	}
	return ctrl.Result{}, false, nil
}

func (r *BackupRestoreReconciler) reconcileSourceSwitchLocal(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	clusterUUID, clusterSecret string,
	apiClient *webapi.Client,
) (ctrl.Result, bool, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/source-switch", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, sourceSwitchRequest{
		SourceClusterID: "local",
	})
	if err != nil || status >= 300 {
		msg := "source-switch back to local failed"
		if err != nil {
			msg = fmt.Sprintf("%s: %v", msg, err)
		} else {
			msg = fmt.Sprintf("%s: status=%d body=%s", msg, status, string(body))
		}
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Message = msg
		}); patchErr != nil {
			return ctrl.Result{}, true, patchErr
		}
		r.Recorder.Eventf(restoreCR, corev1.EventTypeWarning, eventReasonRestoreAPIFailed, "%s", msg)
		return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, true, nil
	}

	if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
		s.Phase = simplyblockv1alpha1.RestorePhasePVCBinding
		s.SourceSwitchedAt = nil
		s.Message = "Switched back to local backup source; creating PV and PVC"
	}); patchErr != nil {
		return ctrl.Result{}, true, patchErr
	}
	return ctrl.Result{}, false, nil
}

func (r *BackupRestoreReconciler) getActiveBackupSource(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/sources", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("get backup sources failed: status=%d", status)
	}
	var sources []backupSourceAPIResponse
	if err := json.Unmarshal(body, &sources); err != nil {
		return "", fmt.Errorf("unmarshal backup sources: %w", err)
	}
	for _, s := range sources {
		if s.Active {
			return s.SourceClusterID, nil
		}
	}
	return clusterUUID, nil // no active entry means local
}

func (r *BackupRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.BackupRestore{}).
		Named("backuprestore").
		Complete(r)
}

func (r *BackupRestoreReconciler) apiClient() *webapi.Client {
	if r.APIClient != nil {
		return r.APIClient
	}
	return webapi.NewClient()
}

func (r *BackupRestoreReconciler) resolvePool(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	backup *simplyblockv1alpha1.StorageBackup,
) (poolName, poolUUID string, err error) {
	if restoreCR.Spec.TargetPool != "" {
		poolName = restoreCR.Spec.TargetPool
		poolUUID, err = r.lookupPoolUUID(ctx, apiClient, clusterSecret, clusterUUID, poolName)
		return
	}
	poolName = backup.Status.PoolName
	if poolName == "" {
		err = fmt.Errorf("backup %q has no pool name in status", backup.Name)
		return
	}
	poolUUID = backup.Status.PoolUUID
	if poolUUID == "" {
		poolUUID, err = r.lookupPoolUUID(ctx, apiClient, clusterSecret, clusterUUID, poolName)
	}
	return
}

func (r *BackupRestoreReconciler) lookupPoolUUID(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, poolName string,
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
	for _, p := range pools {
		if p.Name == poolName {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("storage pool %q not found in cluster %s", poolName, clusterUUID)
}

func (r *BackupRestoreReconciler) callRestoreAPI(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, backupID, lvolName, poolName, targetNodeID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/backups/restore", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, restoreAPIRequest{
		BackupID:     backupID,
		LvolName:     lvolName,
		Pool:         poolName,
		TargetNodeID: targetNodeID,
	})
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", apiError{StatusCode: status, Message: fmt.Sprintf("restore API failed: status=%d body=%s", status, string(body))}
	}
	var resp restoreAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("unmarshal restore response: %w", err)
	}
	if resp.LvolID == "" {
		return "", fmt.Errorf("restore API returned empty lvol_id")
	}
	return resp.LvolID, nil
}

func (r *BackupRestoreReconciler) pollLvolStatus(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID, poolUUID, lvolID string,
) (string, error) {
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/volumes/%s/",
		clusterUUID, poolUUID, lvolID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("get lvol status failed: status=%d body=%s", status, string(body))
	}
	var resp restoreLvolStatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("unmarshal lvol status: %w", err)
	}
	return resp.Status, nil
}

func (r *BackupRestoreReconciler) resolvedPVCNamespacedName(
	restoreCR *simplyblockv1alpha1.BackupRestore,
) (name, namespace string) {
	name = restoreCR.Spec.PVCTemplate.Metadata.Name
	if name == "" {
		name = restoreCR.Name + "-restored"
	}
	return name, restoreCR.Namespace
}

func (r *BackupRestoreReconciler) ensurePV(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	pvName, pvcName, pvcNamespace, clusterUUID string,
	clusterSecret string,
) error {
	existing := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvName}, existing); err == nil {
		wantStorageClass := simplyblockStorageClassName(restoreCR.Namespace, restoreCR.Spec.ClusterName, restoreCR.Status.PoolName)
		wantHandle := fmt.Sprintf("%s:%s:%s", clusterUUID, restoreCR.Status.PoolName, restoreCR.Status.RestoredLvolID)
		var mismatch string
		switch {
		case existing.Spec.StorageClassName != wantStorageClass:
			mismatch = fmt.Sprintf("storageClassName %q, expected %q", existing.Spec.StorageClassName, wantStorageClass)
		case existing.Spec.CSI == nil || existing.Spec.CSI.VolumeHandle != wantHandle:
			got := ""
			if existing.Spec.CSI != nil {
				got = existing.Spec.CSI.VolumeHandle
			}
			mismatch = fmt.Sprintf("volumeHandle %q, expected %q", got, wantHandle)
		case existing.Spec.ClaimRef == nil ||
			existing.Spec.ClaimRef.Name != pvcName ||
			existing.Spec.ClaimRef.Namespace != pvcNamespace:
			var gotRef string
			if existing.Spec.ClaimRef != nil {
				gotRef = existing.Spec.ClaimRef.Namespace + "/" + existing.Spec.ClaimRef.Name
			}
			mismatch = fmt.Sprintf("claimRef %q, expected %q", gotRef, pvcNamespace+"/"+pvcName)
		}
		if mismatch != "" {
			return fmt.Errorf("PV %s already exists with unexpected %s", pvName, mismatch)
		}
		return nil
	} else if !kerrors.IsNotFound(err) {
		return fmt.Errorf("get PV %s: %w", pvName, err)
	}

	storageClassName := simplyblockStorageClassName(restoreCR.Namespace, restoreCR.Spec.ClusterName, restoreCR.Status.PoolName)

	storageQty := restoreCR.Spec.PVCTemplate.Spec.Resources.Requests[corev1.ResourceStorage]

	volumeHandle := fmt.Sprintf("%s:%s:%s",
		clusterUUID,
		restoreCR.Status.PoolName,
		restoreCR.Status.RestoredLvolID,
	)
	volumeAttributes, err := r.restoreVolumeAttributes(
		ctx,
		clusterSecret,
		clusterUUID,
		restoreCR.Status.PoolUUID,
		restoreCR.Status.RestoredLvolID,
	)
	if err != nil {
		return fmt.Errorf("build restore volume attributes: %w", err)
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:              storageClassName,
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: storageQty},
			AccessModes:                   restoreCR.Spec.PVCTemplate.Spec.AccessModes,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       pvcName,
				Namespace:  pvcNamespace,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           utils.CSIProvisioner,
					VolumeHandle:     volumeHandle,
					VolumeAttributes: volumeAttributes,
				},
			},
		},
	}

	if restoreCR.Spec.PVCTemplate.Spec.VolumeMode != nil {
		pv.Spec.VolumeMode = restoreCR.Spec.PVCTemplate.Spec.VolumeMode
	}

	return r.Create(ctx, pv)
}

func (r *BackupRestoreReconciler) restoreVolumeAttributes(
	ctx context.Context,
	clusterSecret, clusterUUID, poolUUID, lvolID string,
) (map[string]string, error) {
	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/%s/connect",
		clusterUUID,
		poolUUID,
		lvolID,
	)
	body, status, err := r.apiClient().Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get volume connect info: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("get volume connect info failed: status=%d body=%s", status, string(body))
	}

	var connectInfo []restoreVolumeConnectResponse
	if err := json.Unmarshal(body, &connectInfo); err != nil {
		return nil, fmt.Errorf("unmarshal volume connect info: %w", err)
	}
	if len(connectInfo) == 0 {
		return nil, fmt.Errorf("volume connect info response is empty")
	}

	connections := make([]restoreCSIConnection, 0, len(connectInfo))
	for _, entry := range connectInfo {
		connections = append(connections, restoreCSIConnection{
			IP:   entry.IP,
			Port: entry.Port,
		})
	}
	connectionsJSON, err := json.Marshal(connections)
	if err != nil {
		return nil, fmt.Errorf("marshal CSI connections: %w", err)
	}

	attrs := map[string]string{
		"cluster_id":     clusterUUID,
		"connections":    string(connectionsJSON),
		"ctrlLossTmo":    strconv.Itoa(connectInfo[0].CtrlLossTmo),
		"model":          lvolID,
		"name":           lvolID,
		"nqn":            connectInfo[0].NQN,
		"nrIoQueues":     strconv.Itoa(connectInfo[0].NrIoQueues),
		"nsId":           strconv.Itoa(connectInfo[0].NSID),
		"reconnectDelay": strconv.Itoa(connectInfo[0].ReconnectDelay),
		"targetType":     connectInfo[0].TargetType,
		"uuid":           lvolID,
	}
	if connectInfo[0].HostIface != "" {
		attrs["hostIface"] = connectInfo[0].HostIface
	}

	return attrs, nil
}

func (r *BackupRestoreReconciler) ensurePVC(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	pvcName, pvcNamespace string,
) error {
	existing := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: pvcNamespace}, existing); err == nil {
		if existing.Spec.VolumeName != restoreCR.Status.PVName {
			return fmt.Errorf("PVC %s/%s already exists bound to PV %q, expected %q",
				pvcNamespace, pvcName, existing.Spec.VolumeName, restoreCR.Status.PVName)
		}
		return nil
	} else if !kerrors.IsNotFound(err) {
		return fmt.Errorf("get PVC %s/%s: %w", pvcNamespace, pvcName, err)
	}

	pvcSpec := restoreCR.Spec.PVCTemplate.Spec.DeepCopy()
	pvcSpec.VolumeName = restoreCR.Status.PVName
	sc := simplyblockStorageClassName(restoreCR.Namespace, restoreCR.Spec.ClusterName, restoreCR.Status.PoolName)
	pvcSpec.StorageClassName = &sc

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   pvcNamespace,
			Labels:      restoreCR.Spec.PVCTemplate.Metadata.Labels,
			Annotations: restoreCR.Spec.PVCTemplate.Metadata.Annotations,
		},
		Spec: *pvcSpec,
	}

	return r.Create(ctx, pvc)
}

func (r *BackupRestoreReconciler) handleRestoreAPIError(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	err error,
) (ctrl.Result, error) {
	var apiErr apiError
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
		if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
			s.Phase = simplyblockv1alpha1.RestorePhaseFailed
			s.Message = apiErr.Message
		}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	if patchErr := r.patchStatus(ctx, restoreCR, func(s *simplyblockv1alpha1.BackupRestoreStatus) {
		s.Phase = simplyblockv1alpha1.RestorePhasePending
		s.Message = err.Error()
	}); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{RequeueAfter: restoreReconcileRequeue}, nil
}

// isNonRetryableCreateError returns true for API errors that will never succeed on retry
// regardless of cluster state — invalid spec, semantic validation failure, or permission errors.
func isNonRetryableCreateError(err error) bool {
	return kerrors.IsInvalid(err) ||
		kerrors.IsForbidden(err) ||
		kerrors.IsUnauthorized(err)
}

func (r *BackupRestoreReconciler) patchStatus(
	ctx context.Context,
	restoreCR *simplyblockv1alpha1.BackupRestore,
	mutate func(*simplyblockv1alpha1.BackupRestoreStatus),
) error {
	desired := restoreCR.Status
	mutate(&desired)
	if reflect.DeepEqual(restoreCR.Status, desired) {
		return nil
	}
	patch := client.MergeFrom(restoreCR.DeepCopy())
	restoreCR.Status = desired
	return r.Status().Patch(ctx, restoreCR, patch)
}
