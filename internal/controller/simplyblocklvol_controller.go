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
	"reflect"
	"sort"
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

// SimplyBlockLvolReconciler reconciles a SimplyBlockLvol object
type SimplyBlockLvolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type LVOLAPIResponse struct {
	UUID           string   `json:"id"`
	LvolName       string   `json:"name"`
	NodeUUID       []string `json:"nodes,omitempty"`
	Hostname       string   `json:"hostname,omitempty"`
	ClonedFromSnap string   `json:"cloned_from,omitempty"`
	SnapName       string   `json:"snapshot_name,omitempty"`
	NQN            string   `json:"nqn,omitempty"`
	SubsysPort     int64    `json:"port,omitempty"`
	NamespaceID    int64    `json:"ns_id,omitempty"`
	BlobID         int64    `json:"blobid,omitempty"`
	PoolUUID       string   `json:"pool_uuid,omitempty"`
	PoolName       string   `json:"pool_name,omitempty"`
	PvcName        string   `json:"pvc_name,omitempty"`
	HA             bool     `json:"high_availability,omitempty"`
	Health         bool     `json:"health_check,omitempty"`
	IsCrypto       *string  `json:"crypto_key,omitempty"`
	Size           int64    `json:"size,omitempty"`
	Fabric         string   `json:"fabric,omitempty"`
	StripeWdata    int64    `json:"ndcs,omitempty"`
	StripeWparity  int64    `json:"npcs,omitempty"`
	QosIOPS        int64    `json:"max_rw_iops,omitempty"`
	QosWTP         int64    `json:"max_w_mbytes,omitempty"`
	QosRTP         int64    `json:"max_r_mbytes,omitempty"`
	QosRWTP        int64    `json:"max_rw_mbytes,omitempty"`
	QosClass       int64    `json:"lvol_priority_class,omitempty"`
	Status         string   `json:"status,omitempty"`

	MaxNamespacesPerSubsystem int64 `json:"max_namespace_per_subsys,omitempty"`
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocklvols,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocklvols/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocklvols/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimplyBlockLvol object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *SimplyBlockLvolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Pool CR
	lvolCR := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(ctx, req.NamespacedName, lvolCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		lvolCR.Namespace,
		lvolCR.Spec.ClusterName,
	)

	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", lvolCR.Spec.ClusterName,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, lvolCR.Namespace, lvolCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	poolUUID, err := utils.ResolvePoolUUID(
		ctx,
		r.Client,
		lvolCR.Namespace,
		lvolCR.Spec.ClusterName,
		lvolCR.Spec.PoolName,
	)

	if err != nil {
		log.Info("Pool UUID not ready yet, requeuing",
			"poolName", lvolCR.Spec.PoolName,
			"cluster", lvolCR.Spec.ClusterName,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !lvolCR.DeletionTimestamp.IsZero() {
		if utils.ContainsString(lvolCR.Finalizers, "simplyblock.lvol.finalizer") {
			// TODO: add any cleanup logic needed before lvol deletion

			lvolCR.Finalizers = utils.RemoveString(lvolCR.Finalizers, "simplyblock.lvol.finalizer")
			if err := r.Update(ctx, lvolCR); err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			log.Info("Lvol CR deleted successfully", "name", lvolCR.Name)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(lvolCR, "simplyblock.lvol.finalizer") {
		controllerutil.AddFinalizer(lvolCR, "simplyblock.lvol.finalizer")
		if err := r.Update(ctx, lvolCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	apiClient := webapi.NewClient()

	lvol := lvolCR.DeepCopy()

	if !lvol.Status.Configured {
		params := utils.PoolUpdateParams{
			LvolCRName:      lvolCR.Name,
			LvolCRNameSpace: lvolCR.Namespace,
			LvolCRPlural:    "simplyblocklvols",
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-pools/%s", clusterUUID, poolUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPut, endpoint, params)
		if err != nil || status >= 300 {
			log.Error(err, "Pool Update failed", "status", status, "response", string(body))
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		log.Info("POOL UPDATE API call",
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)

		lvolCR.Status.Configured = true

		if err := r.Status().Update(ctx, lvolCR); err != nil {
			log.Error(err, "Failed to update lvolCR status after creation")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		log.Info("Pool successfully updated", "lvols_cr_name", lvolCR.Name)
	}

	endpoint := fmt.Sprintf(
		"/api/v2/clusters/%s/storage-pools/%s/volumes/",
		clusterUUID,
		poolUUID,
	)

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		log.Error(err, "Failed to fetch lvols",
			"poolUUID", poolUUID,
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)
	}

	log.Info("LVOL API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	var apiLvols []LVOLAPIResponse
	if err := json.Unmarshal(body, &apiLvols); err != nil {
		log.Error(err, "Failed to unmarshal lvol list", "poolUUID", poolUUID)
	}

	desiredStatus := lvolStatusListFromAPI(apiLvols)
	desiredStatus.Configured = lvolCR.Status.Configured

	normalizeLvolStatus(&desiredStatus)
	normalizeLvolStatus(&lvolCR.Status)

	if reflect.DeepEqual(lvolCR.Status, desiredStatus) {
		log.Info("LVOL status already up to date", "lvolCR", lvolCR.Name)
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(lvolCR.DeepCopy())
	lvolCR.Status = desiredStatus

	if err := r.Status().Patch(ctx, lvolCR, patch); err != nil {
		log.Error(err, "Failed to patch LVOL status", "lvol", lvolCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("LVOL status updated",
		"lvolCR", lvolCR.Name,
		"pool", lvolCR.Spec.PoolName,
		"count", len(desiredStatus.Lvols),
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockLvolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockLvol{}).
		Named("simplyblocklvol").
		Complete(r)
}

func lvolStatusListFromAPI(api []LVOLAPIResponse) simplyblockv1alpha1.SimplyBlockLvolStatus {
	lvols := make([]simplyblockv1alpha1.LvolStatus, 0, len(api))

	for _, l := range api {
		lvols = append(lvols, simplyblockv1alpha1.LvolStatus{
			UUID:           l.UUID,
			LvolName:       l.LvolName,
			NodeUUID:       l.NodeUUID,
			Hostname:       l.Hostname,
			ClonedFromSnap: l.ClonedFromSnap,
			SnapName:       l.SnapName,
			NQN:            l.NQN,
			SubsysPort:     l.SubsysPort,
			NamespaceID:    l.NamespaceID,
			BlobID:         l.BlobID,
			PoolUUID:       l.PoolUUID,
			PoolName:       l.PoolName,
			PvcName:        l.PvcName,
			Status:         l.Status,
			HA:             l.HA,
			Health:         l.Health,
			IsCrypto:       l.IsCrypto != nil,
			Size:           utils.HumanBytes(l.Size, "iec"),
			StripeWdata:    l.StripeWdata,
			StripeWparity:  l.StripeWparity,

			QosIOPS:  l.QosIOPS,
			QosWTP:   l.QosWTP,
			QosRTP:   l.QosRTP,
			QosRWTP:  l.QosRWTP,
			QosClass: l.QosClass,

			MaxNamespacesPerSubsystem: l.MaxNamespacesPerSubsystem,
			Fabric:                    l.Fabric,
		})
	}

	return simplyblockv1alpha1.SimplyBlockLvolStatus{
		Lvols: lvols,
	}
}

func normalizeLvolStatus(s *simplyblockv1alpha1.SimplyBlockLvolStatus) {
	sort.SliceStable(s.Lvols, func(i, j int) bool {
		return s.Lvols[i].UUID < s.Lvols[j].UUID
	})

	for i := range s.Lvols {
		sort.Strings(s.Lvols[i].NodeUUID)
	}
}
