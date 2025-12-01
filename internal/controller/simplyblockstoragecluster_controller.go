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

// SimplyBlockStorageClusterReconciler reconciles a SimplyBlockStorageCluster object
type SimplyBlockStorageClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	// Fetch CR
	clusterCR := &simplyblockv1alpha1.SimplyBlockStorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, clusterCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If already configured, nothing to do
	if clusterCR.Status.Configured {
		return ctrl.Result{}, nil
	}

	// Build API parameters from Spec
	params := utils.ClusterAddParams{
		Name:                   clusterCR.Spec.ClusterName,
		BlkSize:                utils.IntPtrOrDefault(clusterCR.Spec.BlkSize, 512),
		PageSizeInBlocks:       utils.IntPtrOrDefault(clusterCR.Spec.PageSizeInBlocks, 2097152),
		CapWarn:                utils.IntPtrOrZero(clusterCR.Spec.CapWarn),
		CapCrit:                utils.IntPtrOrZero(clusterCR.Spec.CapCrit),
		ProvCapWarn:            utils.IntPtrOrZero(clusterCR.Spec.ProvCapWarn),
		ProvCapCrit:            utils.IntPtrOrZero(clusterCR.Spec.ProvCapCrit),
		DistrNdcs:              utils.IntPtrOrDefault(clusterCR.Spec.DistrNdcs, 1),
		DistrNpcs:              utils.IntPtrOrDefault(clusterCR.Spec.DistrNpcs, 1),
		DistrBs:                utils.IntPtrOrDefault(clusterCR.Spec.DistrBs, 4096),
		DistrChunkBs:           utils.IntPtrOrDefault(clusterCR.Spec.DistrChunkBs, 4096),
		HAType:                 clusterCR.Spec.HAType,
		QpairCount:             utils.IntPtrOrDefault(clusterCR.Spec.QpairCount, 256),
		MaxQueueSize:           utils.IntPtrOrDefault(clusterCR.Spec.MaxQueueSize, 128),
		InflightIOThreshold:    utils.IntPtrOrDefault(clusterCR.Spec.InflightIOThreshold, 4),
		EnableNodeAffinity:     utils.BoolPtrOrFalse(clusterCR.Spec.EnableNodeAffinity),
		StrictNodeAntiAffinity: utils.BoolPtrOrFalse(clusterCR.Spec.StrictNodeAntiAffinity),
	}

	endpoint := "/api/v2/clusters/"
	apiClient := webapi.NewClient()

	body, status, err := apiClient.Do(
		ctx,
		clusterCR.Spec.ContactPoint,
		http.MethodPost,
		endpoint,
		params,
	)

	if err != nil || status >= 300 {
		log.Error(err, "Cluster add failed", "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	// Success — update status
	clusterCR.Status.Configured = true
	if err := r.Status().Update(ctx, clusterCR); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("Cluster successfully configured", "name", clusterCR.Name)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockStorageClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockStorageCluster{}).
		Named("simplyblockstoragecluster").
		Complete(r)
}
