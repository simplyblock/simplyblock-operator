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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
)

// StorageNodeReconciler reconciles a StorageNode object
type StorageNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=storagenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=storagenodes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StorageNode object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *StorageNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	snCR := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(ctx, req.NamespacedName, snCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var cluster simplyblockv1alpha1.SimplyBlockStorageCluster
	if err := r.Get(ctx, types.NamespacedName{Name: snCR.Spec.ClusterName, Namespace: snCR.Namespace}, &cluster); err != nil {
		log.Info("Cluster not found yet — requeuing")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if err := r.labelWorkerNodes(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	ds := utils.BuildStorageNodeDaemonSet(snCR)

	if err := controllerutil.SetControllerReference(snCR, ds, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var existing appsv1.DaemonSet
	err := r.Get(ctx, client.ObjectKey{Name: ds.Name, Namespace: ds.Namespace}, &existing)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating StorageNode DaemonSet", "Name", ds.Name)
		if err := r.Create(ctx, ds); err != nil {
			return ctrl.Result{}, err
		}
	} else if err == nil {
		ds.ResourceVersion = existing.ResourceVersion
		log.Info("Updating StorageNode DaemonSet", "Name", ds.Name)
		if err := r.Update(ctx, ds); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		return ctrl.Result{}, err
	}

	// 6. Update status safely
	snCR.Status.State = "Ready"
	if err := r.Status().Update(ctx, snCR); err != nil {
		log.Error(err, "Failed to update status")
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Named("storagenode").
		Complete(r)
}

func (r *StorageNodeReconciler) labelWorkerNodes(ctx context.Context, sn *simplyblockv1alpha1.StorageNode) error {
	for _, nodeName := range sn.Spec.WorkerNodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = map[string]string{}
		}

		key := "simplyblock.com/storage-node"
		value := sn.Name

		// Skip if already set
		if node.Labels[key] == value {
			continue
		}

		node.Labels[key] = value
		if err := r.Update(ctx, &node); err != nil {
			return err
		}
	}

	return nil
}
