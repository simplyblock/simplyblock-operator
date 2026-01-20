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
	"fmt"
	"net/http"
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

// SimplyBlockSnapshotReplicationReconciler reconciles a SimplyBlockSnapshotReplication object
type SimplyBlockSnapshotReplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocksnapshotreplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocksnapshotreplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblocksnapshotreplications/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimplyBlockSnapshotReplication object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *SimplyBlockSnapshotReplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Pool CR
	snapRepCR := &simplyblockv1alpha1.SimplyBlockSnapshotReplication{}
	if err := r.Get(ctx, req.NamespacedName, snapRepCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, err := utils.ResolveClusterIdentifier(
		ctx,
		r.Client,
		snapRepCR.Namespace,
		snapRepCR.Spec.SourceCluster,
	)

	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", snapRepCR.Spec.SourceCluster,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snapRepCR.Namespace, snapRepCR.Spec.SourceCluster)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	/* -------------------- Deletion -------------------- */
	if updated, err := r.handleDeletion(ctx, snapRepCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	/* -------------------- Finalizer -------------------- */
	if updated, err := r.ensureFinalizer(ctx, snapRepCR); updated || err != nil {
		return ctrl.Result{}, err
	}

	apiClient := webapi.NewClient()

	snapRep := snapRepCR.DeepCopy()

	if !snapRep.Status.Configured {

		targetClusterUUID, err := utils.ResolveClusterIdentifier(
			ctx,
			r.Client,
			snapRepCR.Namespace,
			snapRepCR.Spec.TargetCluster,
		)

		if err != nil {
			log.Info("Cluster UUID not found, requeuing",
				"cluster", snapRepCR.Spec.SourceCluster,
			)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		poolUUID, err := utils.ResolvePoolIdentifier(
			ctx,
			r.Client,
			snapRepCR.Namespace,
			snapRepCR.Spec.TargetCluster,
			snapRepCR.Spec.TargetPool,
		)

		if err != nil {
			log.Info("Pool UUID not found, requeuing",
				"poolName", snapRepCR.Spec.TargetPool,
				"cluster", snapRepCR.Spec.TargetCluster,
			)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		params := utils.ReplicationAddParams{
			TargetCluster: targetClusterUUID,
			Timeout:       utils.IntPtrOrDefault(snapRepCR.Spec.Timeout, 0),
			TargetPool:    poolUUID,
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/addreplication/", clusterUUID)
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, params)
		if err != nil || status >= 300 {
			log.Error(err, "Cluster add replication failed", "status", status, "response", string(body))
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		snapRepCR.Status.Configured = true

		patch := client.MergeFrom(snapRep)

		if err := r.Status().Patch(ctx, snapRepCR, patch); err != nil {
			log.Error(err, "Failed to patch snapshot replication status after creation")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		log.Info("Snapshot Replication successfully added", "name", snapRepCR.Name)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockSnapshotReplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockSnapshotReplication{}).
		Named("simplyblocksnapshotreplication").
		Complete(r)
}

func (r *SimplyBlockSnapshotReplicationReconciler) handleDeletion(
	ctx context.Context,
	SnapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
) (bool, error) {

	if SnapRepCR.DeletionTimestamp.IsZero() {
		return false, nil
	}

	if !controllerutil.ContainsFinalizer(SnapRepCR, "simplyblock.replication.finalizer") {
		return true, nil
	}

	controllerutil.RemoveFinalizer(SnapRepCR, "simplyblock.replication.finalizer")
	return true, r.Update(ctx, SnapRepCR)
}

func (r *SimplyBlockSnapshotReplicationReconciler) ensureFinalizer(
	ctx context.Context,
	SnapRepCR *simplyblockv1alpha1.SimplyBlockSnapshotReplication,
) (bool, error) {

	if controllerutil.ContainsFinalizer(SnapRepCR, "simplyblock.replication.finalizer") {
		return false, nil
	}

	controllerutil.AddFinalizer(SnapRepCR, "simplyblock.replication.finalizer")
	return true, r.Update(ctx, SnapRepCR)
}
