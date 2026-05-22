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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// SingletonControlPlaneName is the fixed name of the singleton ControlPlane CR
// created by the Helm chart. The controller ignores any CR with a different name.
const SingletonControlPlaneName = "simplyblock"

// controlPlaneRequeueInterval is how often the FDB health check is repeated.
const controlPlaneRequeueInterval = 30 * time.Second

// ControlPlaneReconciler reconciles the singleton ControlPlane object.
type ControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes/status,verbs=get;update;patch

func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if req.Name != SingletonControlPlaneName {
		log.Info("ignoring non-singleton ControlPlane CR", "name", req.Name)
		return ctrl.Result{}, nil
	}

	cp := &simplyblockv1alpha1.ControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := metav1.Now()
	orig := cp.DeepCopy()

	apiClient := webapi.NewClient()
	body, status, err := apiClient.Do(ctx, "", http.MethodGet, "/api/v1/health/fdb/", nil)
	if err != nil || status >= 300 {
		msg := string(body)
		if err != nil {
			msg = err.Error()
		} else {
			msg = fmt.Sprintf("status=%d: %s", status, msg)
		}
		log.Info("control plane not ready", "reason", msg)

		cp.Status.Phase = utils.ClusterPhaseInitializing
		cp.Status.Message = msg
		cp.Status.LastChecked = &now

		if err := r.Status().Patch(ctx, cp, client.MergeFrom(orig)); err != nil {
			log.Error(err, "failed to patch ControlPlane status")
		}
		return ctrl.Result{RequeueAfter: controlPlaneRequeueInterval}, nil
	}

	cp.Status.Phase = utils.ClusterPhaseReady
	cp.Status.Message = ""
	cp.Status.LastChecked = &now

	if err := r.Status().Patch(ctx, cp, client.MergeFrom(orig)); err != nil {
		log.Error(err, "failed to patch ControlPlane status")
	}

	log.Info("control plane ready")
	return ctrl.Result{RequeueAfter: controlPlaneRequeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.ControlPlane{}).
		Named("controlplane").
		Complete(r)
}
