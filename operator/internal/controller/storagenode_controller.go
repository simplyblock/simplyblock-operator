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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	storageNodeFinalizer    = "storage.simplyblock.io/storagenode-finalizer"
	storageNodeSyncInterval = 30 * time.Second
)

// StorageNodeReconciler reconciles StorageNode objects.
// It owns the per-node provisioning loop: node-add POST, online polling, status
// sync, and triggering a StorageNodeOps(action=remove) on deletion.
type StorageNodeReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Recorder         record.EventRecorder
	TLSEnabled       bool
	TLSMutualEnabled bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/finalizers,verbs=update

func (r *StorageNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var sn simplyblockv1alpha1.StorageNode
	if err := r.Get(ctx, req.NamespacedName, &sn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the parent StorageNodeSet for fleet config.
	var sns simplyblockv1alpha1.StorageNodeSet
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sn.Spec.StorageNodeSetRef,
		Namespace: sn.Namespace,
	}, &sns); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("parent StorageNodeSet not found, requeuing", "ref", sn.Spec.StorageNodeSetRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Resolve cluster UUID early — needed for both provisioning and status sync.
	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, sn.Namespace, sns.Spec.ClusterName)
	if err != nil {
		log.Info("cluster UUID not ready yet, requeuing", "cluster", sns.Spec.ClusterName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Handle deletion.
	if !sn.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &sn, &sns)
	}

	// Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&sn, storageNodeFinalizer) {
		controllerutil.AddFinalizer(&sn, storageNodeFinalizer)
		if err := r.Update(ctx, &sn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Sync overrides from the parent StorageNodeSet.
	if err := r.syncOverrides(ctx, &sn, &sns); err != nil {
		return ctrl.Result{}, err
	}

	apiClient := webapi.NewClient()

	// Node not yet provisioned → post to backend.
	if sn.Status.UUID == "" {
		return r.provisionNode(ctx, &sn, &sns, clusterUUID, apiClient)
	}

	// Node provisioned → sync status periodically.
	return r.syncStatus(ctx, &sn, clusterUUID, apiClient)
}

// syncOverrides propagates StorageNodeSet.spec.nodeConfigs[worker] into
// StorageNode.spec.overrides. The StorageNodeSet is the single source of truth.
func (r *StorageNodeReconciler) syncOverrides(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	overrides, ok := sns.Spec.NodeConfigs[sn.Spec.WorkerNode]
	if !ok {
		return nil
	}
	patch := client.MergeFrom(sn.DeepCopy())
	sn.Spec.Overrides = &overrides
	if err := r.Patch(ctx, sn, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("syncing overrides for %s: %w", sn.Name, err)
	}
	return nil
}

// provisionNode posts the node to the backend API and begins polling for online status.
func (r *StorageNodeReconciler) provisionNode(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Guard: if enableFailureDomains is set on the cluster, failureDomain must be populated.
	if err := r.checkFailureDomain(ctx, sn, sns); err != nil {
		r.Recorder.Event(sn, "Warning", "FailureDomainMissing", err.Error())
		log.Info("blocking node-add: "+err.Error(), "node", sn.Name)
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	// Wait until the node's API endpoint is reachable.
	if err := checkNodeInfoReachable(ctx, sn.Spec.WorkerNode, sn.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
		log.V(1).Info("storage node API not reachable yet, requeuing",
			"worker", sn.Spec.WorkerNode, "error", err.Error())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Build effective config: fleet defaults merged with per-node overrides.
	eff := effectiveNodeConfig(sn, sns)

	nodeAddress := utils.StorageNodeSetAPIAddress(sn.Spec.WorkerNode, sn.Namespace)
	params := utils.StorageNodeSetAddParams{
		NodeAddress:      nodeAddress,
		InterfaceName:    sns.Spec.MgmtIfname,
		SPDKImage:        eff.SpdkImage,
		SPDKProxyImage:   eff.SpdkProxyImage,
		DataNics:         sns.Spec.DataIfname,
		Namespace:        sn.Namespace,
		JMPercent:        journalManagerPercentPerDevice(sns),
		Partitions:       utils.IntPtrOrDefault(sns.Spec.Partitions, 1),
		HaJMCount:        journalManagerCount(sns),
		CRName:           sns.Name,
		CRNameSpace:      sns.Namespace,
		CRPlural:         "storagenodesets",
		Format4K:         utils.BoolPtrOrFalse(sns.Spec.ForceFormat4K),
		SpdkSystemMemory: eff.SpdkSystemMemory,
		FailureDomain:    effectiveFailureDomain(sn, sns),
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes", clusterUUID)
	body, status, err := apiClient.Do(ctx, http.MethodPost, endpoint, params)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("unexpected status %d", status)
		}
		log.Error(err, "storage node add failed", "status", status, "response", string(body))
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	log.Info("storage node add POST sent", "endpoint", endpoint, "status", status)

	// Mark PostedAt so the next reconcile can poll online status.
	now := metav1.Now()
	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.PostedAt = &now
	if err := r.Status().Patch(ctx, sn, patch); err != nil {
		log.Error(err, "failed to patch PostedAt")
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// syncStatus fetches the current node status from the backend and updates StorageNode.status.
func (r *StorageNodeReconciler) syncStatus(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, sn.Status.UUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if err == nil {
			err = fmt.Errorf("status %d", status)
		}
		log.Error(err, "failed to GET node status", "uuid", sn.Status.UUID)
		return ctrl.Result{RequeueAfter: storageNodeSyncInterval}, nil
	}

	var resp SNODEAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		log.Error(err, "failed to unmarshal node status response")
		return ctrl.Result{RequeueAfter: storageNodeSyncInterval}, nil
	}

	cpu := int32(resp.CPU)
	volumes := int32(resp.Volumes)
	rpcPort := int32(resp.RPC_PORT)
	lvolPort := int32(resp.LVOL_PORT)
	nvmfPort := int32(resp.NVMF_PORT)

	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.Status = resp.Status
	sn.Status.Health = resp.Health
	sn.Status.CPU = &cpu
	sn.Status.Volumes = &volumes
	sn.Status.MgmtIp = resp.IP
	sn.Status.Hostname = resp.Hostname
	sn.Status.RpcPort = &rpcPort
	sn.Status.LvolPort = &lvolPort
	sn.Status.NvmfPort = &nvmfPort

	if err := r.Status().Patch(ctx, sn, patch); err != nil {
		log.Error(err, "failed to patch StorageNode status")
	}
	return ctrl.Result{RequeueAfter: storageNodeSyncInterval}, nil
}

// checkFailureDomain returns an error if the parent cluster has
// enableFailureDomains=true but this node has no failureDomain set.
func (r *StorageNodeReconciler) checkFailureDomain(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	var cluster simplyblockv1alpha1.StorageCluster
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sns.Spec.ClusterName,
		Namespace: sn.Namespace,
	}, &cluster); err != nil {
		return nil // can't determine; don't block
	}
	if cluster.Spec.EnableFailureDomains == nil || !*cluster.Spec.EnableFailureDomains {
		return nil
	}
	if effectiveFailureDomain(sn, sns) > 0 {
		return nil
	}
	return fmt.Errorf(
		"failureDomain not set for worker %q; add nodeConfigs[%s].failureDomain to StorageNodeSet %q",
		sn.Spec.WorkerNode, sn.Spec.WorkerNode, sns.Name,
	)
}

// effectiveNodeConfig returns the merged config for a node: fleet defaults
// overridden by any per-node values from StorageNode.spec.overrides.
func effectiveNodeConfig(sn *simplyblockv1alpha1.StorageNode, sns *simplyblockv1alpha1.StorageNodeSet) simplyblockv1alpha1.StorageNodeOverrides {
	eff := simplyblockv1alpha1.StorageNodeOverrides{
		SpdkImage:             sns.Spec.SpdkImage,
		SpdkProxyImage:        sns.Spec.SpdkProxyImage,
		MaxLogicalVolumeCount: sns.Spec.MaxLogicalVolumeCount,
		MaxSize:               sns.Spec.MaxSize,
		CorePercentage:        sns.Spec.CorePercentage,
		SpdkSystemMemory:      sns.Spec.SpdkSystemMemory,
		JournalManagerSpec:    sns.Spec.JournalManagerSpec,
		PcieAllowList:         sns.Spec.PcieAllowList,
		PcieDenyList:          sns.Spec.PcieDenyList,
		PcieModel:             sns.Spec.PcieModel,
		DriveSizeRange:        sns.Spec.DriveSizeRange,
		DeviceNames:           sns.Spec.DeviceNames,
		EnableCpuTopology:     sns.Spec.EnableCpuTopology,
		ReservedSystemCPU:     sns.Spec.ReservedSystemCPU,
		UbuntuHost:            sns.Spec.UbuntuHost,
	}
	if sn.Spec.Overrides == nil {
		return eff
	}
	o := sn.Spec.Overrides
	if o.SpdkImage != "" {
		eff.SpdkImage = o.SpdkImage
	}
	if o.SpdkProxyImage != "" {
		eff.SpdkProxyImage = o.SpdkProxyImage
	}
	if o.MaxLogicalVolumeCount != nil {
		eff.MaxLogicalVolumeCount = o.MaxLogicalVolumeCount
	}
	if o.MaxSize != "" {
		eff.MaxSize = o.MaxSize
	}
	if o.CorePercentage != nil {
		eff.CorePercentage = o.CorePercentage
	}
	if o.SpdkSystemMemory != "" {
		eff.SpdkSystemMemory = o.SpdkSystemMemory
	}
	if o.JournalManagerSpec != nil {
		eff.JournalManagerSpec = o.JournalManagerSpec
	}
	if len(o.PcieAllowList) > 0 {
		eff.PcieAllowList = o.PcieAllowList
	}
	if len(o.PcieDenyList) > 0 {
		eff.PcieDenyList = o.PcieDenyList
	}
	if o.PcieModel != "" {
		eff.PcieModel = o.PcieModel
	}
	if o.DriveSizeRange != "" {
		eff.DriveSizeRange = o.DriveSizeRange
	}
	if len(o.DeviceNames) > 0 {
		eff.DeviceNames = o.DeviceNames
	}
	if o.EnableCpuTopology != nil {
		eff.EnableCpuTopology = o.EnableCpuTopology
	}
	if o.ReservedSystemCPU != "" {
		eff.ReservedSystemCPU = o.ReservedSystemCPU
	}
	if o.UbuntuHost != nil {
		eff.UbuntuHost = o.UbuntuHost
	}
	if o.FailureDomain != nil {
		eff.FailureDomain = o.FailureDomain
	}
	return eff
}

// effectiveFailureDomain returns the failure domain for the node:
// StorageNode.spec.overrides.failureDomain takes precedence over
// StorageNodeSet.spec.nodeFailureDomains[worker].
func effectiveFailureDomain(sn *simplyblockv1alpha1.StorageNode, sns *simplyblockv1alpha1.StorageNodeSet) int {
	if sn.Spec.Overrides != nil && sn.Spec.Overrides.FailureDomain != nil {
		return int(*sn.Spec.Overrides.FailureDomain)
	}
	if v, ok := sns.Spec.NodeFailureDomains[sn.Spec.WorkerNode]; ok {
		return int(v)
	}
	return 0
}

// handleDeletion ensures a StorageNodeOps(action=remove) exists for this node
// if it is online, then removes the finalizer once the ops CR completes.
func (r *StorageNodeReconciler) handleDeletion(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	_ *simplyblockv1alpha1.StorageNodeSet,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If the node was never provisioned, skip ops and remove finalizer immediately.
	if sn.Status.UUID == "" {
		controllerutil.RemoveFinalizer(sn, storageNodeFinalizer)
		return ctrl.Result{}, r.Update(ctx, sn)
	}

	if sn.Status.Status == utils.NodeStatusSuspended ||
		sn.Status.Status == utils.ClusterStatusActive ||
		sn.Status.Status == "online" {
		if err := r.ensureRemoveOps(ctx, sn); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If an ops is still active, requeue and wait.
	if sn.Status.ActiveOpsRef != "" {
		log.Info("waiting for StorageNodeOps to complete before finalizer removal",
			"ops", sn.Status.ActiveOpsRef)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(sn, storageNodeFinalizer)
	return ctrl.Result{}, r.Update(ctx, sn)
}

// ensureRemoveOps creates a StorageNodeOps(action=remove) for this StorageNode
// if one does not already exist.
func (r *StorageNodeReconciler) ensureRemoveOps(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
) error {
	opsName := sn.Name + "-remove"
	var existing simplyblockv1alpha1.StorageNodeOps
	err := r.Get(ctx, types.NamespacedName{Name: opsName, Namespace: sn.Namespace}, &existing)
	if err == nil {
		return nil // already exists
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	ops := simplyblockv1alpha1.StorageNodeOps{}
	ops.Name = opsName
	ops.Namespace = sn.Namespace
	ops.Spec.StorageNodeRef = sn.Name
	ops.Spec.Action = "remove"
	if err := controllerutil.SetControllerReference(sn, &ops, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, &ops)
}

// storageNodeSetToStorageNodeRequests maps a StorageNodeSet change to all
// owned StorageNode reconcile requests.
func (r *StorageNodeReconciler) storageNodeSetToStorageNodeRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{"spec.storageNodeSetRef": obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, len(snList.Items))
	for i, sn := range snList.Items {
		reqs[i] = reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      sn.Name,
			Namespace: sn.Namespace,
		}}
	}
	return reqs
}

// SetupWithManager registers the StorageNodeReconciler with the controller manager.
func (r *StorageNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.StorageNode{},
		"spec.storageNodeSetRef",
		func(obj client.Object) []string {
			sn := obj.(*simplyblockv1alpha1.StorageNode)
			return []string{sn.Spec.StorageNodeSetRef}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Named("storagenode").
		Watches(
			&simplyblockv1alpha1.StorageNodeSet{},
			handler.EnqueueRequestsFromMapFunc(r.storageNodeSetToStorageNodeRequests),
		).
		Owns(&simplyblockv1alpha1.StorageNodeOps{}).
		Complete(r)
}
