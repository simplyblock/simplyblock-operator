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
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

// SimplyBlockDeviceReconciler reconciles a SimplyBlockDevice object
type SimplyBlockDeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type deviceAPIResponse struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"`
	HealthCheck bool          `json:"health_check"`
	Size        int64         `json:"size"`
	IOError     bool          `json:"io_error"`
	IsPartition bool          `json:"is_partition"`
	NvmfIPs     []string      `json:"nvmf_ips"`
	NvmfNQN     string        `json:"nvmf_nqn"`
	NvmfPort    int           `json:"nvmf_port"`
	Capacity    *CapacityInfo `json:"capacity,omitempty"`
}

// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockdevices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockdevices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=simplyblock.simplyblock.io,resources=simplyblockdevices/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimplyBlockDevice object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *SimplyBlockDeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := logf.FromContext(ctx)

	devCR := &simplyblockv1alpha1.SimplyBlockDevice{}
	if err := r.Get(ctx, req.NamespacedName, devCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(
		ctx,
		r.Client,
		devCR.Namespace,
		devCR.Spec.ClusterName,
	)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	var nodeUUIDs []string

	if devCR.Spec.NodeUUID != "" {
		nodeUUIDs = []string{devCR.Spec.NodeUUID}
	} else {
		// Multi-node mode: fetch all storage nodes in the cluster
		nodesEndpoint := fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/",
			clusterUUID,
		)

		apiClient := webapi.NewClient()
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, nodesEndpoint, nil)
		if err != nil || status >= 300 {
			log.Error(err, "Failed to fetch storage nodes",
				"endpoint", nodesEndpoint,
				"status", status,
				"response", string(body),
			)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		var nodes []SNODEAPIResponse
		if err := json.Unmarshal(body, &nodes); err != nil {
			log.Error(err, "Failed to unmarshal storage node list")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		if len(nodes) == 0 {
			log.Info("No storage nodes found in cluster yet")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		for _, n := range nodes {
			nodeUUIDs = append(nodeUUIDs, n.UUID)
		}
	}

	apiClient := webapi.NewClient()
	nodeDeviceMap := make(map[string][]simplyblockv1alpha1.DeviceInfo)

	for _, nodeUUID := range nodeUUIDs {
		endpoint := fmt.Sprintf(
			"/api/v2/clusters/%s/storage-nodes/%s/devices/",
			clusterUUID,
			nodeUUID,
		)

		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
		if err != nil || status >= 300 {
			log.Error(err, "Failed to fetch devices",
				"nodeUUID", nodeUUID,
				"endpoint", endpoint,
				"status", status,
				"response", string(body),
			)
			continue
		}

		log.Info("DEVICE API call",
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)

		var apiDevices []deviceAPIResponse
		if err := json.Unmarshal(body, &apiDevices); err != nil {
			log.Error(err, "Failed to unmarshal device list", "nodeUUID", nodeUUID)
			continue
		}

		devices := r.mapDevices(apiDevices)
		nodeDeviceMap[nodeUUID] = devices
	}

	if len(nodeDeviceMap) == 0 {
		log.Info("No devices found on any storage node yet, requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	newStatus := make([]simplyblockv1alpha1.NodeDevices, 0, len(nodeDeviceMap))

	for nodeUUID, devices := range nodeDeviceMap {
		sort.Slice(devices, func(i, j int) bool {
			return devices[i].UUID < devices[j].UUID
		})

		newStatus = append(newStatus, simplyblockv1alpha1.NodeDevices{
			NodeUUID: nodeUUID,
			Devices:  devices,
		})
	}

	sort.Slice(newStatus, func(i, j int) bool {
		return newStatus[i].NodeUUID < newStatus[j].NodeUUID
	})

	if reflect.DeepEqual(devCR.Status.Nodes, newStatus) {
		log.V(1).Info("Device status unchanged")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	devCR.Status.Nodes = newStatus
	if err := r.Status().Update(ctx, devCR); err != nil {
		log.Error(err, "Failed to update device status")
		return ctrl.Result{}, err
	}

	log.Info("Device status updated",
		"nodeUUID", devCR.Spec.NodeUUID,
		"deviceCount", len(newStatus),
	)

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SimplyBlockDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.SimplyBlockDevice{}).
		Named("simplyblockdevice").
		Complete(r)
}

func (r *SimplyBlockDeviceReconciler) mapDevices(
	apiDevs []deviceAPIResponse,
) []simplyblockv1alpha1.DeviceInfo {

	out := make([]simplyblockv1alpha1.DeviceInfo, 0, len(apiDevs))
	for _, d := range apiDevs {
		var capacity *simplyblockv1alpha1.CapacityInfo
		if d.Capacity != nil {
			capacity = &simplyblockv1alpha1.CapacityInfo{
				SizeTotal: utils.HumanBytes(d.Capacity.SizeTotal, "iec"),
				SizeProv:  utils.HumanBytes(d.Capacity.SizeProv, "iec"),
				SizeUsed:  utils.HumanBytes(d.Capacity.SizeUsed, "iec"),
				SizeFree:  utils.HumanBytes(d.Capacity.SizeFree, "iec"),
				SizeUtil:  fmt.Sprintf("%.1f%%", d.Capacity.SizeUtil),
			}
		}
		out = append(out, simplyblockv1alpha1.DeviceInfo{
			UUID:     d.ID,
			Status:   d.Status,
			Size:     d.Size,
			Health:   strconv.FormatBool(d.HealthCheck),
			Model:    "nvme",
			Capacity: capacity,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].UUID < out[j].UUID
	})

	return out
}
