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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	if sn.Status.UUID == "" {
		// Fast path: old StorageNodeSetReconciler recorded UUID in status.nodes[].
		if err := r.syncUUIDFromNodeSet(ctx, &sn, &sns); err != nil {
			return ctrl.Result{}, err
		}

		if sn.Status.UUID == "" && sn.Status.PostedAt != nil {
			// POST already sent but UUID not in status.nodes[] — this happens for
			// manually created StorageNodes whose worker is not in spec.workerNodes.
			// Poll the backend by worker IP to retrieve the UUID directly.
			if err := r.pollUUIDFromBackend(ctx, &sn, clusterUUID, apiClient); err != nil {
				return ctrl.Result{}, err
			}
			if sn.Status.UUID == "" {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		}

		if sn.Status.UUID == "" {
			return r.provisionNode(ctx, &sn, &sns, clusterUUID, apiClient)
		}
	}

	// Node provisioned → sync status periodically.
	return r.syncStatus(ctx, &sn, clusterUUID, apiClient)
}

// pollUUIDFromBackend lists all backend nodes for the cluster, finds the ones
// matching the worker's internal IP, and assigns the UUID to this StorageNode
// based on its socketIndex. For multi-socket workers (multiple nodes per IP),
// backend nodes are sorted by RPC port (ascending) and matched by position
// to the socketIndex — socket 0 → lowest RPC port, socket 1 → next, etc.
// Called every 10s while PostedAt is set but UUID is still empty; stops as
// soon as the UUID is assigned.
func (r *StorageNodeReconciler) pollUUIDFromBackend(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) error {
	log := logf.FromContext(ctx)

	ip, err := getNodeInternalIP(ctx, r.Client, sn.Spec.WorkerNode)
	if err != nil {
		log.V(1).Info("pollUUIDFromBackend: could not get worker IP, retrying",
			"worker", sn.Spec.WorkerNode, "error", err.Error())
		return nil
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, httpStatus, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || httpStatus >= 300 {
		return nil // transient — requeue silently
	}

	var allNodes []SNODEAPIResponse
	if err := json.Unmarshal(body, &allNodes); err != nil {
		return nil
	}

	// Collect all backend nodes for this worker's IP.
	// Multi-socket: one backend node per socket, each with a different RPC port.
	var matching []SNODEAPIResponse
	for _, n := range allNodes {
		if n.IP == ip && n.UUID != "" {
			matching = append(matching, n)
		}
	}
	if len(matching) == 0 {
		return nil // node not yet visible on backend — requeue
	}

	// Sort by RPC port ascending: socket 0 → lowest port, socket 1 → next, etc.
	slices.SortFunc(matching, func(a, b SNODEAPIResponse) int {
		return a.RPC_PORT - b.RPC_PORT
	})

	socketIdx := 0
	if sn.Spec.SocketIndex != nil {
		socketIdx = int(*sn.Spec.SocketIndex)
	}
	if socketIdx >= len(matching) {
		log.V(1).Info("pollUUIDFromBackend: socket not yet online",
			"worker", sn.Spec.WorkerNode, "socketIndex", socketIdx, "found", len(matching))
		return nil
	}
	n := matching[socketIdx]

	cpu := int32(n.CPU)
	volumes := int32(n.Volumes)
	rpcPort := int32(n.RPC_PORT)
	lvolPort := int32(n.LVOL_PORT)
	nvmfPort := int32(n.NVMF_PORT)

	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.UUID = n.UUID
	sn.Status.Status = n.Status
	sn.Status.Health = n.Health
	sn.Status.MgmtIp = n.IP
	sn.Status.Hostname = n.Hostname
	sn.Status.CPU = &cpu
	sn.Status.Volumes = &volumes
	sn.Status.RpcPort = &rpcPort
	sn.Status.LvolPort = &lvolPort
	sn.Status.NvmfPort = &nvmfPort
	if err := r.Status().Patch(ctx, sn, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pollUUIDFromBackend: %w", err)
	}
	log.Info("pollUUIDFromBackend: UUID assigned",
		"worker", sn.Spec.WorkerNode, "socketIndex", socketIdx,
		"uuid", n.UUID, "status", n.Status)
	return nil
}

// syncUUIDFromNodeSet copies the backend UUID from StorageNodeSet.status.nodes[]
// into StorageNode.status.uuid. This is the Phase 1 bridge: the old
// StorageNodeSetReconciler owns provisioning and tracks UUIDs in its own status;
// the StorageNodeReconciler reads that status so it doesn't re-POST.
func (r *StorageNodeReconciler) syncUUIDFromNodeSet(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	for _, ns := range sns.Status.Nodes {
		if ns.Hostname != sn.Spec.WorkerNode || ns.UUID == "" {
			continue
		}
		patch := client.MergeFrom(sn.DeepCopy())
		sn.Status.UUID = ns.UUID
		sn.Status.Status = ns.Status
		sn.Status.Health = ns.Health
		if err := r.Status().Patch(ctx, sn, patch); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("syncing UUID for StorageNode %s: %w", sn.Name, err)
		}
		return nil
	}
	return nil
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

// provisionNode posts the node to the backend API. StorageNodeReconciler is the
// sole owner of provisioning — the old StorageNodeSetReconciler skips nodes
// whose StorageNode CR already has PostedAt set.
func (r *StorageNodeReconciler) provisionNode(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	sns *simplyblockv1alpha1.StorageNodeSet,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// POST already sent — poll until the UUID appears via syncUUIDFromNodeSet.
	if sn.Status.PostedAt != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Respect MaxParallelNodeAdds: count sibling StorageNode CRs in this set
	// that are in-flight (PostedAt set, UUID not yet assigned) and block if the
	// limit is reached. This replicates the old PendingNodeAdds gate.
	if sns.Spec.MaxParallelNodeAdds != nil {
		inFlight, err := r.countInFlightNodes(ctx, sn.Namespace, sn.Spec.StorageNodeSetRef, sn.Name)
		if err == nil && inFlight >= int(*sns.Spec.MaxParallelNodeAdds) {
			log.Info("parallel node add limit reached, requeuing",
				"inFlight", inFlight, "max", *sns.Spec.MaxParallelNodeAdds)
			return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
		}
	}

	// FDB workers must be added sequentially to avoid simultaneous reboots that
	// would reduce FDB fault tolerance. If this worker hosts an FDB pod and any
	// other FDB worker in the same StorageNodeSet is currently in-flight, block.
	if r.isWorkerFDB(ctx, sn.Namespace, sn.Spec.WorkerNode) {
		if blocked, err := r.isFDBWorkerBlocked(ctx, sn); err == nil && blocked {
			log.Info("FDB worker: another FDB node is in-flight, requeuing sequentially",
				"worker", sn.Spec.WorkerNode)
			return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
		}
	}

	// Guard: failure domain must be set if the feature is enabled.
	if err := r.checkFailureDomain(ctx, sn, sns); err != nil {
		r.Recorder.Event(sn, "Warning", "FailureDomainMissing", err.Error())
		log.Info("blocking node-add: "+err.Error(), "node", sn.Name)
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	// Wait until the node's SPDK API endpoint is reachable.
	if err := checkNodeInfoReachable(ctx, sn.Spec.WorkerNode, sn.Namespace, r.TLSEnabled, r.TLSMutualEnabled); err != nil {
		log.V(1).Info("storage node API not reachable yet, requeuing",
			"worker", sn.Spec.WorkerNode, "error", err.Error())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Merge fleet defaults with per-node overrides — overrides always win.
	eff := effectiveNodeConfig(sn, sns)

	nodeAddress := utils.StorageNodeSetAPIAddress(sn.Spec.WorkerNode, sn.Namespace)
	params := utils.StorageNodeSetAddParams{
		NodeAddress:      nodeAddress,
		InterfaceName:    sns.Spec.MgmtIfname,
		SPDKImage:        eff.SpdkImage,
		SPDKProxyImage:   eff.SpdkProxyImage,
		DataNics:         sns.Spec.DataIfname,
		Namespace:        sn.Namespace,
		JMPercent:        journalManagerPercentPerDeviceFromSpec(eff.JournalManagerSpec),
		Partitions:       utils.IntPtrOrDefault(sns.Spec.Partitions, 1),
		HaJMCount:        journalManagerCountFromSpec(eff.JournalManagerSpec),
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

	now := metav1.Now()
	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.PostedAt = &now
	if err := r.Status().Patch(ctx, sn, patch); err != nil {
		log.Error(err, "failed to patch PostedAt")
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// isWorkerFDB returns true if the given worker node currently hosts at least
// one FDB pod.
func (r *StorageNodeReconciler) isWorkerFDB(ctx context.Context, namespace, workerNode string) bool {
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(namespace),
		client.HasLabels{utils.LabelFDBClusterName},
		client.MatchingFields{"spec.nodeName": workerNode},
	); err != nil {
		return false
	}
	return len(podList.Items) > 0
}

// isFDBWorkerBlocked returns true if any sibling StorageNode in the same
// StorageNodeSet is an FDB worker currently in-flight (PostedAt set, UUID
// empty). Used to enforce sequential adds for FDB nodes.
func (r *StorageNodeReconciler) isFDBWorkerBlocked(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
) (bool, error) {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(sn.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sn.Spec.StorageNodeSetRef},
	); err != nil {
		return false, err
	}
	for _, sibling := range snList.Items {
		if sibling.Name == sn.Name {
			continue
		}
		if sibling.Status.PostedAt == nil || sibling.Status.UUID != "" {
			continue
		}
		// Sibling is in-flight — check if it's also an FDB worker.
		if r.isWorkerFDB(ctx, sn.Namespace, sibling.Spec.WorkerNode) {
			return true, nil
		}
	}
	return false, nil
}

// countInFlightNodes returns how many sibling StorageNode CRs in the same
// StorageNodeSet have PostedAt set but no UUID yet (i.e. add is in progress).
// The calling node is excluded from the count.
func (r *StorageNodeReconciler) countInFlightNodes(
	ctx context.Context,
	namespace, snsRef, excludeName string,
) (int, error) {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.storageNodeSetRef": snsRef},
	); err != nil {
		return 0, err
	}
	count := 0
	for _, sn := range snList.Items {
		if sn.Name == excludeName {
			continue
		}
		if sn.Status.PostedAt != nil && sn.Status.UUID == "" {
			count++
		}
	}
	return count, nil
}

// journalManagerPercentPerDeviceFromSpec returns JM percent from the effective
// JournalManagerSpec, defaulting to 3 when nil.
func journalManagerPercentPerDeviceFromSpec(spec *simplyblockv1alpha1.JournalManagerSpec) int {
	if spec == nil {
		return 3
	}
	return utils.IntPtrOrDefault(spec.PercentPerDevice, 3)
}

// journalManagerCountFromSpec returns JM count from the effective
// JournalManagerSpec, defaulting to 3 when nil.
func journalManagerCountFromSpec(spec *simplyblockv1alpha1.JournalManagerSpec) int {
	if spec == nil {
		return 3
	}
	return utils.IntPtrOrDefault(spec.Count, 3)
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
		sn.Status.Status == utils.NodeStatusOnline {
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
	ops.Spec.Action = utils.NodeActionRemove
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

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha1.StorageNode{},
		"spec.workerNode",
		func(obj client.Object) []string {
			sn := obj.(*simplyblockv1alpha1.StorageNode)
			return []string{sn.Spec.WorkerNode}
		},
	); err != nil {
		return err
	}

	// Index Pods by spec.nodeName for efficient FDB worker detection.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		"spec.nodeName",
		func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			if pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
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
