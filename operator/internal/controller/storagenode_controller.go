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
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	storageNodeFinalizer = "storage.simplyblock.io/storagenode-finalizer"
	// storageNodeSyncInterval is how soon a node is looked at again after a
	// failed read. It stays short because it is a retry rather than a poll.
	storageNodeSyncInterval = 30 * time.Second
	// storageNodeBackstopInterval is how soon a healthy node is re-read from the
	// control plane when nothing has pushed. The stream carries a status change
	// within a second, so this is the correctness floor rather than the
	// mechanism: it covers the operator's own stream goroutine wedging, and the
	// control plane's own note that a change written by a pre-upgrade component
	// can take 30 seconds to reach a stream at all.
	storageNodeBackstopInterval = 3 * time.Minute
)

// StorageNodeReconciler reconciles StorageNode objects.
// It owns the per-node provisioning loop: node-add POST, online polling, status
// sync, and triggering a StorageNodeOps(action=remove) on deletion.
type StorageNodeReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Recorder         events.EventRecorder
	TLSEnabled       bool
	TLSMutualEnabled bool

	// DeviceScopes, if set, receives this node's (cluster, node) scope so the
	// control-plane SSE manager streams the node's devices. The control plane
	// offers no cluster-wide device stream, so the node rather than the cluster
	// is what drives a device subscription. Optional (nil in tests).
	DeviceScopes *cpinformer.ScopeSet
	// NodeRegistries learn which StorageNode object a backend node id belongs
	// to. Both the device and the storage-node subscriptions need that mapping
	// to name what they stream, and this reconciler is where the backend id and
	// the object are known at once. Optional (empty in tests).
	NodeRegistries []NodeObjectRegistry
	// Capacity, if set, supplies the node's storage occupancy. It is separate
	// from the control-plane API and from the stream because neither carries
	// the number: a node's capacity exists only in the metrics the control
	// plane exports. Optional (nil leaves status.resources.capacity absent).
	Capacity NodeCapacitySource
	// Nodes, if set, is the storage-node subscription's cache. Status is read
	// from it in preference to the control-plane API: the stream has already
	// delivered the same DTO, so a request would ask for what is in memory.
	// Optional (nil in tests, and nil leaves the reconciler polling).
	Nodes NodeCache
}

// NodeObjectRegistry is how the StorageNode reconciler tells a subscription
// which object a backend node id names. It is an interface so the reconciler
// does not depend on any subscription's concrete type.
type NodeObjectRegistry interface {
	RegisterNode(nodeID string, node types.NamespacedName)
	UnregisterNode(nodeID string)
}

// NodeCapacitySource supplies how full a node's storage is. It is satisfied by
// atlas-lib's prometheus.Provider, and it is an interface here so that a test
// needs no Prometheus.
type NodeCapacitySource interface {
	// NodeCapacity returns the sample for every node of a cluster, keyed by
	// backend node UUID. A node with no sample is absent.
	NodeCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// NodeCache is the read surface the reconciler needs from the storage-node
// subscription: the node the control plane last reported, and whether the
// cluster's snapshot has arrived at all.
type NodeCache interface {
	// Lookup returns the cached node with the given backend id, or ok=false
	// when the control plane no longer reports it.
	Lookup(nodeID string) (cpinformer.Scope, subscriptions.NodeDTO, bool)
	// List returns every node of a cluster, which is what a StorageNode with
	// no backend id yet has to search to find its own.
	List(scope cpinformer.Scope) []subscriptions.NodeDTO
	// Synced reports whether the cluster's initial snapshot has been applied.
	Synced(scope cpinformer.Scope) bool
	// Triggers is the reconcile-trigger stream; each event names a StorageNode.
	Triggers() <-chan event.GenericEvent
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
		return r.handleDeletion(ctx, &sn, clusterUUID)
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

		if sn.Status.UUID == "" {
			alreadyPosted := sn.Status.PostedAt != nil
			// An adopted node already has a backend record that
			// provisionNode's POST can't match, so check by IP first.
			// Only during an upgrade (upgrade secret present) — normal
			// nodes skip this extra check.
			adopting := !alreadyPosted && r.isUpgradeAdoption(ctx, sn.Namespace, sns.Spec.ClusterName)
			if alreadyPosted || adopting {
				if err := r.pollUUIDFromBackend(ctx, &sn, clusterUUID, apiClient); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		if sn.Status.UUID == "" && sn.Status.PostedAt == nil {
			return r.provisionNode(ctx, &sn, &sns, clusterUUID, apiClient)
		}
		if sn.Status.UUID == "" {
			// POST already sent (by us or a sibling socket); keep polling.
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// Node provisioned → its devices can be streamed. The name is registered
	// before the scope, so the subscription can name what the first snapshot
	// delivers.
	r.registerStreams(clusterUUID, &sn)

	// Node provisioned → sync status periodically.
	return r.syncStatus(ctx, &sn, clusterUUID, apiClient)
}

// registerStreams makes the node's control-plane events nameable and opens its
// device stream. The name is registered before the scope, so a subscription can
// name whatever its first snapshot delivers.
//
// The storage-node stream needs no scope registered here: it is per cluster, so
// the cluster's own controller opens it, and this node's events arrive on it
// whether or not this reconciler has run.
func (r *StorageNodeReconciler) registerStreams(clusterUUID string, sn *simplyblockv1alpha1.StorageNode) {
	if sn.Status.UUID == "" {
		return
	}
	for _, registry := range r.NodeRegistries {
		registry.RegisterNode(sn.Status.UUID, client.ObjectKeyFromObject(sn))
	}
	if r.DeviceScopes != nil {
		r.DeviceScopes.Add(cpinformer.Scope{clusterUUID, sn.Status.UUID})
	}
}

// unregisterStreams closes the node's device stream and stops naming events
// after it. The scope goes first: no further device events can arrive once the
// stream is closed, so the name mappings are dropped second and nothing is left
// naming objects after a node on its way out.
func (r *StorageNodeReconciler) unregisterStreams(clusterUUID string, sn *simplyblockv1alpha1.StorageNode) {
	if sn.Status.UUID == "" {
		return
	}
	if r.DeviceScopes != nil {
		r.DeviceScopes.Remove(cpinformer.Scope{clusterUUID, sn.Status.UUID})
	}
	for _, registry := range r.NodeRegistries {
		registry.UnregisterNode(sn.Status.UUID)
	}
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

	allNodes, ok := r.clusterNodes(ctx, clusterUUID, apiClient)
	if !ok {
		return nil // transient — requeue silently
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
	sn.Status.Hostname = n.Hostname
	sn.Status.FailureDomain = fdPtr(n.FailureDomain)
	sn.Status.Resources = &simplyblockv1alpha1.StorageNodeResources{
		CPU:     &cpu,
		Volumes: &volumes,
	}
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{
		Management: n.IP,
		NvmeOf:     &nvmfPort,
		Lvol:       &lvolPort,
		Rpc:        &rpcPort,
	}
	if err := r.Status().Patch(ctx, sn, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pollUUIDFromBackend: %w", err)
	}
	log.Info("pollUUIDFromBackend: UUID assigned",
		"worker", sn.Spec.WorkerNode, "socketIndex", socketIdx,
		"uuid", n.UUID, "status", n.Status)
	return nil
}

// isUpgradeAdoption reports whether the "simplyblock-<cluster>-upgrade"
// secret exists — the same signal simplyblockstoragecluster_controller.go
// uses to adopt the StorageCluster instead of creating a new one.
func (r *StorageNodeReconciler) isUpgradeAdoption(ctx context.Context, namespace, clusterName string) bool {
	secretName := fmt.Sprintf("simplyblock-%s-upgrade", clusterName)
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, &secret)
	return err == nil
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
		sn.Status.FailureDomain = ns.FailureDomain
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

	// One POST per worker: the backend add_node handles all sockets internally.
	// If any sibling CR for the same worker already has PostedAt, skip the POST
	// and mark ourselves so pollUUIDFromBackend can pick up our UUID.
	if r.workerAlreadyPosted(ctx, sn) {
		log.Info("worker already posted by sibling socket — skipping POST, polling for UUID",
			"worker", sn.Spec.WorkerNode)
		now := metav1.Now()
		patch := client.MergeFrom(sn.DeepCopy())
		sn.Status.PostedAt = &now
		if err := r.Status().Patch(ctx, sn, patch); err != nil {
			log.Error(err, "failed to patch PostedAt for non-primary socket")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Respect MaxParallelNodeAdds: count sibling StorageNode CRs in this set
	// that are in-flight (PostedAt set, UUID not yet assigned) and block if the
	// limit is reached. Defaults to 1 (sequential) when not set, which is safe
	// for FDB clusters where simultaneous node reboots reduce fault tolerance.
	maxParallel := 1
	if sns.Spec.MaxParallelNodeAdds != nil {
		maxParallel = int(*sns.Spec.MaxParallelNodeAdds)
	}
	inFlight, err := r.countInFlightNodes(ctx, sn.Namespace, sn.Spec.StorageNodeSetRef, sn.Spec.WorkerNode)
	if err == nil && inFlight >= maxParallel {
		log.Info("parallel node add limit reached, requeuing",
			"inFlight", inFlight, "max", maxParallel)
		return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
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
		r.Recorder.Eventf(sn, nil, "Warning", "FailureDomainMissing", "FailureDomainMissing", "%s", err.Error())
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
		Partitions:       partitionsPerDevice(sns),
		HaJMCount:        journalManagerCountFromSpec(eff.JournalManagerSpec),
		CRName:           sns.Name,
		CRNameSpace:      sns.Namespace,
		CRPlural:         "storagenodesets",
		Format4K:         ptr.BoolFromOrFalse(sns.Spec.ForceFormat4K),
		SpdkSystemMemory: eff.SpdkSystemMemory,
		FailureDomain:    effectiveFailureDomain(sn, sns),
		Expand:           ptr.BoolFromOrFalse(eff.Expand),
	}

	// Re-read the in-flight count immediately before the POST to narrow the
	// check-then-act race window. The first check (above) filters the common
	// case; this final re-check reduces the window to the round-trip of a
	// single List call, making concurrent overshoot extremely unlikely.
	if recheck, recheckErr := r.countInFlightNodes(ctx, sn.Namespace, sn.Spec.StorageNodeSetRef, sn.Spec.WorkerNode); recheckErr == nil && recheck >= maxParallel {
		log.Info("parallel node add limit reached on re-check, requeuing",
			"inFlight", recheck, "max", maxParallel)
		return ctrl.Result{RequeueAfter: waitForNodeOnlineWaitInterval}, nil
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

// countInFlightNodes returns how many distinct workers (physical hosts) in the
// same StorageNodeSet are still being provisioned, excluding the calling node's
// own worker. A worker is considered in-flight if any of its StorageNode CRs
// has PostedAt set and either has no UUID yet or is still "in_creation."
// Counting distinct workers (not individual CRs) ensures maxParallelNodeAdds
// matches its documented meaning regardless of nodesPerSocket — without this,
// each in-flight host would consume nodesPerSocket slots instead of one.
func (r *StorageNodeReconciler) countInFlightNodes(
	ctx context.Context,
	namespace, snsRef, excludeWorker string,
) (int, error) {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.storageNodeSetRef": snsRef},
	); err != nil {
		return 0, err
	}
	inFlightWorkers := make(map[string]struct{})
	for _, sn := range snList.Items {
		if sn.Spec.WorkerNode == excludeWorker {
			continue
		}
		if sn.Status.PostedAt != nil &&
			sn.Status.Status != utils.NodeStatusTimeout &&
			(sn.Status.UUID == "" || sn.Status.Status == utils.NodeStatusInCreation) {
			inFlightWorkers[sn.Spec.WorkerNode] = struct{}{}
		}
	}
	return len(inFlightWorkers), nil
}

// workerAlreadyPosted returns true if a sibling StorageNode for the same
// worker already has PostedAt or UUID set (UUID too: an adopted sibling
// skips PostedAt entirely). Enforces one POST per worker — the backend
// add_node handles all sockets from a single call.
func (r *StorageNodeReconciler) workerAlreadyPosted(ctx context.Context, sn *simplyblockv1alpha1.StorageNode) bool {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(sn.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sn.Spec.StorageNodeSetRef},
	); err != nil {
		return false
	}
	for _, sibling := range snList.Items {
		if sibling.Name == sn.Name || sibling.Spec.WorkerNode != sn.Spec.WorkerNode {
			continue
		}
		if sibling.Status.PostedAt != nil || sibling.Status.UUID != "" {
			return true
		}
	}
	return false
}

// journalManagerPercentPerDeviceFromSpec returns JM percent from the effective
// JournalManagerSpec, defaulting to 3 when nil.
func journalManagerPercentPerDeviceFromSpec(spec *simplyblockv1alpha1.JournalManagerSpec) int {
	if spec == nil {
		return 3
	}
	return ptr.IntFrom(spec.PercentPerDevice, 3)
}

// journalManagerCountFromSpec returns JM count from the effective
// JournalManagerSpec, defaulting to 3 when nil.
func journalManagerCountFromSpec(spec *simplyblockv1alpha1.JournalManagerSpec) int {
	if spec == nil {
		return 3
	}
	return ptr.IntFrom(spec.Count, 3)
}

// syncStatus fetches the current node status from the backend and updates StorageNode.status.
func (r *StorageNodeReconciler) syncStatus(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	apiClient *webapi.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// The stream has already delivered this node's DTO, so asking the control
	// plane for it would fetch what is in memory. The cache is only trusted for
	// a node it actually holds: an absent one may be gone, or may be a scope
	// whose snapshot has not arrived, and the request below tells those apart
	// by returning either the node or a 404.
	if dto, ok := r.cachedNode(sn.Status.UUID); ok {
		return r.applyNodeStatus(ctx, sn, clusterUUID, nodeResponseFrom(dto))
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s", clusterUUID, sn.Status.UUID)
	body, status, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || status >= 300 {
		if status == http.StatusNotFound {
			// The stored UUID no longer exists on the backend — the cluster may
			// have been reset and nodes re-created with new UUIDs.
			// Clear only UUID (PostedAt is intentionally preserved): provisionNode
			// checks PostedAt and returns early without POSTing, while
			// syncUUIDFromNodeSet and pollUUIDFromBackend find the new UUID from
			// StorageNodeSet.status.nodes[] or by querying the backend by IP.
			log.Info("backend node not found (404) — clearing stale UUID for re-adoption",
				"staleUUID", sn.Status.UUID)
			patch := client.MergeFrom(sn.DeepCopy())
			sn.Status.UUID = ""
			sn.Status.Status = ""
			sn.Status.Health = false
			if patchErr := r.Status().Patch(ctx, sn, patch); patchErr != nil {
				log.Error(patchErr, "failed to clear stale UUID from StorageNode")
			}
			return ctrl.Result{Requeue: true}, nil
		}
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

	return r.applyNodeStatus(ctx, sn, clusterUUID, resp)
}

// capacityWriteThreshold is how much a node's used size has to move before the
// new reading is worth recording: one percent of the node's own total, so a
// larger node tolerates a larger absolute drift.
//
// Some threshold is required rather than merely economical. The reconciler
// watches its own objects, so every status write schedules another reconcile;
// writing a freshly sampled number every time would make the node reconcile
// itself in a loop for as long as any I/O was happening, bounded only by the
// workqueue's rate limiter.
const capacityWriteThresholdPercent = 1

// existingCapacity returns what the object already records, so an unchanged
// reading can be carried forward rather than rewritten.
func existingCapacity(sn *simplyblockv1alpha1.StorageNode) *simplyblockv1alpha1.StorageNodeCapacity {
	if sn.Status.Resources == nil {
		return nil
	}
	return sn.Status.Resources.Capacity
}

// worthWriting reports whether a sample says something the object does not
// already say. A first reading always does; after that the used size has to
// have moved by at least capacityWriteThresholdPercent of the total, or the
// total itself has to have changed, which happens when a device joins or
// leaves the node.
func worthWriting(
	current *simplyblockv1alpha1.StorageNodeCapacity,
	sample prometheus.Capacity,
) bool {
	if !sample.Sampled() {
		return false // nothing has measured this node; say nothing about it
	}
	if current == nil || current.UsedBytes == nil || current.TotalBytes == nil {
		return true
	}
	if *current.TotalBytes != sample.Total {
		return true
	}
	drift := *current.UsedBytes - sample.Used
	if drift < 0 {
		drift = -drift
	}
	return drift*100 >= sample.Total*capacityWriteThresholdPercent
}

// nodeCapacity reads one node's occupancy, or reports that there is none to
// read. A failure is not an error the caller has to handle: the rest of the
// status is correct without it, and a node whose capacity is momentarily
// unknown is better published than not published at all.
func (r *StorageNodeReconciler) nodeCapacity(
	ctx context.Context,
	clusterUUID, nodeUUID string,
) (prometheus.Capacity, bool) {
	if r.Capacity == nil || clusterUUID == "" || nodeUUID == "" {
		return prometheus.Capacity{}, false
	}
	samples, err := r.Capacity.NodeCapacity(ctx, clusterUUID)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("no capacity sample for this node",
			"cluster", clusterUUID, "node", nodeUUID, "err", err.Error())
		return prometheus.Capacity{}, false
	}
	sample, ok := samples[nodeUUID]
	return sample, ok
}

// clusterNodes returns every backend node of the cluster, from the stream's
// cache once it has delivered the cluster's snapshot and from the control plane
// until then.
//
// The cache is preferred because the stream has already delivered exactly this
// list, so requesting it again asks for what is in memory. It is only trusted
// once the scope is synced: an empty unsynced cache and a cluster with no nodes
// look identical, and adoption would read the first as the second and keep
// waiting for a node that is already there.
func (r *StorageNodeReconciler) clusterNodes(
	ctx context.Context,
	clusterUUID string,
	apiClient *webapi.Client,
) ([]SNODEAPIResponse, bool) {
	if r.Nodes != nil && r.Nodes.Synced(cpinformer.Scope{clusterUUID}) {
		cached := r.Nodes.List(cpinformer.Scope{clusterUUID})
		out := make([]SNODEAPIResponse, 0, len(cached))
		for _, dto := range cached {
			out = append(out, nodeResponseFrom(dto))
		}
		return out, true
	}

	endpoint := fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", clusterUUID)
	body, httpStatus, err := apiClient.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil || httpStatus >= 300 {
		return nil, false
	}
	var allNodes []SNODEAPIResponse
	if err := json.Unmarshal(body, &allNodes); err != nil {
		return nil, false
	}
	return allNodes, true
}

// cachedNode returns the node the storage-node stream last reported, when there
// is a subscription and it holds one.
func (r *StorageNodeReconciler) cachedNode(nodeID string) (subscriptions.NodeDTO, bool) {
	if r.Nodes == nil || nodeID == "" {
		return subscriptions.NodeDTO{}, false
	}
	_, dto, ok := r.Nodes.Lookup(nodeID)
	return dto, ok
}

// nodeResponseFrom converts a streamed node into the shape the status
// projection takes. The two are the same wire schema read through different
// transports, so converting is what keeps one projection serving both and makes
// a pushed status and a polled one indistinguishable in the object.
func nodeResponseFrom(dto subscriptions.NodeDTO) SNODEAPIResponse {
	return SNODEAPIResponse{
		UUID:          dto.ID,
		Status:        dto.Status,
		IP:            dto.ManagementIP,
		Health:        dto.HealthCheck,
		Hostname:      dto.Hostname,
		CPU:           int(dto.CPUCount),
		Volumes:       int(dto.Volumes),
		RPC_PORT:      int(dto.RPCPort),
		LVOL_PORT:     int(dto.LvolPort),
		NVMF_PORT:     int(dto.NVMeOFPort),
		FailureDomain: dto.FailureDomain,
	}
}

// applyNodeStatus writes what the control plane reports into the CR's status,
// whichever transport it arrived by.
//
// The requeue is the backstop rather than the mechanism: a status change reaches
// the stream in about a second, and this interval only bounds how long a wedged
// stream can go unnoticed.
func (r *StorageNodeReconciler) applyNodeStatus(
	ctx context.Context,
	sn *simplyblockv1alpha1.StorageNode,
	clusterUUID string,
	resp SNODEAPIResponse,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cpu := int32(resp.CPU)
	volumes := int32(resp.Volumes)
	rpcPort := int32(resp.RPC_PORT)
	lvolPort := int32(resp.LVOL_PORT)
	nvmfPort := int32(resp.NVMF_PORT)

	patch := client.MergeFrom(sn.DeepCopy())
	// Carry the previous capacity forward by default. It is replaced below only
	// when the reading has moved materially, so an unchanged one produces an
	// empty patch and therefore no write and no retrigger.
	capacity := existingCapacity(sn)
	if sample, ok := r.nodeCapacity(ctx, clusterUUID, sn.Status.UUID); ok {
		if worthWriting(capacity, sample) {
			capacity = &simplyblockv1alpha1.StorageNodeCapacity{
				TotalBytes: ptr.To(sample.Total),
				UsedBytes:  ptr.To(sample.Used),
				SampledAt:  ptr.To(metav1.NewTime(sample.SampledAt)),
			}
		}
	}

	sn.Status.Status = resp.Status
	sn.Status.Health = resp.Health
	sn.Status.Hostname = resp.Hostname
	sn.Status.FailureDomain = fdPtr(resp.FailureDomain)
	sn.Status.Resources = &simplyblockv1alpha1.StorageNodeResources{
		CPU:      &cpu,
		Volumes:  &volumes,
		Capacity: capacity,
	}
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{
		Management: resp.IP,
		NvmeOf:     &nvmfPort,
		Lvol:       &lvolPort,
		Rpc:        &rpcPort,
	}

	if err := r.Status().Patch(ctx, sn, patch); err != nil {
		log.Error(err, "failed to patch StorageNode status")
	}
	return ctrl.Result{RequeueAfter: storageNodeBackstopInterval}, nil
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
	if effectiveFailureDomainSet(sn, sns) {
		return nil
	}
	return fmt.Errorf(
		"failureDomain not set for worker %q; add nodeConfigs[%s].failureDomain to StorageNodeSet %q",
		sn.Spec.WorkerNode, sn.Spec.WorkerNode, sns.Name,
	)
}

// partitionsPerDevice translates spec.enableJournalDevice into the backend's
// partitions-per-device count: 0 dedicates a whole NVMe device to the journal
// manager, 1 carves a journal partition out of each storage device. Unset
// defaults to 1, preserving the behavior of the spec.partitions field this
// replaced.
func partitionsPerDevice(sns *simplyblockv1alpha1.StorageNodeSet) int {
	if ptr.BoolFromOrFalse(sns.Spec.EnableJournalDevice) {
		return 0
	}
	return 1
}

// effectiveNodeConfig returns the merged config for a node: fleet defaults
// overridden by any per-node values from StorageNode.spec.overrides.
func effectiveNodeConfig(sn *simplyblockv1alpha1.StorageNode, sns *simplyblockv1alpha1.StorageNodeSet) simplyblockv1alpha1.StorageNodeOverrides {
	eff := simplyblockv1alpha1.StorageNodeOverrides{
		SpdkImage:          sns.Spec.SpdkImage,
		SpdkProxyImage:     sns.Spec.SpdkProxyImage,
		SpdkSystemMemory:   sns.Spec.SpdkSystemMemory,
		JournalManagerSpec: sns.Spec.JournalManagerSpec,
		PcieAllowList:      sns.Spec.PcieAllowList,
		PcieDenyList:       sns.Spec.PcieDenyList,
		PcieModel:          sns.Spec.PcieModel,
		DriveSizeRange:     sns.Spec.DriveSizeRange,
		DeviceNames:        sns.Spec.DeviceNames,
		EnableCpuTopology:  sns.Spec.EnableCpuTopology,
		ReservedSystemCPU:  sns.Spec.ReservedSystemCPU,
		UbuntuHost:         sns.Spec.UbuntuHost,
		Expand:             sns.Spec.Expand,
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
	if o.Expand != nil {
		eff.Expand = o.Expand
	}
	return eff
}

// effectiveFailureDomainSet reports whether a failure domain has been explicitly
// assigned to the node via spec.overrides.failureDomain or spec.nodeFailureDomains.
func effectiveFailureDomainSet(sn *simplyblockv1alpha1.StorageNode, sns *simplyblockv1alpha1.StorageNodeSet) bool {
	if sn.Spec.Overrides != nil && sn.Spec.Overrides.FailureDomain != nil {
		return true
	}
	_, ok := sns.Spec.NodeFailureDomains[sn.Spec.WorkerNode]
	return ok
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
	clusterUUID string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// The node is going away, so stop streaming its devices. Its StorageDevice
	// objects are garbage-collected by the owner reference rather than deleted
	// here.
	r.unregisterStreams(clusterUUID, sn)

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

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageNode{}).
		Named("storagenode").
		Watches(
			&simplyblockv1alpha1.StorageNodeSet{},
			handler.EnqueueRequestsFromMapFunc(r.storageNodeSetToStorageNodeRequests),
		).
		Owns(&simplyblockv1alpha1.StorageNodeOps{})

	// A pushed control-plane change reconciles the node it named. The events
	// already carry the object's own name, so they need no map function.
	if r.Nodes != nil {
		builder = builder.WatchesRawSource(
			source.Channel(r.Nodes.Triggers(), &handler.EnqueueRequestForObject{}),
		)
	}

	return builder.Complete(r)
}
