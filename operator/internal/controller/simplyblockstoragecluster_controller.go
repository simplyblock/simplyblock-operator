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

	"github.com/simplyblock/atlas/net"
	"github.com/simplyblock/atlas/ptr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

	// eventReasonInvalidConfig is emitted when a user-supplied field in the CR
	// fails validation (e.g. a non-HTTPS or private-IP URL).
	eventReasonInvalidConfig = "InvalidConfig"

	// eventReasonClusterCreationFailed is emitted when the cluster creation API
	// call returns a non-2xx status. The event message includes the HTTP status
	// code and the full response body so the root cause is visible without
	// consulting controller logs.
	eventReasonClusterCreationFailed = "ClusterCreationFailed"

	// clusterPhaseCreation is written to Status.Phase before making the backend
	// cluster creation call. The optimistic-lock patch that sets this value acts
	// as a distributed mutex: a concurrent reconciler that also sees UUID="" will
	// receive a 409 Conflict and back off, preventing duplicate cluster creation.
	clusterPhaseCreation = "creation"

	// clusterSubPhaseCreating is the only Status.SubPhase value used during
	// cluster creation today. It is a placeholder for future sub-state machine
	// expansion (e.g. "preparing", "waitingForReachable").
	clusterSubPhaseCreating = "creating"
)

// StorageClusterReconciler reconciles a StorageCluster object
type StorageClusterReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
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

	/* -------------------- Claim creation slot -------------------- */
	// Use an optimistic-lock patch to atomically set Phase="creation".
	// If a concurrent reconciler already patched this version, the Kubernetes
	// API server returns 409 and we back off — preventing duplicate cluster creation.
	base := clusterCR.DeepCopy()
	clusterCR.Status.Phase = clusterPhaseCreation
	clusterCR.Status.SubPhase = clusterSubPhaseCreating
	if patchErr := r.Status().Patch(ctx, clusterCR, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil {
		log.Info("Creation already claimed by another reconciler, backing off", "name", clusterCR.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	apiClient := webapi.NewClient()
	/* -------------------- Readiness Check -------------------- */
	endpoint := "/api/v2/_meta/ready"
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "control plane not ready", "status", status, "response", string(body))
		r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, eventReasonFDBNotReady, eventReasonFDBNotReady, "Control plane readiness check failed (status=%d): %s", status, string(body))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	/* -------------------- Proactive Adoption Check (Helm upgrade) -------------------- */
	// To migrate from a Helm deployment, create a Secret named
	// "simplyblock-{clusterName}-upgrade" in the same namespace containing
	// the cluster's "uuid" and "secret" fields. The operator uses them to
	// fetch the existing cluster and populate the CR status without POSTing.
	upgradeSecretName := fmt.Sprintf("simplyblock-%s-upgrade", clusterCR.Name)
	upgradeSecret := &corev1.Secret{}
	if getErr := r.Get(ctx, types.NamespacedName{Name: upgradeSecretName, Namespace: clusterCR.Namespace}, upgradeSecret); getErr == nil {
		upgradeUUID := string(upgradeSecret.Data["uuid"])
		upgradeClusterSecret := string(upgradeSecret.Data["secret"])
		if upgradeUUID != "" && upgradeClusterSecret != "" {
			clusterEndpoint := fmt.Sprintf("/api/v2/clusters/%s", upgradeUUID)
			upgradeBody, upgradeStatus, upgradeErr := apiClient.Do(ctx, http.MethodGet, clusterEndpoint, nil)
			if upgradeErr == nil && upgradeStatus < 300 {
				if clusterResp, parseErr := webapi.ParseClusterResponse(upgradeBody); parseErr == nil {
					adoptSecret := upgradeClusterSecret
					if clusterResp.Secret != "" {
						adoptSecret = clusterResp.Secret
					}
					existing := &utils.ClusterListEntry{
						UUID:   clusterResp.UUID,
						Secret: adoptSecret,
						Name:   clusterCR.Name,
						NQN:    clusterResp.NQN,
						Status: clusterResp.Status,
						NDCS:   clusterResp.NDCS,
						NPCS:   clusterResp.NPCS,
					}
					log.Info("Cluster found via upgrade secret, adopting", "clusterName", clusterCR.Name, "uuid", existing.UUID)
					return r.adoptExistingCluster(ctx, clusterCR, existing)
				}
			}
		}
	}

	/* -------------------- Create Cluster -------------------- */
	cluster := clusterCR.DeepCopy()
	backupConfig, err := r.buildBackupConfig(ctx, clusterCR)
	if err != nil {
		log.Error(err, "Failed to resolve backup credentials", "secretName", clusterCR.Spec.Backup.CredentialsSecretRef.Name)
		r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, eventReasonBackupCredentialsError, eventReasonBackupCredentialsError, "Failed to resolve backup credentials: %v", err)
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	vaultConfig, err := buildHashicorpVaultConfig(clusterCR.Spec.HashicorpVaultSettings)
	if err != nil {
		log.Error(err, "Invalid HashiCorp Vault configuration")
		r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, eventReasonInvalidConfig, eventReasonInvalidConfig, "Invalid HashiCorp Vault configuration: %v", err)
		return ctrl.Result{}, err
	}

	params := utils.ClusterAddParams{
		Name:                   clusterCR.Name,
		BlkSize:                ptr.IntFrom(clusterCR.Spec.BlockSize, 512),
		PageSizeInBlocks:       ptr.IntFrom(clusterCR.Spec.PageSizeInBlocks, 2097152),
		CapWarn:                capacityThreshold(clusterCR.Spec.WarningThresholdSpec),
		CapCrit:                capacityThreshold(clusterCR.Spec.CriticalThresholdSpec),
		ProvCapWarn:            provisionedCapacityThreshold(clusterCR.Spec.WarningThresholdSpec),
		ProvCapCrit:            provisionedCapacityThreshold(clusterCR.Spec.CriticalThresholdSpec),
		DistrNdcs:              stripeDataChunks(clusterCR.Spec.StripeSpec),
		DistrNpcs:              stripeParityChunks(clusterCR.Spec.StripeSpec),
		HAType:                 clusterCR.Spec.HAType,
		QpairCount:             ptr.IntFrom(clusterCR.Spec.QpairCount, 256),
		ClientQpairCount:       ptr.IntFrom(clusterCR.Spec.ClientQpairCount, 3),
		MaxQueueSize:           ptr.IntFrom(clusterCR.Spec.MaxQueueSize, 128),
		InflightIOThreshold:    ptr.IntFrom(clusterCR.Spec.InflightIOThreshold, 4),
		EnableNodeAffinity:     ptr.BoolFromOrFalse(clusterCR.Spec.EnableNodeAffinity),
		StrictNodeAntiAffinity: ptr.BoolFromOrFalse(clusterCR.Spec.StrictNodeAntiAffinity),
		IsSingleNode:           ptr.BoolFromOrFalse(clusterCR.Spec.IsSingleNode),
		Fabric:                 clusterCR.Spec.FabricType,
		CRName:                 clusterCR.Name,
		CRNameSpace:            clusterCR.Namespace,
		CRPlural:               "storageclusters",
		ClientDataIfname:       clusterCR.Spec.ClientDataIfname,
		MaxFaultTolerance:      ptr.IntFrom(clusterCR.Spec.MaxFaultTolerance, 1),
		NvmfBasePort:           ptr.IntFrom(clusterCR.Spec.NvmfBasePort, 4420),
		RpcBasePort:            ptr.IntFrom(clusterCR.Spec.RpcBasePort, 8080),
		SnodeApiPort:           ptr.IntFrom(clusterCR.Spec.SnodeApiPort, 50001),
		BackupConfig:           backupConfig,
		HashicorpVaultSettings: vaultConfig,
		EnableFailureDomain:    ptr.BoolFromOrFalse(clusterCR.Spec.EnableFailureDomains),
	}

	endpoint = "/api/v2/clusters/"

	body, status, err = apiClient.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		// POST failed — the cluster may already exist on the backend (race between
		// two reconciles both seeing UUID="" before the first one patches status).
		// Try to look it up by name and adopt it instead of failing.
		existing, lookupErr := utils.GetClusterByName(ctx, apiClient, clusterCR.Name)
		if lookupErr != nil || existing == nil {
			r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, eventReasonClusterCreationFailed, eventReasonClusterCreationFailed, "Cluster creation failed (status=%d): %s", status, string(body))

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

	// original := clusterCR.DeepCopy()

	apiResp, err := webapi.ParseClusterResponse(body)
	if err != nil {
		log.Error(err, "Unable to parse cluster creation response", "raw", string(body))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err = r.upsertCSICredentialsSecret(
		ctx,
		r.Namespace,
		apiResp.UUID,
		utils.ENDPOINT,
		apiResp.Secret,
	); err != nil {
		log.Error(err, "Failed to update CSI credentials secret")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	clusterCR.Status.UUID = apiResp.UUID
	clusterCR.Status.Phase = ""
	clusterCR.Status.SubPhase = ""
	clusterCR.Status.Rebalancing = &apiResp.Rebalancing
	clusterCR.Status.Status = apiResp.Status
	clusterCR.Status.NQN = apiResp.NQN
	clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", apiResp.NDCS, apiResp.NPCS)
	mft := int32(apiResp.MaxFaultTolerance)
	clusterCR.Status.MaxFaultTolerance = &mft

	clusterCR.Status.ClusterName = clusterCR.Name
	clusterCR.Status.Configured = true

	patch := client.MergeFrom(cluster)

	if err := r.Status().Patch(ctx, clusterCR, patch); err != nil {
		log.Error(err, "Failed to patch cluster status after creation")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Cluster successfully created", "name", clusterCR.Name)
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
	clusterCR.Status.Phase = ""
	clusterCR.Status.SubPhase = ""
	clusterCR.Status.NQN = existing.NQN
	clusterCR.Status.Status = existing.Status
	clusterCR.Status.ErasureCodingScheme = fmt.Sprintf("%dx%d", existing.NDCS, existing.NPCS)
	clusterCR.Status.ClusterName = clusterCR.Name
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

	if ep := clusterCR.Spec.Backup.LocalEndpoint; ep != "" {
		if err := net.ValidateExternalURL(ep); err != nil {
			return nil, fmt.Errorf("backup.localEndpoint: %w", err)
		}
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

func buildHashicorpVaultConfig(s *simplyblockv1alpha1.HashicorpVaultSettings) (*utils.HashicorpVaultConfig, error) {
	if s == nil || s.BaseURL == "" {
		return nil, nil
	}
	if err := net.ValidateExternalURL(s.BaseURL); err != nil {
		return nil, fmt.Errorf("hashicorpVault.baseURL: %w", err)
	}
	return &utils.HashicorpVaultConfig{BaseURL: s.BaseURL}, nil
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

	if clusterCR.Status.UUID == "" {
		log.Info("Cluster has no UUID, removing finalizer without API call", "name", clusterCR.Name)
		controllerutil.RemoveFinalizer(clusterCR, utils.FinalizerStorageCluster)
		return ctrl.Result{}, true, r.Update(ctx, clusterCR)
	}

	clusterUUID := clusterCR.Status.UUID

	apiClient := webapi.NewClient()
	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)

	body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
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

// clusterFailureDomainHosts aggregates each host's (by management IP)
// failure domain across every StorageNodeSet belonging to clusterName in
// namespace. Hosts with no failure-domain assignment reported to status
// yet are skipped, not treated as domain 0.
//
// reconcileActivate/reconcileExpand moved to StorageClusterOpsReconciler
// (storageclusterops_controller.go) when cluster operations were decoupled
// from this reconciler (#397); this pair of pure helpers has no dependency
// on the old receiver and is kept here for the FD-activation-readiness gate
// in the new reconcileActivate to call.
func clusterFailureDomainHosts(
	ctx context.Context, c client.Client, namespace, clusterName string,
) (map[string]int32, error) {
	var snList simplyblockv1alpha1.StorageNodeSetList
	if err := c.List(ctx, &snList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	hostDomains := map[string]int32{}
	for _, sn := range snList.Items {
		if sn.Spec.ClusterName != clusterName {
			continue
		}
		for _, ns := range sn.Status.Nodes {
			if ns.FailureDomain == nil || ns.MgmtIp == "" {
				continue
			}
			hostDomains[ns.MgmtIp] = *ns.FailureDomain
		}
	}
	return hostDomains, nil
}

// fdActivationDomainCountViolation validates the number of distinct failure
// domains and per-domain host balance for fresh activation, mirroring
// simplyblock_core's fd_activation_domain_count_violation (Python side) so
// the two stay in lockstep.
//
// A 2-FD layout can never absorb a second independent failure once one
// domain is fully down, so fresh activation requires npcs+2 distinct
// domains (3 for npcs=1, 4 for npcs=2) with an EQUAL host count in each --
// below npcs+1 domains even the initial static role-placement rotation is
// structurally wrong; at exactly npcs+1 it is correct but has zero spare
// capacity for a later single add/remove. Returns a human-readable reason
// when the cluster isn't ready yet, "" when it is.
func fdActivationDomainCountViolation(npcs int, hostDomains map[string]int32) string {
	if len(hostDomains) == 0 {
		return "no storage nodes with a failure-domain assignment reported yet"
	}
	counts := map[int32]int{}
	for _, fd := range hostDomains {
		counts[fd]++
	}
	minDomains := npcs + 2
	if len(counts) < minDomains {
		return fmt.Sprintf(
			"failure domains are enabled with npcs=%d, which requires at least "+
				"%d distinct failure domains (2 domains is not supported at any "+
				"npcs level); currently have %d",
			npcs, minDomains, len(counts))
	}
	first := -1
	for _, c := range counts {
		if first == -1 {
			first = c
			continue
		}
		if c != first {
			return fmt.Sprintf(
				"failure domains must hold an equal number of hosts at "+
					"activation; current split: %v", counts)
		}
	}
	return ""
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

			found := false
			for i, c := range creds.Clusters {
				if c.ClusterID == clusterID {
					creds.Clusters[i] = CSIClusterEntry{
						ClusterID:       clusterID,
						ClusterEndpoint: clusterEndpoint,
						ClusterSecret:   clusterSecret,
					}
					found = true
					break
				}
			}
			if !found {
				creds.Clusters = append(creds.Clusters, CSIClusterEntry{
					ClusterID:       clusterID,
					ClusterEndpoint: clusterEndpoint,
					ClusterSecret:   clusterSecret,
				})
			}

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

	clusterSecretName := fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Name)
	clusterSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterSecretName, Namespace: clusterCR.Namespace}, clusterSecret); err == nil {
		secretVal := string(clusterSecret.Data["secret"])
		if upsertErr := r.upsertCSICredentialsSecret(ctx, r.Namespace, clusterCR.Status.UUID, utils.ENDPOINT, secretVal); upsertErr != nil {
			log.Error(upsertErr, "syncStatus: failed to upsert CSI credentials secret", "name", clusterCR.Name)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	apiClient := webapi.NewClient()
	endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterCR.Status.UUID)

	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
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
	mftSync := int32(resp.MaxFaultTolerance)
	clusterCR.Status.MaxFaultTolerance = &mftSync

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
	return ptr.IntFromOrZero(t.Capacity)
}

func provisionedCapacityThreshold(t *simplyblockv1alpha1.CapacityThresholdSpec) int {
	if t == nil {
		return 0
	}
	return ptr.IntFromOrZero(t.ProvisionedCapacity)
}

func stripeDataChunks(s *simplyblockv1alpha1.StripeSpec) int {
	if s == nil {
		return 1
	}
	return ptr.IntFrom(s.DataChunks, 1)
}

func stripeParityChunks(s *simplyblockv1alpha1.StripeSpec) int {
	if s == nil {
		return 1
	}
	return ptr.IntFrom(s.ParityChunks, 1)
}
