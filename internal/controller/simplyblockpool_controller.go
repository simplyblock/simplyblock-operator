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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// PoolReconciler reconciles a Pool object
type PoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
// Fields absent from the DTO format (e.g. qos_host) are not carried over.
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
// (detected by the "id" field), then falls back to the legacy format (detected by the "uuid"
// field). Returns an error if neither format is recognised.
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

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=pools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=pools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=pools/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Pool object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Pool CR
	poolCR := &simplyblockv1alpha1.Pool{}
	if err := r.Get(ctx, req.NamespacedName, poolCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		poolCR.Namespace,
		poolCR.Spec.ClusterName,
	)

	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", poolCR.Spec.ClusterName,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, poolCR.Namespace, poolCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	if !poolCR.DeletionTimestamp.IsZero() {
		if utils.ContainsString(poolCR.Finalizers, utils.FinalizerPool) && poolCR.Status.UUID != "" {
			endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolCR.Status.UUID)
			body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
			if err != nil || status >= 300 {
				if err == nil {
					err = fmt.Errorf("unexpected status %d", status)
				}
				log.Error(err, "Failed to delete pool", "status", status, "response", string(body))
				return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			}

			if err := r.deleteStorageClass(ctx, poolCR); err != nil {
				log.Error(err, "Failed to delete StorageClass for pool")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			// Clear pool node labels from all nodes.
			poolCR.Spec.AllowedNodes = nil
			if err := r.syncNodeLabels(ctx, poolCR); err != nil {
				log.Error(err, "Failed to clear node labels on pool deletion")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			poolCR.Finalizers = utils.RemoveString(poolCR.Finalizers, utils.FinalizerPool)
			if err := r.Update(ctx, poolCR); err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			log.Info("Pool deleted successfully", "name", poolCR.Name)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(poolCR, utils.FinalizerPool) {
		controllerutil.AddFinalizer(poolCR, utils.FinalizerPool)
		if err := r.Update(ctx, poolCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	pool := poolCR.DeepCopy()

	if pool.Status.UUID == "" {
		params := utils.PoolAddParams{
			Name:          poolCR.Name,
			PoolMax:       utils.Int64PtrOrDefault(utils.ParseSizeInt64(poolCR.Spec.CapacityLimit, "si/iec", "", false), 0),
			VolumeMaxSize: utils.Int64PtrOrDefault(utils.ParseSizeInt64(poolCR.Spec.LogicalVolumeMaxSize, "si/iec", "", false), 0),
			MaxRwMB:       poolSpecQoSThroughputReadWrite(poolCR.Spec.QosSpec),
			MaxRwIOPS:     poolSpecQoSIOPS(poolCR.Spec.QosSpec),
			MaxRMB:        poolSpecQoSThroughputRead(poolCR.Spec.QosSpec),
			MaxWMB:        poolSpecQoSThroughputWrite(poolCR.Spec.QosSpec),
			DHCHAP:        poolCR.Spec.DHCHAP,
			CRName:        poolCR.Name,
			CRNameSpace:   poolCR.Namespace,
			CRPlural:      "pools",
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, params)
		if err != nil || status >= 300 {
			if err == nil {
				err = fmt.Errorf("unexpected status %d", status)
			}
			log.Error(err, "Pool creation failed", "status", status, "response", string(body))
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		log.Info("POOL API call",
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)

		poolDTO, err := parsePoolAPIResponse(body)
		if err != nil {
			log.Error(err, "Failed to parse pool creation response", "raw", string(body))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		poolCR.Status.UUID = poolDTO.ID
		poolCR.Status.Status = poolDTO.Status
		poolCR.Status.QoS = &simplyblockv1alpha1.PoolQoSStatus{
			IOPS: utils.ToInt32Ptr(poolDTO.MaxRwIOPS),
			Throughput: &simplyblockv1alpha1.PoolQoSThroughputStatus{
				Read:      utils.ToInt32Ptr(poolDTO.MaxRMbytes),
				ReadWrite: utils.ToInt32Ptr(poolDTO.MaxRwMbytes),
				Write:     utils.ToInt32Ptr(poolDTO.MaxWMbytes),
			},
		}

		if err := r.Status().Update(ctx, poolCR); err != nil {
			log.Error(err, "Failed to update pool status after creation")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		log.Info("Pool successfully created", "name", poolCR.Name)
		return ctrl.Result{}, nil
	}

	if err := r.upsertStorageClass(ctx, poolCR, clusterUUID); err != nil {
		log.Error(err, "Failed to create StorageClass for pool")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.syncNodeLabels(ctx, poolCR); err != nil {
		log.Error(err, "Failed to sync node labels for pool")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if changed, err := r.syncPoolHosts(ctx, apiClient, clusterSecret, clusterUUID, poolCR); err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	} else if changed {
		poolCR.Status.AllowedNodes = poolCR.Spec.AllowedNodes
		if err := r.Status().Update(ctx, poolCR); err != nil {
			log.Error(err, "Failed to update pool status after host sync")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// // --- Handle update ---
	// updateParams := utils.PoolUpdateParams{
	// 	Name:    poolCR.Name,
	// 	PoolMax: utils.IntPtrOrDefault(poolCR.Spec.RWLimit, 0),
	// 	// VolumeMaxSize: poolCR.Spec.CapacityLimitIntPtr(),
	// 	MaxRwIOPS: utils.IntPtrOrDefault(poolCR.Spec.QoSIOPSLimit, 0),
	// 	MaxRwMB:   utils.IntPtrOrDefault(poolCR.Spec.RWLimit, 0),
	// 	MaxRMB:    utils.IntPtrOrDefault(poolCR.Spec.RLimit, 0),
	// 	MaxWMB:    utils.IntPtrOrDefault(poolCR.Spec.WLimit, 0),
	// }

	// endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolCR.Status.UUID)
	// body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPut, endpoint, updateParams)
	// if err != nil || status >= 300 {
	// 	log.Error(err, "Pool update failed", "status", status, "response", string(body))
	// 	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	// }

	// log.Info("Pool updated successfully", "name", poolCR.Name)
	return ctrl.Result{}, nil
}

// deleteStorageClass deletes the StorageClass associated with the pool, ignoring not-found errors.
func (r *PoolReconciler) deleteStorageClass(ctx context.Context, poolCR *simplyblockv1alpha1.Pool) error {
	sc := &storagev1.StorageClass{}
	name := simplyblockStorageClassName(poolCR.Namespace, poolCR.Spec.ClusterName, poolCR.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: name}, sc); err != nil {
		return client.IgnoreNotFound(err)
	}
	return client.IgnoreNotFound(r.Delete(ctx, sc))
}

// upsertStorageClass creates a StorageClass for the pool if one does not already exist.
// StorageClass parameters are immutable in Kubernetes, so this is create-once: if the
// StorageClass already exists it is left unchanged.
func (r *PoolReconciler) upsertStorageClass(ctx context.Context, poolCR *simplyblockv1alpha1.Pool, clusterUUID string) error {
	bindingMode := storagev1.VolumeBindingWaitForFirstConsumer
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	allowExpansion := true

	params := map[string]string{
		"cluster_id":                clusterUUID,
		"pool_name":                 poolCR.Name,
		"csi.storage.k8s.io/fstype": "ext4",
	}
	mergeStorageClassParameters(params, poolCR.Spec.StorageClassParameters)

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: simplyblockStorageClassName(poolCR.Namespace, poolCR.Spec.ClusterName, poolCR.Name),
			Labels: map[string]string{
				"storage.simplyblock.io/namespace": poolCR.Namespace,
				"storage.simplyblock.io/cluster":   poolCR.Spec.ClusterName,
				"storage.simplyblock.io/pool":      poolCR.Name,
			},
		},
		Provisioner:          utils.CSIProvisioner,
		Parameters:           params,
		VolumeBindingMode:    &bindingMode,
		ReclaimPolicy:        &reclaimPolicy,
		AllowVolumeExpansion: &allowExpansion,
	}

	if poolCR.Spec.DHCHAP && len(poolCR.Spec.AllowedNodes) > 0 {
		sc.AllowedTopologies = []corev1.TopologySelectorTerm{
			{
				MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{
					{
						Key:    poolNodeLabelKey(poolCR.Namespace, poolCR.Spec.ClusterName, poolCR.Name),
						Values: []string{"allowed"},
					},
				},
			},
		}
	}

	if err := r.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// mergeStorageClassParameters writes StorageClassParameters fields into dst using the CSI
// driver's snake_case parameter names. Defaults are declared on the struct via
// +kubebuilder:default markers and are applied by the API server before the CR is stored,
// so p fields always carry their intended values here.
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
	dst["distr_ndcs"] = p.NumDataChunks
	dst["distr_npcs"] = p.NumParityChunks
	dst["lvol_priority_class"] = p.LvolPriorityClass
	dst["fabric"] = p.Fabric
	dst["max_namespace_per_subsys"] = p.MaxNamespacePerSubsys
	dst["tune2fs_reserved_blocks"] = p.Tune2fsReservedBlocks
}

// syncPoolHosts reconciles the pool's allowed hosts: fetches the current host list from the
// backend, adds hosts in spec but not on the backend, and removes hosts on the backend but
// no longer in spec. Returns true if any change was made.
func poolNodeLabelKey(namespace, clusterName, poolName string) string {
	return fmt.Sprintf("simplyblock.io/pool.%s.%s.%s", namespace, clusterName, poolName)
}

// syncNodeLabels ensures the label simplyblock.io/pool.<name>=allowed is present on every
// node in spec.allowedNodes and absent from nodes no longer in the list.
func (r *PoolReconciler) syncNodeLabels(ctx context.Context, poolCR *simplyblockv1alpha1.Pool) error {
	log := logf.FromContext(ctx)
	labelKey := poolNodeLabelKey(poolCR.Namespace, poolCR.Spec.ClusterName, poolCR.Name)

	// Find all nodes currently carrying this pool's label.
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels{labelKey: "allowed"}); err != nil {
		return fmt.Errorf("failed to list labeled nodes: %w", err)
	}

	desiredSet := make(map[string]struct{}, len(poolCR.Spec.AllowedNodes))
	for _, n := range poolCR.Spec.AllowedNodes {
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
	for _, nodeName := range poolCR.Spec.AllowedNodes {
		if _, ok := currentSet[nodeName]; ok {
			continue
		}
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

	return nil
}

func (r *PoolReconciler) syncPoolHosts(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret, clusterUUID string,
	poolCR *simplyblockv1alpha1.Pool,
) (bool, error) {
	log := logf.FromContext(ctx)
	desired := make([]string, 0, len(poolCR.Spec.AllowedNodes))

	for _, nodeName := range poolCR.Spec.AllowedNodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}
		desired = append(desired, fmt.Sprintf("nqn.2014-08.io.simplyblock:uuid:%s", node.UID))
	}

	// Fetch current backend state to use as applied list.
	getEndpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolCR.Status.UUID)
	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, getEndpoint, nil)
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

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s/host", clusterUUID, poolCR.Status.UUID)
	changed := false

	for _, h := range desired {
		if _, ok := appliedSet[h]; ok {
			continue
		}
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, poolHostParams{HostNQN: h})
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
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, poolHostParams{HostNQN: h})
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
func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.Pool{}).
		Named("pool").
		Complete(r)
}

func poolSpecQoSIOPS(q *simplyblockv1alpha1.PoolQoSSpec) int {
	if q == nil {
		return 0
	}
	return utils.IntPtrOrDefault(q.IOPS, 0)
}

func poolSpecQoSThroughputRead(q *simplyblockv1alpha1.PoolQoSSpec) int {
	if q == nil || q.Throughput == nil {
		return 0
	}
	return utils.IntPtrOrDefault(q.Throughput.Read, 0)
}

func poolSpecQoSThroughputReadWrite(q *simplyblockv1alpha1.PoolQoSSpec) int {
	if q == nil || q.Throughput == nil {
		return 0
	}
	return utils.IntPtrOrDefault(q.Throughput.ReadWrite, 0)
}

func poolSpecQoSThroughputWrite(q *simplyblockv1alpha1.PoolQoSSpec) int {
	if q == nil || q.Throughput == nil {
		return 0
	}
	return utils.IntPtrOrDefault(q.Throughput.Write, 0)
}
