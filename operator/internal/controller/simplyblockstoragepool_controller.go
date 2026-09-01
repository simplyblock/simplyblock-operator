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
	"time"

	"github.com/simplyblock/atlas/ptr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	poolStatusInvalidClusterReference = "InvalidClusterReference"
	poolEventInvalidClusterReference  = "InvalidClusterReference"

	dhchapNodeLabelParam = "dhchap_node_label" // read by paramDHCHAPNodeLabel in csi-driver/pkg/spdk/controllerserver.go
)

// StoragePoolReconciler reconciles a StoragePool object
type StoragePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// VolumeScopes, if set, receives this pool's (cluster, pool) scope so the
	// control-plane SSE manager streams its volumes into the cache the aggregated
	// metrics API reads. Optional (nil in tests).
	VolumeScopes *cpinformer.ScopeSet
}

// StoragePoolDTO mirrors the new API's storage pool response format.
type StoragePoolDTO struct {
	ID           string   `json:"id"`
	ClusterID    string   `json:"cluster_id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	MaxRwIOPS    int64    `json:"max_rw_iops"`
	MaxRwMbytes  int64    `json:"max_rw_mbytes"`
	MaxRMbytes   int64    `json:"max_r_mbytes"`
	MaxWMbytes   int64    `json:"max_w_mbytes"`
	DHCHAP       bool     `json:"dhchap"`
	AllowedHosts []string `json:"allowed_hosts"`
}

// legacyPoolAPIResponse is the pre-DTO pool response format.
// FIXME: Remove thisonce all deployments have migrated to the new API that returns StoragePoolDTO.
type legacyPoolAPIResponse struct {
	UUID         string   `json:"uuid"`
	QoSIOPSLimit int64    `json:"max_rw_ios_per_sec"`
	RWLimit      int64    `json:"max_rw_mbytes_per_sec"`
	RLimit       int64    `json:"max_r_mbytes_per_sec"`
	WLimit       int64    `json:"max_w_mbytes_per_sec"`
	QoSHost      string   `json:"qos_host,omitempty"`
	Status       string   `json:"status"`
	DHCHAP       bool     `json:"dhchap,omitempty"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

// toDTO converts the legacy response to the canonical StoragePoolDTO.
// Fields absent from the DTO format (e.g., qos_host) are not carried over.
func (r *legacyPoolAPIResponse) toDTO() StoragePoolDTO {
	return StoragePoolDTO{
		ID:           r.UUID,
		Status:       r.Status,
		MaxRwIOPS:    r.QoSIOPSLimit,
		MaxRwMbytes:  r.RWLimit,
		MaxRMbytes:   r.RLimit,
		MaxWMbytes:   r.WLimit,
		DHCHAP:       r.DHCHAP,
		AllowedHosts: r.AllowedHosts,
	}
}

// parsePoolAPIResponse parses raw JSON into a StoragePoolDTO. It tries the DTO format first
// (detected by the `id` field), then falls back to the legacy format (detected by the
// `uuid` field). Returns an error if neither format is recognized.
func parsePoolAPIResponse(data []byte) (StoragePoolDTO, error) {
	var probe struct {
		ID   string `json:"id"`
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return StoragePoolDTO{}, fmt.Errorf("failed to parse pool API response: %w", err)
	}
	if probe.ID != "" {
		var dto StoragePoolDTO
		if err := json.Unmarshal(data, &dto); err != nil {
			return StoragePoolDTO{}, fmt.Errorf("failed to parse StoragePoolDTO: %w", err)
		}
		return dto, nil
	}
	if probe.UUID != "" {
		var legacy legacyPoolAPIResponse
		if err := json.Unmarshal(data, &legacy); err != nil {
			return StoragePoolDTO{}, fmt.Errorf("failed to parse legacy pool response: %w", err)
		}
		return legacy.toDTO(), nil
	}
	return StoragePoolDTO{}, fmt.Errorf("pool API response contains neither 'id' (DTO) nor 'uuid' (legacy): %s", string(data))
}

type poolHostParams struct {
	HostNQN string `json:"host_nqn"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StoragePool object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *StoragePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the StoragePool CR
	storagePoolCR := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(ctx, req.NamespacedName, storagePoolCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		storagePoolCR.Namespace,
		storagePoolCR.Spec.ClusterName,
	)

	switch {
	case errors.Is(err, utils.ErrClusterNotFound):
		log.Error(err, "StoragePool references a StorageCluster that does not exist in its namespace",
			"namespace", storagePoolCR.Namespace,
			"cluster", storagePoolCR.Spec.ClusterName,
		)
		if r.Recorder != nil {
			r.Recorder.Eventf(storagePoolCR, nil, corev1.EventTypeWarning, poolEventInvalidClusterReference, poolEventInvalidClusterReference,
				"StorageCluster %q not found in namespace %q; StoragePools must reside in the same namespace as their StorageCluster",
				storagePoolCR.Spec.ClusterName, storagePoolCR.Namespace)
		}
		if storagePoolCR.Status.Status != poolStatusInvalidClusterReference {
			storagePoolCR.Status.Status = poolStatusInvalidClusterReference
			if statusErr := r.Status().Update(ctx, storagePoolCR); statusErr != nil {
				log.Error(statusErr, "Failed to update pool status to InvalidClusterReference")
			}
		}
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	case err != nil:
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", storagePoolCR.Spec.ClusterName,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	if !storagePoolCR.DeletionTimestamp.IsZero() {
		return r.handleStoragePoolDeletion(ctx, storagePoolCR, apiClient, clusterUUID)
	}

	if !controllerutil.ContainsFinalizer(storagePoolCR, utils.FinalizerStoragePool) {
		controllerutil.AddFinalizer(storagePoolCR, utils.FinalizerStoragePool)
		if err := r.Update(ctx, storagePoolCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if storagePoolCR.Status.UUID == "" {
		return r.handleStoragePoolCreation(ctx, storagePoolCR, apiClient, clusterUUID)
	}

	// Pool is ready: register its scope so the SSE manager streams its volumes.
	if r.VolumeScopes != nil {
		r.VolumeScopes.Add(cpinformer.Scope{clusterUUID, storagePoolCR.Status.UUID})
	}

	if err := r.createStorageClassIfNotExists(ctx, storagePoolCR, clusterUUID); err != nil {
		log.Error(err, "Failed to create StorageClass for pool")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.syncNodeLabels(ctx, storagePoolCR); err != nil {
		log.Error(err, "Failed to sync node labels for pool")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if changed, err := r.syncStoragePoolHosts(ctx, apiClient, clusterUUID, storagePoolCR); err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	} else if changed {
		storagePoolCR.Status.AllowedNodes = storagePoolCR.Spec.AllowedNodes
		if err := r.Status().Update(ctx, storagePoolCR); err != nil {
			log.Error(err, "Failed to update pool status after host sync")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// // --- Handle update ---
	// updateParams := utils.PoolUpdateParams{
	// 	Name:    storagePoolCR.Name,
	// 	PoolMax: utils.IntPtrOrDefault(storagePoolCR.Spec.RWLimit, 0),
	// 	// VolumeMaxSize: storagePoolCR.Spec.CapacityLimitIntPtr(),
	// 	MaxRwIOPS: utils.IntPtrOrDefault(storagePoolCR.Spec.QoSIOPSLimit, 0),
	// 	MaxRwMB:   utils.IntPtrOrDefault(storagePoolCR.Spec.RWLimit, 0),
	// 	MaxRMB:    utils.IntPtrOrDefault(storagePoolCR.Spec.RLimit, 0),
	// 	MaxWMB:    utils.IntPtrOrDefault(storagePoolCR.Spec.WLimit, 0),
	// }

	// endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, storagePoolCR.Status.UUID)
	// body, status, err := apiClient.Do(ctx, http.MethodPut, endpoint, updateParams)
	// if err != nil || status >= 300 {
	// 	log.Error(err, "StoragePool update failed", "status", status, "response", string(body))
	// 	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	// }

	// log.Info("StoragePool updated successfully", "name", storagePoolCR.Name)
	return ctrl.Result{}, nil
}

func (r *StoragePoolReconciler) handleStoragePoolDeletion(ctx context.Context, storagePoolCR *simplyblockv1alpha1.StoragePool, apiClient *webapi.Client, clusterUUID string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if r.VolumeScopes != nil && storagePoolCR.Status.UUID != "" {
		r.VolumeScopes.Remove(cpinformer.Scope{clusterUUID, storagePoolCR.Status.UUID})
	}
	if utils.ContainsString(storagePoolCR.Finalizers, utils.FinalizerStoragePool) {
		if storagePoolCR.Status.UUID != "" {
			// Backend pool exists — delete it first.
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, storagePoolCR.Status.UUID)
			body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, nil)
			if err != nil || status >= 300 {
				if err == nil {
					err = fmt.Errorf("unexpected status %d", status)
				}
				log.Error(err, "Failed to delete pool", "status", status, "response", string(body))
				return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			}
			if err := r.deleteStorageClass(ctx, storagePoolCR); err != nil {
				log.Error(err, "Failed to delete StorageClass for pool")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			storagePoolCR.Spec.AllowedNodes = nil
			if err := r.syncNodeLabels(ctx, storagePoolCR); err != nil {
				log.Error(err, "Failed to clear node labels on pool deletion")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			log.Info("StoragePool deleted successfully", "name", storagePoolCR.Name)
		} else {
			// No backend pool was ever created (creation API never completed or
			// status patch failed). Nothing to clean up on the backend — remove
			// the finalizer immediately so the object is not stuck in Terminating.
			log.Info("StoragePool has no backend UUID; removing finalizer without backend call", "name", storagePoolCR.Name)
		}
		storagePoolCR.Finalizers = utils.RemoveString(storagePoolCR.Finalizers, utils.FinalizerStoragePool)
		if err := r.Update(ctx, storagePoolCR); err != nil {
			log.Error(err, "Failed to remove finalizer")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	return ctrl.Result{}, nil
}

func (r *StoragePoolReconciler) handleStoragePoolCreation(ctx context.Context, storagePoolCR *simplyblockv1alpha1.StoragePool, apiClient *webapi.Client, clusterUUID string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if existing, lookupErr := utils.GetPoolByName(ctx, apiClient, clusterUUID, storagePoolCR.Name); lookupErr == nil && existing != nil {
		log.Info("StoragePool already exists on backend, adopting", "name", storagePoolCR.Name, "uuid", existing.UUID)
		return r.adoptExistingStoragePool(ctx, storagePoolCR, existing)
	}
	params := utils.PoolAddParams{
		Name:          storagePoolCR.Name,
		PoolMax:       ptr.From(utils.ParseSize(storagePoolCR.Spec.CapacityLimit, "si/iec", "", false), 0),
		VolumeMaxSize: ptr.From(utils.ParseSize(storagePoolCR.Spec.LogicalVolumeMaxSize, "si/iec", "", false), 0),
		MaxRwMB:       poolSpecQoSThroughputReadWrite(storagePoolCR.Spec.QosSpec),
		MaxRwIOPS:     poolSpecQoSIOPS(storagePoolCR.Spec.QosSpec),
		MaxRMB:        poolSpecQoSThroughputRead(storagePoolCR.Spec.QosSpec),
		MaxWMB:        poolSpecQoSThroughputWrite(storagePoolCR.Spec.QosSpec),
		DHCHAP:        storagePoolCR.Spec.DHCHAP,
		CRName:        storagePoolCR.Name,
		CRNameSpace:   storagePoolCR.Namespace,
		CRPlural:      "storagepools",
	}
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "StoragePool creation failed", "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}
	log.Info("POOL API call", "endpoint", endpoint, "status", status, "response", string(body))
	poolDTO, err := parsePoolAPIResponse(body)
	if err != nil {
		log.Error(err, "Failed to parse pool creation response", "raw", string(body))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	storagePoolCR.Status.UUID = poolDTO.ID
	storagePoolCR.Status.Status = poolDTO.Status
	storagePoolCR.Status.QoS = &simplyblockv1alpha1.StoragePoolQoSStatus{
		IOPS: ptr.To(int32(poolDTO.MaxRwIOPS)),
		Throughput: &simplyblockv1alpha1.StoragePoolQoSThroughputStatus{
			Read:      ptr.To(int32(poolDTO.MaxRMbytes)),
			ReadWrite: ptr.To(int32(poolDTO.MaxRwMbytes)),
			Write:     ptr.To(int32(poolDTO.MaxWMbytes)),
		},
	}
	if err := r.Status().Update(ctx, storagePoolCR); err != nil {
		log.Error(err, "Failed to update pool status after creation")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	log.Info("StoragePool successfully created", "name", storagePoolCR.Name)
	return ctrl.Result{}, nil
}

// deleteStorageClass deletes the StorageClass associated with the pool, ignoring not-found errors.
func (r *StoragePoolReconciler) deleteStorageClass(ctx context.Context, storagePoolCR *simplyblockv1alpha1.StoragePool) error {
	sc := &storagev1.StorageClass{}
	name := simplyblockStorageClassName(storagePoolCR.Namespace, storagePoolCR.Spec.ClusterName, storagePoolCR.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: name}, sc); err != nil {
		return client.IgnoreNotFound(err)
	}
	return client.IgnoreNotFound(r.Delete(ctx, sc))
}

// createStorageClassIfNotExists creates the StorageClass for the pool the first time it's
// needed. It is intentionally create-only, not create-or-update: StorageClass Parameters and
// AllowedTopologies are immutable in the Kubernetes API itself, so there is no "update" to
// perform here. StoragePool.Spec.StorageClassParameters (and DHCHAP, which controls
// AllowedTopologies) are marked +k8s:immutable for the same reason — the API server rejects
// edits to them once set, so this function never needs to reconcile drift.
func (r *StoragePoolReconciler) createStorageClassIfNotExists(ctx context.Context, storagePoolCR *simplyblockv1alpha1.StoragePool, clusterUUID string) error {
	bindingMode := storagev1.VolumeBindingWaitForFirstConsumer
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	allowExpansion := true

	params := map[string]string{
		"cluster_id": clusterUUID,
		"pool_name":  storagePoolCR.Name,
	}
	mergeStorageClassParameters(params, storagePoolCR.Spec.StorageClassParameters)

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: simplyblockStorageClassName(storagePoolCR.Namespace, storagePoolCR.Spec.ClusterName, storagePoolCR.Name),
			Labels: map[string]string{
				"storage.simplyblock.io/namespace": storagePoolCR.Namespace,
				"storage.simplyblock.io/cluster":   storagePoolCR.Spec.ClusterName,
				"storage.simplyblock.io/pool":      storagePoolCR.Name,
			},
		},
		Provisioner:          utils.CSIProvisioner,
		Parameters:           params,
		VolumeBindingMode:    &bindingMode,
		ReclaimPolicy:        &reclaimPolicy,
		AllowVolumeExpansion: &allowExpansion,
	}

	if storagePoolCR.Spec.DHCHAP && len(storagePoolCR.Spec.AllowedNodes) > 0 {
		nodeLabelKey := poolNodeLabelKey(storagePoolCR.Namespace, storagePoolCR.Spec.ClusterName, storagePoolCR.Name)
		sc.AllowedTopologies = []corev1.TopologySelectorTerm{
			{
				MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{
					{
						Key:    nodeLabelKey,
						Values: []string{"allowed"},
					},
				},
			},
		}
		params[dhchapNodeLabelParam] = nodeLabelKey // CreateVolume can't derive this key on its own (#403)
	}

	if err := r.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// mergeStorageClassParameters writes StorageClassParameters fields into dst using the CSI
// driver's well-known parameter names (csi.storage.k8s.io/fstype) and the CSI driver's own
// snake_case names for the rest. Defaults are declared on the struct via +kubebuilder:default
// markers and are applied by the API server before the CR is stored, so p fields always carry
// their intended values here.
func mergeStorageClassParameters(dst map[string]string, p *simplyblockv1alpha1.StorageClassParameters) {
	if p == nil {
		return
	}
	boolStr := func(b *bool) string {
		if b != nil && *b {
			return "True"
		}
		return "False"
	}
	dst["qos_rw_iops"] = p.QosRwIops
	dst["qos_rw_mbytes"] = p.QosRwMbytes
	dst["qos_r_mbytes"] = p.QosRMbytes
	dst["qos_w_mbytes"] = p.QosWMbytes
	dst["compression"] = p.Compression
	dst["encryption"] = boolStr(p.Encryption)
	dst["replicate"] = boolStr(p.Replicate)
	dst["lvol_priority_class"] = p.LvolPriorityClass
	dst["fabric"] = p.Fabric
	dst["max_namespace_per_subsys"] = p.MaxNamespacePerSubsys
	dst["tune2fs_reserved_blocks"] = p.Tune2fsReservedBlocks
	dst["csi.storage.k8s.io/fstype"] = p.Filesystem
}

// syncStoragePoolHosts reconciles the pool's allowed hosts: fetches the current host list from the
// backend, adds hosts in spec but not on the backend, and removes hosts on the backend but
// no longer in spec. Returns true if any change was made.
func poolNodeLabelKey(namespace, clusterName, poolName string) string {
	return fmt.Sprintf("simplyblock.io/pool.%s.%s.%s", namespace, clusterName, poolName)
}

// syncNodeLabels ensures the label simplyblock.io/pool.<name>=allowed is present on every
// node in spec.allowedNodes and absent from nodes no longer in the list.
func (r *StoragePoolReconciler) syncNodeLabels(ctx context.Context, storagePoolCR *simplyblockv1alpha1.StoragePool) error {
	log := logf.FromContext(ctx)
	labelKey := poolNodeLabelKey(storagePoolCR.Namespace, storagePoolCR.Spec.ClusterName, storagePoolCR.Name)

	// Find all nodes currently carrying this pool's label.
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels{labelKey: "allowed"}); err != nil {
		return fmt.Errorf("failed to list labeled nodes: %w", err)
	}

	desiredSet := make(map[string]struct{}, len(storagePoolCR.Spec.AllowedNodes))
	for _, n := range storagePoolCR.Spec.AllowedNodes {
		desiredSet[n] = struct{}{}
	}

	// Remove label from nodes no longer desired.
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if _, ok := desiredSet[node.Name]; ok {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		delete(node.Labels, labelKey)
		if err := r.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("failed to remove label from node %s: %w", node.Name, err)
		}
		log.Info("Removed pool label from node", "node", node.Name, "label", labelKey)
	}

	// Add label to newly desired nodes.
	currentSet := make(map[string]struct{}, len(nodeList.Items))
	for _, node := range nodeList.Items {
		currentSet[node.Name] = struct{}{}
	}
	for _, nodeName := range storagePoolCR.Spec.AllowedNodes {
		if _, ok := currentSet[nodeName]; !ok {
			var node corev1.Node
			if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
				return fmt.Errorf("failed to get node %s: %w", nodeName, err)
			}
			patch := client.MergeFrom(node.DeepCopy())
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[labelKey] = "allowed"
			if err := r.Patch(ctx, &node, patch); err != nil {
				return fmt.Errorf("failed to label node %s: %w", nodeName, err)
			}
			log.Info("Added pool label to node", "node", nodeName, "label", labelKey)
		}
	}

	return nil
}

func (r *StoragePoolReconciler) syncStoragePoolHosts(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID string,
	storagePoolCR *simplyblockv1alpha1.StoragePool,
) (bool, error) {
	log := logf.FromContext(ctx)
	desired := make([]string, 0, len(storagePoolCR.Spec.AllowedNodes))

	for _, nodeName := range storagePoolCR.Spec.AllowedNodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}
		desired = append(desired, fmt.Sprintf("nqn.2014-08.io.simplyblock:uuid:%s", node.UID))
	}

	// Fetch current backend state to use as applied list.
	getEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, storagePoolCR.Status.UUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, getEndpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d: %s", status, string(body))
		}
		log.Error(err, "Failed to fetch pool for host sync")
		return false, err
	}
	poolDTO, err := parsePoolAPIResponse(body)
	if err != nil {
		log.Error(err, "Failed to parse pool GET response")
		return false, err
	}
	applied := poolDTO.AllowedHosts

	if len(desired) == 0 && len(applied) == 0 {
		return false, nil
	}

	desiredSet := make(map[string]struct{}, len(desired))
	for _, h := range desired {
		desiredSet[h] = struct{}{}
	}
	appliedSet := make(map[string]struct{}, len(applied))
	for _, h := range applied {
		appliedSet[h] = struct{}{}
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/host", clusterUUID, storagePoolCR.Status.UUID)
	changed := false

	for _, h := range desired {
		if _, ok := appliedSet[h]; ok {
			continue
		}
		body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, poolHostParams{HostNQN: h})
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d: %s", status, string(body))
			}
			log.Error(err, "Failed to add host to pool", "host", h)
			return changed, err
		}
		log.Info("Added host to pool", "host", h)
		changed = true
	}

	for _, h := range applied {
		if _, ok := desiredSet[h]; ok {
			continue
		}
		body, status, err := apiClient.Do(ctx, http.MethodDelete, endpoint, poolHostParams{HostNQN: h})
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d: %s", status, string(body))
			}
			log.Error(err, "Failed to remove host from pool", "host", h)
			return changed, err
		}
		log.Info("Removed host from pool", "host", h)
		changed = true
	}

	return changed, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StoragePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StoragePool{}).
		Named("storagepool").
		Complete(r)
}

func poolSpecQoSIOPS(q *simplyblockv1alpha1.StoragePoolQoSSpec) int {
	if q == nil {
		return 0
	}
	return ptr.IntFrom(q.IOPS, 0)
}

func poolSpecQoSThroughputRead(q *simplyblockv1alpha1.StoragePoolQoSSpec) int {
	if q == nil || q.Throughput == nil {
		return 0
	}
	return ptr.IntFrom(q.Throughput.Read, 0)
}

func poolSpecQoSThroughputReadWrite(q *simplyblockv1alpha1.StoragePoolQoSSpec) int {
	if q == nil || q.Throughput == nil {
		return 0
	}
	return ptr.IntFrom(q.Throughput.ReadWrite, 0)
}

func poolSpecQoSThroughputWrite(q *simplyblockv1alpha1.StoragePoolQoSSpec) int {
	if q == nil || q.Throughput == nil {
		return 0
	}
	return ptr.IntFrom(q.Throughput.Write, 0)
}

func (r *StoragePoolReconciler) adoptExistingStoragePool(
	ctx context.Context,
	storagePoolCR *simplyblockv1alpha1.StoragePool,
	existing *utils.PoolListEntry,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	orig := storagePoolCR.DeepCopy()
	storagePoolCR.Status.UUID = existing.UUID
	storagePoolCR.Status.Status = existing.Status
	storagePoolCR.Status.QoS = &simplyblockv1alpha1.StoragePoolQoSStatus{
		Host: existing.QoSHost,
		IOPS: ptr.To(int32(existing.MaxRwIOPS)),
		Throughput: &simplyblockv1alpha1.StoragePoolQoSThroughputStatus{
			Read:      ptr.To(int32(existing.RLimit)),
			ReadWrite: ptr.To(int32(existing.RWLimit)),
			Write:     ptr.To(int32(existing.WLimit)),
		},
	}
	if err := r.Status().Patch(ctx, storagePoolCR, client.MergeFrom(orig)); err != nil {
		log.Error(err, "Failed to patch pool status after adoption")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	log.Info("StoragePool adopted from existing backend", "name", storagePoolCR.Name, "uuid", existing.UUID)
	return ctrl.Result{}, nil
}
