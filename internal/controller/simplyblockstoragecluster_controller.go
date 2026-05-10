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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// Event reason constants for StorageCluster reconciliation.
// These are emitted as Kubernetes Warning events and are visible
// via `kubectl describe storagecluster <name>` under the Events section.
const (
	// eventReasonFDBNotReady is emitted when the FDB health check endpoint
	// returns a non-2xx status or a connection error, indicating the backend
	// is not yet ready to accept cluster creation requests.
	eventReasonFDBNotReady = "FDBNotReady"

	// eventReasonBackupCredentialsError is emitted when the backup credentials
	// Secret referenced by spec.backup.credentialsSecretRef cannot be resolved
	// (missing, unreadable, or lacking required keys).
	eventReasonBackupCredentialsError = "BackupCredentialsError"

	// eventReasonClusterLookupError is emitted when the controller fails to
	// determine whether a cluster already exists in the namespace, preventing
	// it from choosing the correct API endpoint.
	eventReasonClusterLookupError = "ClusterLookupError"

	// eventReasonClusterAuthError is emitted when cluster credentials cannot
	// be retrieved from the cluster Secret, blocking any authenticated API call.
	eventReasonClusterAuthError = "ClusterAuthError"

	// eventReasonClusterCreationFailed is emitted when the cluster creation API
	// call returns a non-2xx status. The event message includes the HTTP status
	// code and the full response body so the root cause is visible without
	// consulting controller logs.
	eventReasonClusterCreationFailed = "ClusterCreationFailed"
)

// StorageClusterReconciler reconciles a StorageCluster object
type StorageClusterReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Namespace string // operator namespace
}

type CSICredentials struct {
	Clusters []CSIClusterEntry `json:"clusters"`
}

type CSIClusterEntry struct {
	ClusterID       string `json:"cluster_id"`
	ClusterEndpoint string `json:"cluster_endpoint"`
	ClusterSecret   string `json:"cluster_secret"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StorageCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *StorageClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the CR directly from the API server (bypasses the informer cache)
	// to avoid a stale UUID="" read after Status().Patch() triggers a new reconcile.
	clusterCR := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, clusterCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	/* -------------------- Deletion -------------------- */
	if res, done, err := r.handleDeletion(ctx, clusterCR); done {
		return res, err
	}

	/* -------------------- Finalizer -------------------- */
	if updated, err := r.ensureFinalizer(ctx, clusterCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	switch clusterCR.Spec.Action {
	case utils.ClusterActionActivate:
		return r.reconcileActivate(ctx, clusterCR)

	case utils.ClusterActionExpand:
		return r.reconcileExpand(ctx, clusterCR)

	case utils.ClusterActionShutdown:
		return r.reconcileShutdown(ctx, clusterCR)

	case utils.ClusterActionStart:
		return r.reconcileStart(ctx, clusterCR)

	case utils.ClusterActionRestart:
		return r.reconcileRestart(ctx, clusterCR)

	case utils.ClusterActionNodeRecycle:
		return r.reconcileNodeRecycle(ctx, clusterCR)
	}

	if clusterCR.Status.UUID != "" {
		return r.syncStatus(ctx, clusterCR)
	}

	return r.reconcileCreate(ctx, clusterCR)
}

func (r *StorageClusterReconciler) reconcileCreate(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	apiClient := webapi.NewClient()
	/* -------------------- Health Check -------------------- */
	endpoint := "/api/v1/health/fdb/"
	body, status, err := apiClient.Do(ctx, "", http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "FDB not ready", "status", status, "response", string(body))
		r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, eventReasonFDBNotReady, "FDB health check failed (status=%d): %s", status, string(body))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	/* -------------------- Create Cluster -------------------- */
	cluster := clusterCR.DeepCopy()
	backupConfig, err := r.buildBackupConfig(ctx, clusterCR)
	if err != nil {
		log.Error(err, "Failed to resolve backup credentials", "secretName", clusterCR.Spec.Backup.CredentialsSecretRef.Name)
		r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, eventReasonBackupCredentialsError, "Failed to resolve backup credentials: %v", err)
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	params := utils.ClusterAddParams{
		Name:             clusterCR.Name,
		BlkSize:          utils.IntPtrOrDefault(clusterCR.Spec.BlockSize, 512),
		PageSizeInBlocks: utils.IntPtrOrDefault(clusterCR.Spec.PageSizeInBlocks, 2097152),
		CapWarn:          capacityThreshold(clusterCR.Spec.WarningThresholdSpec),
		CapCrit:          capacityThreshold(clusterCR.Spec.CriticalThresholdSpec),
		ProvCapWarn:      provisionedCapacityThreshold(clusterCR.Spec.WarningThresholdSpec),
		ProvCapCrit:      provisionedCapacityThreshold(clusterCR.Spec.CriticalThresholdSpec),
		DistrNdcs:        stripeDataChunks(clusterCR.Spec.StripeSpec),
		DistrNpcs:        stripeParityChunks(clusterCR.Spec.StripeSpec),
		// FIXME: Remove distrBs mapping after backend contract clarification.
		DistrBs: 4096,
		// FIXME: Remove distrChunkBs mapping after backend contract clarification.
		DistrChunkBs:           4096,
		HAType:                 clusterCR.Spec.HAType,
		QpairCount:             utils.IntPtrOrDefault(clusterCR.Spec.QpairCount, 256),
		ClientQpairCount:       utils.IntPtrOrDefault(clusterCR.Spec.ClientQpairCount, 3),
		MaxQueueSize:           utils.IntPtrOrDefault(clusterCR.Spec.MaxQueueSize, 128),
		InflightIOThreshold:    utils.IntPtrOrDefault(clusterCR.Spec.InflightIOThreshold, 4),
		EnableNodeAffinity:     utils.BoolPtrOrFalse(clusterCR.Spec.EnableNodeAffinity),
		StrictNodeAntiAffinity: utils.BoolPtrOrFalse(clusterCR.Spec.StrictNodeAntiAffinity),
		IsSingleNode:           utils.BoolPtrOrFalse(clusterCR.Spec.IsSingleNode),
		Fabric:                 clusterCR.Spec.FabricType,
		CRName:                 clusterCR.Name,
		CRNameSpace:            clusterCR.Namespace,
		CRPlural:               "storageclusters",
		ClientDataIfname:       clusterCR.Spec.ClientDataIfname,
		MaxFaultTolerance:      utils.IntPtrOrDefault(clusterCR.Spec.MaxFaultTolerance, 1),
		NvmfBasePort:           utils.IntPtrOrDefault(clusterCR.Spec.NvmfBasePort, 4420),
		RpcBasePort:            utils.IntPtrOrDefault(clusterCR.Spec.RpcBasePort, 8080),
		SnodeApiPort:           utils.IntPtrOrDefault(clusterCR.Spec.SnodeApiPort, 50001),
		BackupConfig:           backupConfig,
		HashicorpVaultSettings: buildHashicorpVaultConfig(clusterCR.Spec.HashicorpVaultSettings),
	}

	endpoint = "/api/v1/cluster/create_first/"
	clusterSecret := ""

	exists, clusterUUID, clusterName, clusterNamespace, err := utils.ExistingClusterUUID(ctx, r.Client)
	if err != nil {
		log.Error(err, "Failed to check existing cluster")
		r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, eventReasonClusterLookupError, "Failed to check existing cluster: %v", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if exists {
		endpoint = "/api/v2/clusters/"

		_, clusterSecret, err = utils.GetClusterAuth(ctx, r.Client, clusterNamespace, clusterName)
		if err != nil {
			log.Error(
				err,
				"Failed to get cluster auth",
				"clusterName", clusterName,
				"clusterUUID", clusterUUID,
			)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, eventReasonClusterAuthError, "Failed to get cluster auth for %s: %v", clusterName, err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	body, status, err = apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		// POST failed — the cluster may already exist on the backend (race between
		// two reconciles both seeing UUID="" before the first one patches status).
		// Try to look it up by name and adopt it instead of failing.
		existing, lookupErr := utils.GetClusterByName(ctx, apiClient, clusterSecret, clusterCR.Name)
		if lookupErr != nil || existing == nil {
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, eventReasonClusterCreationFailed, "Cluster creation failed (status=%d): %s", status, string(body))

			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}
		log.Info("Cluster already exists on backend, adopting", "clusterName", existing.Name, "uuid", existing.UUID)
		return r.adoptExistingCluster(ctx, clusterCR, existing)
	}

	log.Info("Cluster API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	secretName := fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Name)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: clusterCR.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(clusterCR, secret, r.Scheme); err != nil {
		log.Error(err, "Failed to set owner reference on cluster secret")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// original := clusterCR.DeepCopy()

	apiResp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "Unable to parse cluster creation response", "raw", string(body))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if endpoint == "/api/v1/cluster/create_first/" {
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data["uuid"] = []byte(apiResp.UUID)
			secret.Data["secret"] = []byte(apiResp.Secret)
			return nil
		})

		if err != nil {
			log.Error(err, "Failed to create/update Secret for cluster")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		err := r.upsertCSICredentialsSecret(
			ctx,
			r.Namespace,
			apiResp.UUID,
			utils.ENDPOINT,
			apiResp.Secret,
		)

		if err != nil {
			log.Error(err, "Failed to update CSI credentials secret")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		clusterCR.Status.UUID = apiResp.UUID
		clusterCR.Status.Rebalancing = &apiResp.Rebalancing
		clusterCR.Status.Status = apiResp.Status
		clusterCR.Status.NQN = apiResp.NQN
		clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", apiResp.NDCS, apiResp.NPCS)
	} else {
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data["uuid"] = []byte(apiResp.UUID)
			secret.Data["secret"] = []byte(apiResp.Secret)
			return nil
		})

		if err != nil {
			log.Error(err, "Failed to create/update Secret for cluster")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		err := r.upsertCSICredentialsSecret(
			ctx,
			r.Namespace,
			apiResp.UUID,
			utils.ENDPOINT,
			apiResp.Secret,
		)

		if err != nil {
			log.Error(err, "Failed to update CSI credentials secret")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		clusterCR.Status.UUID = apiResp.UUID
		clusterCR.Status.Rebalancing = &apiResp.Rebalancing
		clusterCR.Status.Status = apiResp.Status
		clusterCR.Status.NQN = apiResp.NQN
		clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", apiResp.NDCS, apiResp.NPCS)
	}

	clusterCR.Status.ClusterName = clusterCR.Name
	clusterCR.Status.SecretName = fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Name)
	clusterCR.Status.Configured = true

	patch := client.MergeFrom(cluster)

	if err := r.Status().Patch(ctx, clusterCR, patch); err != nil {
		log.Error(err, "Failed to patch cluster status after creation")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Cluster successfully created", "name", clusterCR.Name)

	// clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	// if err != nil {
	// 	log.Error(err, "Failed to get cluster auth")
	// 	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	// }

	// endpoint := fmt.Sprintf("/api/v2/clusters/%s/update", clusterUUID)

	// body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, updateParams)
	// if err != nil || status >= 300 {
	// 	log.Error(err, "Cluster update failed", "status", status, "response", string(body))
	// 	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	// }

	// log.Info("Cluster updated successfully", "name", clusterCR.Name)
	return ctrl.Result{}, nil
}

func (r *StorageClusterReconciler) adoptExistingCluster(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	existing *utils.ClusterListEntry,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	secretName := fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: clusterCR.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(clusterCR, secret, r.Scheme); err != nil {
		log.Error(err, "Failed to set owner reference on cluster secret")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["uuid"] = []byte(existing.UUID)
		secret.Data["secret"] = []byte(existing.Secret)
		return nil
	})
	if err != nil {
		log.Error(err, "Failed to create/update Secret for adopted cluster")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err := r.upsertCSICredentialsSecret(ctx, r.Namespace, existing.UUID, utils.ENDPOINT, existing.Secret); err != nil {
		log.Error(err, "Failed to update CSI credentials secret for adopted cluster")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	orig := clusterCR.DeepCopy()
	clusterCR.Status.UUID = existing.UUID
	clusterCR.Status.NQN = existing.NQN
	clusterCR.Status.Status = existing.Status
	clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", existing.NDCS, existing.NPCS)
	clusterCR.Status.ClusterName = clusterCR.Name
	clusterCR.Status.SecretName = secretName
	clusterCR.Status.Configured = true
	if err := r.Status().Patch(ctx, clusterCR, client.MergeFrom(orig)); err != nil {
		log.Error(err, "Failed to patch cluster status after adoption")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *StorageClusterReconciler) buildBackupConfig(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (*utils.BackupConfig, error) {
	if clusterCR.Spec.Backup == nil {
		return nil, nil
	}

	secretName := clusterCR.Spec.Backup.CredentialsSecretRef.Name
	if secretName == "" {
		return nil, fmt.Errorf("backup.credentialsSecretRef.name is required")
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: clusterCR.Namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("get backup credentials secret %q: %w", secretName, err)
	}

	accessKeyID, ok := secret.Data["access_key_id"]
	if !ok {
		return nil, fmt.Errorf("secret %q missing key %q", secretName, "access_key_id")
	}

	secretAccessKey, ok := secret.Data["secret_access_key"]
	if !ok {
		return nil, fmt.Errorf("secret %q missing key %q", secretName, "secret_access_key")
	}

	return &utils.BackupConfig{
		AccessKeyID:     string(accessKeyID),
		SecretAccessKey: string(secretAccessKey),
		LocalEndpoint:   clusterCR.Spec.Backup.LocalEndpoint,
		SnapshotBackups: clusterCR.Spec.Backup.SnapshotBackups,
		WithCompression: clusterCR.Spec.Backup.WithCompression,
		SecondaryTarget: clusterCR.Spec.Backup.SecondaryTarget,
		LocalTesting:    clusterCR.Spec.Backup.LocalTesting,
	}, nil
}

func buildHashicorpVaultConfig(s *simplyblockv1alpha1.HashicorpVaultSettings) *utils.HashicorpVaultConfig {
	if s == nil || s.BaseURL == "" {
		return nil
	}
	return &utils.HashicorpVaultConfig{BaseURL: s.BaseURL}
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageCluster{}).
		Named("storagecluster").
		Complete(r)
}

func (r *StorageClusterReconciler) handleDeletion(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, bool, error) {

	log := logf.FromContext(ctx)

	if clusterCR.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, false, nil
	}

	log.Info("Handling deletion", "name", clusterCR.Name)

	if !controllerutil.ContainsFinalizer(clusterCR, utils.FinalizerStorageCluster) {
		return ctrl.Result{}, true, nil
	}

	if clusterCR.Spec.Action == utils.ClusterActionActivate {
		controllerutil.RemoveFinalizer(clusterCR, utils.FinalizerStorageCluster)
		return ctrl.Result{}, true, r.Update(ctx, clusterCR)
	}

	if clusterCR.Status.UUID == "" {
		log.Info("Cluster has no UUID, removing finalizer without API call", "name", clusterCR.Name)
		controllerutil.RemoveFinalizer(clusterCR, utils.FinalizerStorageCluster)
		return ctrl.Result{}, true, r.Update(ctx, clusterCR)
	}

	clusterUUID, clusterSecret, err :=
		utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Error(err, "Failed to get cluster auth during deletion, will retry", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, true, nil
	}

	apiClient := webapi.NewClient()
	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Cluster DELETE API call failed, will retry", "name", clusterCR.Name, "status", status, "clusterUUID", clusterUUID, "response", string(body))
		return ctrl.Result{RequeueAfter: 20 * time.Second}, true, nil
	}

	log.Info("Cluster deleted via API", "name", clusterCR.Name, "clusterUUID", clusterUUID)

	if err := r.removeCSICredentialsEntry(ctx, r.Namespace, clusterUUID); err != nil {
		log.Error(err, "Failed to remove CSI credentials entry, will retry", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, true, nil
	}

	if err := r.deleteClusterSecret(ctx, clusterCR); err != nil {
		log.Error(err, "Failed to delete cluster secret, will retry", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, true, nil
	}

	controllerutil.RemoveFinalizer(clusterCR, utils.FinalizerStorageCluster)
	return ctrl.Result{}, true, r.Update(ctx, clusterCR)
}

func (r *StorageClusterReconciler) ensureFinalizer(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (bool, error) {

	if controllerutil.ContainsFinalizer(clusterCR, utils.FinalizerStorageCluster) {
		return false, nil
	}

	controllerutil.AddFinalizer(clusterCR, utils.FinalizerStorageCluster)
	return true, r.Update(ctx, clusterCR)
}

func (r *StorageClusterReconciler) deleteClusterSecret(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) error {

	secretName := clusterCR.Status.SecretName
	if secretName == "" {
		secretName = fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Name)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: clusterCR.Namespace,
		},
	}

	if err := r.Delete(ctx, secret); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	return nil
}

func (r *StorageClusterReconciler) reconcileActivate(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {

	log := logf.FromContext(ctx)

	if clusterCR.Status.ActionStatus != nil &&
		clusterCR.Status.ActionStatus.Action == utils.ClusterActionActivate &&
		clusterCR.Status.ActionStatus.State == utils.ActionStateSuccess &&
		clusterCR.Status.ActionStatus.ObservedGeneration == clusterCR.Generation {
		return ctrl.Result{}, nil
	}

	// --- Initialize action ---
	if clusterCR.Status.ActionStatus == nil ||
		clusterCR.Status.ActionStatus.Action != utils.ClusterActionActivate {

		clusterCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:             utils.ClusterActionActivate,
			State:              utils.ActionStateRunning,
			ObservedGeneration: clusterCR.Generation,
		}

		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, clusterCR)
	}

	if clusterCR.Status.ActionStatus.State == utils.ActionStateRunning &&
		!clusterCR.Status.ActionStatus.Triggered {

		clusterUUID, clusterSecret, err :=
			utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
		if err != nil {
			return r.failActivate(ctx, clusterCR, err)
		}

		apiClient := webapi.NewClient()
		endpoint := fmt.Sprintf("/api/v2/clusters/%s/activate", clusterUUID)

		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d", status)
			}
			log.Error(err, "Cluster activate API call failed", "cluster", clusterCR.Name, "status", status, "clusterUUID", clusterUUID, "response", string(body))
			return r.failActivate(ctx, clusterCR,
				fmt.Errorf("activate API failed: status=%d err=%v", status, err))
		}

		log.Info("Cluster activate API called", "cluster", clusterCR.Name)

		clusterCR.Status.ActionStatus.Triggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		clusterCR.Namespace,
		clusterCR.Name,
	)

	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", clusterCR.Name,
		)
		return r.failActivate(ctx, clusterCR, err)
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return r.failActivate(ctx, clusterCR, err)
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Cluster GET API call failed during activate poll", "cluster", clusterCR.Name, "status", status, "clusterUUID", clusterUUID, "response", string(body))
		return r.failActivate(ctx, clusterCR, err)
	}

	log.Info("Cluster API Activate call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		return r.failActivate(ctx, clusterCR, err)
	}

	if resp.Status == utils.ClusterStatusActive {
		clusterCR.Status.Status = utils.ClusterStatusActive
		clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
		clusterCR.Status.ActionStatus.Message = "Cluster activated successfully"
		clusterCR.Status.UUID = resp.UUID
		clusterCR.Status.NQN = resp.NQN
		clusterCR.Status.ClusterName = clusterCR.Name
		clusterCR.Status.Configured = true
		clusterCR.Status.Rebalancing = &resp.Rebalancing
		clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", resp.NDCS, resp.NPCS)

		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{}, err
		}

		log.Info("Cluster activated successfully", "cluster", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *StorageClusterReconciler) reconcileExpand(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {

	log := logf.FromContext(ctx)

	if clusterCR.Status.ActionStatus != nil &&
		clusterCR.Status.ActionStatus.Action == utils.ClusterActionExpand &&
		clusterCR.Status.ActionStatus.State == utils.ActionStateSuccess &&
		clusterCR.Status.ActionStatus.ObservedGeneration == clusterCR.Generation {
		return ctrl.Result{}, nil
	}

	if clusterCR.Status.ActionStatus == nil ||
		clusterCR.Status.ActionStatus.Action != utils.ClusterActionExpand {

		clusterCR.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:             utils.ClusterActionExpand,
			State:              utils.ActionStateRunning,
			ObservedGeneration: clusterCR.Generation,
		}

		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, clusterCR)
	}

	clusterUUID, clusterSecret, err :=
		utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		return r.failExpand(ctx, clusterCR, err)
	}

	apiClient := webapi.NewClient()

	if clusterCR.Status.ActionStatus.State == utils.ActionStateRunning &&
		!clusterCR.Status.ActionStatus.Triggered {

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/expand", clusterUUID)

		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, nil)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d", status)
			}
			log.Error(err, "Cluster expand API call failed", "cluster", clusterCR.Name, "status", status, "clusterUUID", clusterUUID, "response", string(body))
			return r.failExpand(ctx, clusterCR,
				fmt.Errorf("expand API failed: status=%d err=%v", status, err))
		}

		log.Info("Cluster expand API called", "cluster", clusterCR.Name)

		clusterCR.Status.ActionStatus.Triggered = true
		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Cluster GET API call failed during expand poll", "cluster", clusterCR.Name, "status", status, "clusterUUID", clusterUUID, "response", string(body))
		return r.failExpand(ctx, clusterCR, err)
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		return r.failExpand(ctx, clusterCR, err)
	}

	if resp.Status == utils.ClusterStatusActive {
		clusterCR.Status.Status = utils.ClusterStatusActive
		clusterCR.Status.ActionStatus.State = utils.ActionStateSuccess
		clusterCR.Status.ActionStatus.Message = "Cluster expanded successfully"
		clusterCR.Status.UUID = resp.UUID
		clusterCR.Status.NQN = resp.NQN
		clusterCR.Status.ClusterName = clusterCR.Name
		clusterCR.Status.Configured = true
		clusterCR.Status.Rebalancing = &resp.Rebalancing
		clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", resp.NDCS, resp.NPCS)

		if err := r.Status().Update(ctx, clusterCR); err != nil {
			return ctrl.Result{}, err
		}

		log.Info("Cluster expansion completed", "cluster", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *StorageClusterReconciler) failActivate(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	err error,
) (ctrl.Result, error) {

	clusterCR.Status.ActionStatus.State = utils.ActionStateFailed
	clusterCR.Status.ActionStatus.Message = err.Error()

	_ = r.Status().Update(ctx, clusterCR)

	return ctrl.Result{}, nil
}

func (r *StorageClusterReconciler) failExpand(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	err error,
) (ctrl.Result, error) {

	clusterCR.Status.ActionStatus.State = utils.ActionStateFailed
	clusterCR.Status.ActionStatus.Message = err.Error()

	_ = r.Status().Update(ctx, clusterCR)

	return ctrl.Result{}, nil
}

func (r *StorageClusterReconciler) upsertCSICredentialsSecret(
	ctx context.Context,
	namespace string,
	clusterID string,
	clusterEndpoint string,
	clusterSecret string,
) error {
	secretName := "simplyblock-csi-secret-v2"

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespace,
			},
		}

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			var creds CSICredentials

			if data, ok := secret.Data["secret.json"]; ok {
				_ = json.Unmarshal(data, &creds)
			}

			for _, c := range creds.Clusters {
				if c.ClusterID == clusterID {
					return nil
				}
			}

			creds.Clusters = append(creds.Clusters, CSIClusterEntry{
				ClusterID:       clusterID,
				ClusterEndpoint: clusterEndpoint,
				ClusterSecret:   clusterSecret,
			})

			payload, err := json.MarshalIndent(creds, "", "  ")
			if err != nil {
				return err
			}

			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}

			secret.Data["secret.json"] = payload
			return nil
		})
		return err
	})
}

func (r *StorageClusterReconciler) removeCSICredentialsEntry(
	ctx context.Context,
	namespace string,
	clusterID string,
) error {
	secretName := "simplyblock-csi-secret-v2"

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespace,
			},
		}

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			var creds CSICredentials

			if data, ok := secret.Data["secret.json"]; ok {
				_ = json.Unmarshal(data, &creds)
			}

			filtered := creds.Clusters[:0]
			for _, c := range creds.Clusters {
				if c.ClusterID != clusterID {
					filtered = append(filtered, c)
				}
			}
			creds.Clusters = filtered

			payload, err := json.MarshalIndent(creds, "", "  ")
			if err != nil {
				return err
			}

			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data["secret.json"] = payload
			return nil
		})
		return err
	})
}

// syncStatus fetches live cluster status from the backend API and patches the
// CR status when it differs from the last observed value. It requeues every 30
// seconds so transient backend transitions (degraded, suspended, in_expansion,
// read_only) are reflected in the CR without any user-initiated action.
func (r *StorageClusterReconciler) syncStatus(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Error(err, "syncStatus: failed to get cluster auth", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.upsertCSICredentialsSecret(ctx, r.Namespace, clusterCR.Status.UUID, utils.ENDPOINT, clusterSecret); err != nil {
		log.Error(err, "syncStatus: failed to upsert CSI credentials secret", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	apiClient := webapi.NewClient()
	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterCR.Status.UUID)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "syncStatus: GET cluster failed", "name", clusterCR.Name, "status", status)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	resp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "syncStatus: failed to parse cluster response", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if resp.Status == clusterCR.Status.Status &&
		resp.NQN == clusterCR.Status.NQN &&
		(clusterCR.Status.Rebalancing == nil || resp.Rebalancing == *clusterCR.Status.Rebalancing) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	patch := client.MergeFrom(clusterCR.DeepCopy())
	clusterCR.Status.Status = resp.Status
	clusterCR.Status.NQN = resp.NQN
	clusterCR.Status.Rebalancing = &resp.Rebalancing
	clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", resp.NDCS, resp.NPCS)

	if err := r.Status().Patch(ctx, clusterCR, patch); err != nil {
		log.Error(err, "syncStatus: failed to patch cluster status", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	log.Info("syncStatus: cluster status updated", "name", clusterCR.Name, "status", resp.Status)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func capacityThreshold(t *simplyblockv1alpha1.CapacityThresholdSpec) int {
	if t == nil {
		return 0
	}
	return utils.IntPtrOrZero(t.Capacity)
}

func provisionedCapacityThreshold(t *simplyblockv1alpha1.CapacityThresholdSpec) int {
	if t == nil {
		return 0
	}
	return utils.IntPtrOrZero(t.ProvisionedCapacity)
}

func stripeDataChunks(s *simplyblockv1alpha1.StripeSpec) int {
	if s == nil {
		return 1
	}
	return utils.IntPtrOrDefault(s.DataChunks, 1)
}

func stripeParityChunks(s *simplyblockv1alpha1.StripeSpec) int {
	if s == nil {
		return 1
	}
	return utils.IntPtrOrDefault(s.ParityChunks, 1)
}
