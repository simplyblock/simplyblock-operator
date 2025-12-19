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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

// SimplyBlockPoolReconciler reconciles a SimplyBlockPool object
type SimplyBlockPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type POOLAPIResponse struct {
	UUID   string `json:"uuid"`
	Status string `json:"status"`
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockpools/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimplyBlockPool object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *SimplyBlockPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Pool CR
	poolCR := &simplyblockv1alpha1.SimplyBlockPool{}
	if err := r.Get(ctx, req.NamespacedName, poolCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// var cluster simplyblockv1alpha1.SimplyBlockStorageCluster
	// if err := r.Get(ctx, types.NamespacedName{Name: poolCR.Spec.ClusterName, Namespace: poolCR.Namespace}, &cluster); err != nil {
	// 	log.Info("Cluster not found yet — requeuing")
	// 	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	// }

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, poolCR.Namespace, poolCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	// if !poolCR.DeletionTimestamp.IsZero() {
	// 	if utils.ContainsString(poolCR.Finalizers, "simplyblock.finalizer") && poolCR.Status.UUID != "" {
	// 		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolCR.Status.UUID)
	// 		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
	// 		if err != nil || status >= 300 {
	// 			log.Error(err, "Failed to delete pool", "status", status, "response", string(body))
	// 			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	// 		}

	// 		poolCR.Finalizers = utils.RemoveString(poolCR.Finalizers, "simplyblock.pool.finalizer")
	// 		if err := r.Update(ctx, poolCR); err != nil {
	// 			log.Error(err, "Failed to remove finalizer")
	// 			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	// 		}

	// 		log.Info("Pool deleted successfully", "name", poolCR.Name)
	// 	}
	// 	return ctrl.Result{}, nil
	// }

	if !utils.ContainsString(poolCR.Finalizers, "simplyblock.pool.finalizer") {
		poolCR.Finalizers = append(poolCR.Finalizers, "simplyblock.pool.finalizer")
		if err := r.Update(ctx, poolCR); err != nil {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	pool := poolCR.DeepCopy()

	if pool.Status.UUID == "" {
		params := utils.PoolAddParams{
			Name:          poolCR.Spec.Name,
			PoolMax:       utils.IntPtrOrDefault(utils.ParseSize(poolCR.Spec.CapacityLimit, "si/iec", "", false), 0),
			VolumeMaxSize: 0,
			MaxRwMB:       utils.IntPtrOrDefault(poolCR.Spec.RWLimit, 0),
			MaxRwIOPS:     utils.IntPtrOrDefault(poolCR.Spec.QoSIOPSLimit, 0),
			MaxRMB:        utils.IntPtrOrDefault(poolCR.Spec.RLimit, 0),
			MaxWMB:        utils.IntPtrOrDefault(poolCR.Spec.WLimit, 0),
			CRName:        poolCR.Name,
			CRNameSpace:   poolCR.Namespace,
			CRPlural:      "pools",
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/", clusterUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, params)
		if err != nil || status >= 300 {
			log.Error(err, "Pool creation failed", "status", status, "response", string(body))
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		log.Info("POOL API call",
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)

		var apiResp POOLAPIResponse

		if err := json.Unmarshal(body, &apiResp); err != nil {
			log.Error(err, "Failed to parse pool creation response", "raw", string(body))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		// API returns UUID of the created pool
		poolCR.Status.UUID = apiResp.UUID
		poolCR.Status.Status = apiResp.Status

		if err := r.Status().Update(ctx, poolCR); err != nil {
			log.Error(err, "Failed to update pool status after creation")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		log.Info("Pool successfully created", "name", poolCR.Name)
		return ctrl.Result{}, nil
	}

	// // --- Handle update ---
	// updateParams := utils.PoolUpdateParams{
	// 	Name:    poolCR.Spec.Name,
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

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockPool{}).
		Named("pool").
		Complete(r)
}
