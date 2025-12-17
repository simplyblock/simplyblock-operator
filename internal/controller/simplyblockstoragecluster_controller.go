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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

// SimplyBlockStorageClusterReconciler reconciles a SimplyBlockStorageCluster object
type SimplyBlockStorageClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type ClusterAPIResponse struct {
	Results struct {
		UUID        string        `json:"uuid"`
		Secret      string        `json:"secret"`
		NQN         string        `json:"nqn"`
		NDCS        int           `json:"distr_ndcs"`
		NPCS        int           `json:"distr_npcs"`
		Rebalancing bool          `json:"is_re_balancing"`
		Capacity    *CapacityInfo `json:"capacity,omitempty"`
		Status      string        `json:"status"`
	} `json:"results"`
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockstorageclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockstorageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockstorageclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimplyBlockStorageCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *SimplyBlockStorageClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the CR
	clusterCR := &simplyblockv1alpha1.SimplyBlockStorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, clusterCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiClient := webapi.NewClient()

	// --- Handle deletion ---
	if !clusterCR.DeletionTimestamp.IsZero() {
		// CR is being deleted
		if utils.ContainsString(clusterCR.Finalizers, "simplyblock.finalizer") && clusterCR.Status.UUID != "" {
			clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Spec.ClusterName)
			if err != nil {
				log.Error(err, "Failed to get cluster auth")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			endpoint := fmt.Sprintf("/api/v2/clusters/%s", clusterUUID)
			body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
			if err != nil || status >= 300 {
				log.Error(err, "Failed to delete cluster", "status", status, "response", string(body))
				return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			}

			// Remove finalizer
			clusterCR.Finalizers = utils.RemoveString(clusterCR.Finalizers, "simplyblock.finalizer")
			if err := r.Update(ctx, clusterCR); err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			log.Info("Cluster deleted successfully", "name", clusterCR.Name)
		}
		return ctrl.Result{}, nil
	}

	// --- Add finalizer if not present ---
	if !utils.ContainsString(clusterCR.Finalizers, "simplyblock.finalizer") {
		clusterCR.Finalizers = append(clusterCR.Finalizers, "simplyblock.finalizer")
		if err := r.Update(ctx, clusterCR); err != nil {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	endpoint := "/api/v1/health/fdb/"
	body, status, err := apiClient.Do(ctx, "", http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		log.Error(err, "FDB not ready", "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// --- Handle creation ---
	cluster := clusterCR.DeepCopy()

	if cluster.Status.UUID == "" {
		params := utils.ClusterAddParams{
			Name:                   clusterCR.Spec.ClusterName,
			BlkSize:                utils.IntPtrOrDefault(clusterCR.Spec.BlkSize, 512),
			PageSizeInBlocks:       utils.IntPtrOrDefault(clusterCR.Spec.PageSizeInBlocks, 2097152),
			CapWarn:                utils.IntPtrOrZero(clusterCR.Spec.CapWarn),
			CapCrit:                utils.IntPtrOrZero(clusterCR.Spec.CapCrit),
			ProvCapWarn:            utils.IntPtrOrZero(clusterCR.Spec.ProvCapWarn),
			ProvCapCrit:            utils.IntPtrOrZero(clusterCR.Spec.ProvCapCrit),
			DistrNdcs:              utils.IntPtrOrDefault(clusterCR.Spec.StripeWdata, 1),
			DistrNpcs:              utils.IntPtrOrDefault(clusterCR.Spec.StripeWparity, 1),
			DistrBs:                utils.IntPtrOrDefault(clusterCR.Spec.DistrBs, 4096),
			DistrChunkBs:           utils.IntPtrOrDefault(clusterCR.Spec.DistrChunkBs, 4096),
			HAType:                 clusterCR.Spec.HAType,
			QpairCount:             utils.IntPtrOrDefault(clusterCR.Spec.QpairCount, 256),
			MaxQueueSize:           utils.IntPtrOrDefault(clusterCR.Spec.MaxQueueSize, 128),
			InflightIOThreshold:    utils.IntPtrOrDefault(clusterCR.Spec.InflightIOThreshold, 4),
			EnableNodeAffinity:     utils.BoolPtrOrFalse(clusterCR.Spec.EnableNodeAffinity),
			StrictNodeAntiAffinity: utils.BoolPtrOrFalse(clusterCR.Spec.StrictNodeAntiAffinity),
			IsSingleNode:           utils.BoolPtrOrFalse(clusterCR.Spec.IsSingleNode),
			Fabric:                 clusterCR.Spec.Fabric,
		}

		// endpoint := "/api/v2/clusters/"

		endpoint = "/api/v1/cluster/create_first/"

		body, status, err = apiClient.Do(ctx, "", http.MethodPost, endpoint, params)
		if err != nil || status >= 300 {
			log.Error(err, "Cluster creation failed", "status", status, "response", string(body))
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		log.Info("Cluster API call",
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)

		var apiResp ClusterAPIResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			log.Error(err, "Unable to parse cluster creation response", "raw", string(body))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		secretName := fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Spec.ClusterName)

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: clusterCR.Namespace,
			},
		}

		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data["uuid"] = []byte(apiResp.Results.UUID)
			secret.Data["secret"] = []byte(apiResp.Results.Secret)
			return nil
		})

		if err != nil {
			log.Error(err, "Failed to create/update Secret for cluster")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		clusterCR.Status.UUID = apiResp.Results.UUID
		clusterCR.Status.Rebalancing = &apiResp.Results.Rebalancing
		clusterCR.Status.Status = apiResp.Results.Status
		clusterCR.Status.NQN = apiResp.Results.NQN
		clusterCR.Status.MOD = fmt.Sprintf("%dx%d", apiResp.Results.NDCS, apiResp.Results.NPCS)
		clusterCR.Status.ClusterName = clusterCR.Spec.ClusterName
		cluster.Status.SecretName = fmt.Sprintf("simplyblock-cluster-%s", clusterCR.Spec.ClusterName)
		clusterCR.Status.Configured = true

		if apiResp.Results.Capacity != nil {
			if clusterCR.Status.Capacity == nil {
				clusterCR.Status.Capacity = &simplyblockv1alpha1.CapacityInfo{}
			}

			clusterCR.Status.Capacity.SizeTotal = apiResp.Results.Capacity.SizeTotal
			clusterCR.Status.Capacity.SizeProv = apiResp.Results.Capacity.SizeProv
			clusterCR.Status.Capacity.SizeUsed = apiResp.Results.Capacity.SizeUsed
			clusterCR.Status.Capacity.SizeFree = apiResp.Results.Capacity.SizeFree
			clusterCR.Status.Capacity.SizeUtil = apiResp.Results.Capacity.SizeUtil
		}

		if err := r.Status().Update(ctx, clusterCR); err != nil {
			log.Error(err, "Failed to update cluster status after creation")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		log.Info("Cluster successfully created", "name", clusterCR.Name)
		return ctrl.Result{}, nil
	}

	// // --- Handle update ---
	// updateParams := utils.ClusterUpdateParams{
	// 	CapWarn:                utils.IntPtrOrZero(clusterCR.Spec.CapWarn),
	// 	CapCrit:                utils.IntPtrOrZero(clusterCR.Spec.CapCrit),
	// 	ProvCapWarn:            utils.IntPtrOrZero(clusterCR.Spec.ProvCapWarn),
	// 	ProvCapCrit:            utils.IntPtrOrZero(clusterCR.Spec.ProvCapCrit),
	// 	QoSClasses:             clusterCR.Spec.QoSClasses,
	// 	LogDelInterval:         clusterCR.Spec.LogDelInterval,
	// 	MetricsRetentionPeriod: clusterCR.Spec.MetricsRetentionPeriod,
	// 	ClientQpairCount:       utils.IntPtrOrZero(clusterCR.Spec.ClientQpairCount),
	// 	IncludeStats:           utils.BoolPtrOrFalse(clusterCR.Spec.IncludeStats),
	// 	StatsHistoryInSeconds:  utils.IntPtrOrZero(clusterCR.Spec.StatsHistoryInSeconds),
	// 	IncludeEventLog:        utils.BoolPtrOrFalse(clusterCR.Spec.IncludeEventLog),
	// 	EventLogEntries:        utils.IntPtrOrZero(clusterCR.Spec.EventLogEntries),
	// }

	// clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Spec.ClusterName)
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

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockStorageClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockStorageCluster{}).
		Named("simplyblockstoragecluster").
		Complete(r)
}
