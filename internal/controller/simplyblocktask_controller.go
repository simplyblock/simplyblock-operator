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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// TaskReconciler reconciles a Task object
type TaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type ClusterTaskAPIResponse []struct {
	UUID     string `json:"id"`
	TaskType string `json:"function_name"`
	Status   string `json:"status"`
	Result   string `json:"function_result"`
	Canceled bool   `json:"canceled"`
	Retried  int    `json:"retry"`
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Task object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	taskCR := &simplyblockv1alpha1.Task{}
	if err := r.Get(ctx, req.NamespacedName, taskCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !taskCR.DeletionTimestamp.IsZero() {
		if utils.ContainsString(taskCR.Finalizers, utils.FinalizerTask) {
			// TODO: add any cleanup logic needed before task deletion

			// Remove finalizer
			taskCR.Finalizers = utils.RemoveString(taskCR.Finalizers, utils.FinalizerTask)
			if err := r.Update(ctx, taskCR); err != nil {
				log.Error(err, "Failed to remove finalizer from task", "task", taskCR.Name)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			log.Info("Task CR deleted successfully", "task", taskCR.Name)
		}
		return ctrl.Result{}, nil
	}

	// --- Add finalizer if not present ---
	if !controllerutil.ContainsFinalizer(taskCR, utils.FinalizerTask) {
		controllerutil.AddFinalizer(taskCR, utils.FinalizerTask)
		if err := r.Update(ctx, taskCR); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	clusterUUID, err := utils.ResolveClusterUUID(
		ctx,
		r.Client,
		taskCR.Namespace,
		taskCR.Spec.ClusterName,
	)

	if err != nil {
		log.Info("Cluster UUID not ready yet, requeuing",
			"cluster", taskCR.Spec.ClusterName,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, taskCR.Namespace, taskCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	var endpoint string
	if taskCR.Spec.TaskID != "" {
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/tasks/%s/",
			clusterUUID,
			taskCR.Spec.TaskID,
		)
	} else {
		endpoint = fmt.Sprintf(
			"/api/v2/clusters/%s/tasks/",
			clusterUUID,
		)
	}
	apiClient := webapi.NewClient()

	body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
	if status == http.StatusNotFound {
		taskCR.Status.Tasks = nil
		if err := r.Status().Update(ctx, taskCR); err != nil {
			log.Error(err, "Failed to clear task status", "task", taskCR.Name)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "Failed to fetch task(s)", "task", taskCR.Name, "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("ClusterTask API call",
		"endpoint", endpoint,
		"status", status,
		"response", string(body),
	)

	var apiRespTask ClusterTaskAPIResponse
	if err := json.Unmarshal(body, &apiRespTask); err != nil {
		log.Error(err, "Failed to parse task API response", "task", taskCR.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	taskCR.Status.Tasks = nil
	for _, tentry := range apiRespTask {
		result := strings.ToLower(tentry.Result)
		if strings.Contains(result, "success") || strings.Contains(result, "done") || tentry.Canceled {
			continue
		}

		taskCR.Status.Tasks = append(taskCR.Status.Tasks, simplyblockv1alpha1.TaskEntry{
			UUID:       tentry.UUID,
			TaskType:   tentry.TaskType,
			TaskStatus: tentry.Status,
			TaskResult: tentry.Result,
			Canceled:   tentry.Canceled,
			Retried:    utils.IntToInt32Ptr(tentry.Retried),
		})
	}

	if err := r.Status().Update(ctx, taskCR); err != nil {
		log.Error(err, "Failed to update task status", "task", taskCR.Name)
	}

	return ctrl.Result{RequeueAfter: 120 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.Task{}).
		Named("task").
		Complete(r)
}
