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
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	"github.com/simplyblock/simplyblock-manager/internal/utils"
	"github.com/simplyblock/simplyblock-manager/internal/webapi"
)

// StorageNodeReconciler reconciles a StorageNode object
type StorageNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type SNODEAPIResponse struct {
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	IP     string `json:"mgmt_ip"`
	Health bool   `json:"health_check"`
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
	log := logf.FromContext(ctx)

	snCR := &simplyblockv1alpha1.StorageNode{}
	if err := r.Get(ctx, req.NamespacedName, snCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// var cluster simplyblockv1alpha1.SimplyBlockStorageCluster
	// if err := r.Get(ctx, types.NamespacedName{Name: snCR.Spec.ClusterName, Namespace: snCR.Namespace}, &cluster); err != nil {
	// 	log.Info("Cluster not found yet — requeuing")
	// 	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	// }

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, snCR.Namespace, snCR.Spec.ClusterName)
	if err != nil {
		log.Error(err, "Failed to get cluster auth")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	apiClient := webapi.NewClient()

	// if !snCR.DeletionTimestamp.IsZero() {
	// 	if utils.ContainsString(snCR.Finalizers, "simplyblock.finalizer") && snCR.Status.UUID != "" {
	// 		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, snCR.Status.UUID)
	// 		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodDelete, endpoint, nil)
	// 		if err != nil || status >= 300 {
	// 			log.Error(err, "Failed to delete storage-node via API", "status", status, "response", string(body))
	// 			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	// 		}

	// 		snCR.Finalizers = utils.RemoveString(snCR.Finalizers, "simplyblock.finalizer")
	// 		if err := r.Update(ctx, snCR); err != nil {
	// 			log.Error(err, "Failed to remove finalizer after deletion")
	// 			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	// 		}

	// 		log.Info("Storage node deleted from cluster API and finalizer removed", "name", snCR.Name)
	// 	}

	// 	return ctrl.Result{}, nil
	// }

	if !utils.ContainsString(snCR.Finalizers, "simplyblock.finalizer") {
		snCR.Finalizers = append(snCR.Finalizers, "simplyblock.finalizer")
		if err := r.Update(ctx, snCR); err != nil {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	if err := r.labelWorkerNodes(ctx, snCR); err != nil {
		return ctrl.Result{}, err
	}

	sa := utils.BuildStorageNodeServiceAccount(snCR.Namespace)
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error { return nil })
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply ServiceAccount: %w", err)
	}

	cr := utils.BuildStorageNodeClusterRole(utils.BoolPtrOrFalse(snCR.Spec.OpenShiftCluster))
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error { return nil })
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply ClusterRole: %w", err)
	}

	crb := utils.BuildStorageNodeClusterRoleBinding(snCR.Namespace)
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error { return nil })
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply ClusterRoleBinding: %w", err)
	}

	ds := utils.BuildStorageNodeDaemonSet(snCR)

	if err := controllerutil.SetControllerReference(snCR, ds, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var existing appsv1.DaemonSet
	err = r.Get(ctx, client.ObjectKey{Name: ds.Name, Namespace: ds.Namespace}, &existing)
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

	for _, nodeName := range snCR.Spec.WorkerNodes {
		nodeExists := false
		for _, n := range snCR.Status.Nodes {
			if n.Hostname == nodeName {
				nodeExists = true
				break
			}
		}
		if nodeExists {
			continue
		}

		ip, err := getNodeInternalIP(ctx, r.Client, nodeName)
		if err != nil {
			log.Error(err, "failed to get internal IP", "node", nodeName)
			return ctrl.Result{RequeueAfter: time.Second * 10}, nil
		}

		if snCR.Spec.Partitions != nil {
			log.Info("Number of Partitions", "raw pointer", snCR.Spec.Partitions, "value", *snCR.Spec.Partitions)
		} else {
			log.Info("Number of Partitions is nil")
		}

		log.Info("Number of Partitions after IntPtrOrDefault", "value", utils.IntPtrOrDefault(snCR.Spec.Partitions, 1))

		if err := checkNodeInfoReachable(ctx, ip); err != nil {
			log.Info("Storage node API not reachable yet, requeueing",
				"node", nodeName,
				"ip", ip,
				"error", err.Error(),
			)

			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		nodeAddress := fmt.Sprintf("%s:5000", ip)
		params := utils.StorageNodeAddParams{
			NodeAddress:         nodeAddress,
			InterfaceName:       snCR.Spec.MgmtIfc,
			MaxSnapshots:        utils.IntPtrOrDefault(snCR.Spec.MaxSnapshots, 500),
			HAJM:                utils.BoolPtrOrFalse(snCR.Spec.HAJM),
			TestDevice:          utils.BoolPtrOrFalse(snCR.Spec.TestDevice),
			SPDKImage:           snCR.Spec.SpdkImage,
			SPDKDebug:           utils.BoolPtrOrFalse(snCR.Spec.SPDKDebug),
			FullPageUnmap:       utils.BoolPtrOrFalse(snCR.Spec.FullPageUnmap),
			DataNics:            snCR.Spec.DataNIC,
			Namespace:           snCR.Namespace,
			JMPercent:           utils.IntPtrOrDefault(snCR.Spec.JMPercent, 3),
			Partitions:          utils.IntPtrOrDefault(snCR.Spec.Partitions, 1),
			IOBufSmallPoolCount: 0,
			IOBufLargePoolCount: 0,
			HaJMCount:           utils.IntPtrOrDefault(snCR.Spec.HaJmCount, 3),
		}

		endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes", clusterUUID)

		jsonParams, err := json.MarshalIndent(params, "", "  ")
		if err != nil {
			log.Error(err, "Failed to marshal params")
		} else {
			log.Info("Sending Storage Node Add Request",
				"endpoint", endpoint,
				"request_body", string(jsonParams),
			)
		}

		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodPost, endpoint, params)
		if err != nil || status >= 300 {
			log.Error(err, "StorageNode creation failed", "status", status, "response", string(body))
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}

		log.Info("Cluster API call",
			"endpoint", endpoint,
			"status", status,
			"response", string(body),
		)

		ensureNodeStatus(snCR, nodeName, ip)

		if err := r.Status().Update(ctx, snCR); err != nil {
			log.Error(err, "Failed to update storage node status")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if err := waitForNodeOnline(ctx, apiClient, clusterSecret, clusterUUID, ip, nodeName, snCR, r); err != nil {
			log.Error(err, "Node did not become online in time", "node", nodeName)
			// return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
			return ctrl.Result{}, nil
		}
	}

	log.Info("Storage node created successfully", "node", snCR.Name)
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

		key := "io.simplyblock.node-type"
		value := "simplyblock-storage-plane"

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

func getNodeInternalIP(ctx context.Context, c client.Client, nodeName string) (string, error) {
	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}

	return "", fmt.Errorf("node %s has no InternalIP", nodeName)
}

func ensureNodeStatus(
	snCR *simplyblockv1alpha1.StorageNode,
	nodeName, ip string,
) *simplyblockv1alpha1.NodeStatus {

	for i := range snCR.Status.Nodes {
		if snCR.Status.Nodes[i].Hostname == nodeName {
			return &snCR.Status.Nodes[i]
		}
	}

	snCR.Status.Nodes = append(snCR.Status.Nodes, simplyblockv1alpha1.NodeStatus{
		Hostname: nodeName,
		MgmtIp:   ip,
		State:    "in_creation",
	})

	return &snCR.Status.Nodes[len(snCR.Status.Nodes)-1]
}

func checkNodeInfoReachable(ctx context.Context, ip string) error {
	url := fmt.Sprintf("http://%s:5000/snode/info", ip)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: 3 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("node info endpoint not reachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node info endpoint returned %d", resp.StatusCode)
	}

	return nil
}

func waitForNodeOnline(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterSecret string,
	clusterUUID string,
	ip string,
	nodeName string,
	snCR *simplyblockv1alpha1.StorageNode,
	r *StorageNodeReconciler,
) error {
	log := logf.FromContext(ctx)
	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)

	retries := 60
	waitInterval := 10 * time.Second

	for attempt := 1; attempt <= retries; attempt++ {
		body, status, err := apiClient.Do(ctx, clusterSecret, http.MethodGet, endpoint, nil)
		log.Info("SNODE LIST raw API response", "endpoint", endpoint, "status", status, "body", string(body))

		if err != nil || status >= 300 {
			log.Error(err, "Failed to get storage node statuses", "node", nodeName, "status", status)
		}

		if strings.TrimSpace(string(body)) == "[]" {
			log.Info("Storage node list is empty, retrying...", "node", nodeName, "attempt", attempt)
			time.Sleep(waitInterval)
			continue
		}

		var apiResp []SNODEAPIResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return fmt.Errorf("failed to unmarshal storage node response for %s: %v", nodeName, err)
		}

		for _, res := range apiResp {
			if res.IP == ip && res.Status == "online" {

				for i := range snCR.Status.Nodes {
					if snCR.Status.Nodes[i].Hostname == nodeName {

						updated := simplyblockv1alpha1.NodeStatus{
							Hostname: nodeName,
							UUID:     res.UUID,
							Health:   strconv.FormatBool(res.Health),
							State:    res.Status,
							MgmtIp:   res.IP,
						}

						if reflect.DeepEqual(snCR.Status.Nodes[i], updated) {
							log.Info("Node already online, status unchanged", "node", nodeName)
							return nil
						}

						snCR.Status.Nodes[i] = updated

						if err := r.Status().Update(ctx, snCR); err != nil {
							log.Error(err, "Failed to update node status to online", "node", nodeName)
						}

						log.Info("Node is online", "node", nodeName)
						return nil
					}
				}

				log.Error(nil, "Node missing from status — invariant violated", "node", nodeName)
				return fmt.Errorf("node %s missing from status", nodeName)
			}
		}
		log.Info("Node not online yet, retrying...", "node", nodeName, "attempt", attempt)
		time.Sleep(waitInterval)
	}

	// Timeout reached
	log.Error(nil, "Timeout waiting for node to become online", "node", nodeName)

	// Update CR status with timeout state
	timeoutNode := simplyblockv1alpha1.NodeStatus{
		Hostname: nodeName,
		MgmtIp:   ip,
		State:    "timeout",
	}
	found := false
	for i := range snCR.Status.Nodes {
		if snCR.Status.Nodes[i].Hostname == nodeName {
			snCR.Status.Nodes[i] = timeoutNode
			found = true
			break
		}
	}
	if !found {
		snCR.Status.Nodes = append(snCR.Status.Nodes, timeoutNode)
	}

	if err := r.Status().Update(ctx, snCR); err != nil {
		log.Error(err, "Failed to update node status after timeout", "node", nodeName)
	}

	return fmt.Errorf("node %s did not become online in time", nodeName)
}
