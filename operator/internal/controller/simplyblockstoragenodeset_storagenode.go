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

// reconcileStorageNodeCRs is the Phase-1 bridge that creates, updates, and
// deletes StorageNode CRs to match StorageNodeSet.spec.workerNodes ×
// spec.socketsToUse. The StorageNodeSet is the single source of truth — this
// function only creates and garbage-collects; the StorageNodeReconciler owns
// the per-node provisioning and status-sync loops.
//
// Called from StorageNodeSetReconciler.Reconcile after fleet infrastructure
// (DaemonSet, RBAC, Services) has been reconciled.

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	atlaskube "github.com/simplyblock/atlas/kube"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// reconcileStorageNodeCRs creates a StorageNode CR for every (worker, socket)
// pair in the StorageNodeSet spec and deletes any owned StorageNode CRs that no
// longer correspond to a configured worker/socket. Overrides are synced from
// spec.nodeConfigs on every call.
func (r *StorageNodeSetReconciler) reconcileStorageNodeCRs(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	log := logf.FromContext(ctx)

	// Compute nodesPerSocket (default 1).
	nodesPerSocket := 1
	if sns.Spec.NodesPerSocket != nil && *sns.Spec.NodesPerSocket > 1 {
		nodesPerSocket = int(*sns.Spec.NodesPerSocket)
	}
	sockets := effectiveSockets(sns)

	// Build the expected set of (worker, globalOrdinal) pairs.
	// globalOrdinal = socketPosition * nodesPerSocket + nodeIndex
	// This uniquely identifies each backend storage node per worker host.
	type workerOrdinal struct {
		worker  string
		ordinal int
	}
	expected := make(map[workerOrdinal]struct{}, len(sns.Spec.WorkerNodes)*len(sockets)*nodesPerSocket)
	for _, worker := range sns.Spec.WorkerNodes {
		for si, socket := range sockets {
			_ = socket // socket string is embedded in the ordinal calculation
			for ni := 0; ni < nodesPerSocket; ni++ {
				ordinal := si*nodesPerSocket + ni
				expected[workerOrdinal{worker, ordinal}] = struct{}{}
			}
		}
	}

	// List all StorageNode CRs owned by this StorageNodeSet.
	var owned simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &owned,
		client.InNamespace(sns.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sns.Name},
	); err != nil {
		return fmt.Errorf("listing owned StorageNode CRs: %w", err)
	}

	// Delete stale CRs (worker removed from spec.workerNodes, socket removed,
	// or nodesPerSocket reduced). Manually created CRs (no OwnerReference) are kept.
	for i := range owned.Items {
		sn := &owned.Items[i]
		ordinal := 0
		if sn.Spec.SocketIndex != nil {
			ordinal = int(*sn.Spec.SocketIndex)
		}
		if _, ok := expected[workerOrdinal{sn.Spec.WorkerNode, ordinal}]; ok {
			continue
		}
		owner := metav1.GetControllerOf(sn)
		if owner == nil || owner.Name != sns.Name {
			continue // manually created — preserve
		}
		if err := r.Delete(ctx, sn); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete stale StorageNode CR", "name", sn.Name)
		} else {
			log.Info("deleted stale StorageNode CR", "name", sn.Name,
				"worker", sn.Spec.WorkerNode, "ordinal", ordinal)
		}
	}

	// Index owned CRs by (worker, ordinal). Since the CR name is now a random id
	// (simplyblock-node-<id>) rather than derived from the worker/socket, a slot
	// is identified by its spec fields, not by a computable name.
	existingBySlot := make(map[workerOrdinal]*simplyblockv1alpha1.StorageNode, len(owned.Items))
	for i := range owned.Items {
		sn := &owned.Items[i]
		ord := 0
		if sn.Spec.SocketIndex != nil {
			ord = int(*sn.Spec.SocketIndex)
		}
		existingBySlot[workerOrdinal{sn.Spec.WorkerNode, ord}] = sn
	}

	// Determine which workers host FDB pods — added sequentially.
	fdbWorkers := r.fdbWorkerSet(ctx, sns)

	fdbInFlight := false
	for _, sn := range owned.Items {
		if !fdbWorkers[sn.Spec.WorkerNode] {
			continue
		}
		if sn.Status.UUID == "" {
			fdbInFlight = true
			break
		}
	}

	// Create or sync one StorageNode CR per (worker, socket, nodeIdx).
	// globalOrdinal = socketPosition × nodesPerSocket + nodeIdx is stored as
	// SocketIndex and used by pollUUIDFromBackend to select the correct backend node.
	for _, worker := range sns.Spec.WorkerNodes {
		for si, socket := range sockets {
			for ni := 0; ni < nodesPerSocket; ni++ {
				globalOrdinal := si*nodesPerSocket + ni
				existing := existingBySlot[workerOrdinal{worker, globalOrdinal}]

				if fdbWorkers[worker] && fdbInFlight && existing == nil {
					log.Info("FDB worker: deferring StorageNode CR creation until previous FDB node is online",
						"worker", worker, "socket", socket, "nodeIdx", ni)
					continue
				}
				if err := r.ensureStorageNodeCR(ctx, sns, existing, worker, socket, ni, globalOrdinal); err != nil {
					log.Error(err, "failed to ensure StorageNode CR",
						"worker", worker, "socket", socket, "nodeIdx", ni)
				}
			}
		}
	}

	// Aggregate status: TotalNodes / OnlineNodes / OfflineNodes.
	if err := r.aggregateStorageNodeStatus(ctx, sns); err != nil {
		log.Error(err, "failed to aggregate StorageNode status into StorageNodeSet")
	}

	return nil
}

// ensureStorageNodeCR creates or patches a StorageNode CR.
// socket is the NUMA socket identifier (from socketsToUse), nodeIdx is the
// per-socket node index (0..nodesPerSocket-1), and globalOrdinal is used as
// SocketIndex for backend node lookup in pollUUIDFromBackend.
func (r *StorageNodeSetReconciler) ensureStorageNodeCR(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
	existing *simplyblockv1alpha1.StorageNode,
	worker, socket string,
	nodeIdx, globalOrdinal int,
) error {
	if existing == nil {
		return r.createStorageNodeCR(ctx, sns, worker, socket, nodeIdx, globalOrdinal)
	}

	// Sync overrides from nodeConfigs — the StorageNodeSet is the source of truth.
	overrides, hasConfig := sns.Spec.NodeConfigs[worker]
	desired := existing.Spec.Overrides
	if hasConfig {
		desired = &overrides
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Overrides = desired
	if err := r.Patch(ctx, existing, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("patching StorageNode overrides %s: %w", existing.Name, err)
	}
	return nil
}

// createStorageNodeCR creates a new StorageNode CR with a random,
// DNS-label-safe name (simplyblock-node-<id>). Because the name is random, a
// collision with an existing object is possible; on AlreadyExists we regenerate
// the id and retry.
func (r *StorageNodeSetReconciler) createStorageNodeCR(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
	worker, socket string,
	nodeIdx, globalOrdinal int,
) error {
	log := logf.FromContext(ctx)

	const maxAttempts = 5
	var sn *simplyblockv1alpha1.StorageNode
	var createErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		name := storageNodeCRName(sns.Name)
		sn = buildStorageNodeCR(sns, name, worker, socket, nodeIdx, globalOrdinal)
		if err := controllerutil.SetControllerReference(sns, sn, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on StorageNode %s: %w", name, err)
		}
		createErr = r.Create(ctx, sn)
		if createErr == nil {
			log.Info("created StorageNode CR", "name", name, "worker", worker, "socket", socket, "nodeIdx", nodeIdx)
			break
		}
		if !apierrors.IsAlreadyExists(createErr) {
			return fmt.Errorf("creating StorageNode %s: %w", name, createErr)
		}
		log.Info("StorageNode CR name collision, regenerating id", "name", name)
	}
	if createErr != nil {
		return fmt.Errorf("creating StorageNode for worker %s after %d attempts: %w", worker, maxAttempts, createErr)
	}

	// Backward compatibility: pre-populate UUID and status from the legacy
	// StorageNodeSet.status.nodes[] so nodes that were already provisioned
	// before the three-tier model are adopted immediately (no re-POST).
	for i := range sns.Status.Nodes {
		ns := &sns.Status.Nodes[i]
		if ns.Hostname != worker || ns.UUID == "" {
			continue
		}
		patch := client.MergeFrom(sn.DeepCopy())
		sn.Status.UUID = ns.UUID
		sn.Status.Status = ns.Status
		sn.Status.Health = ns.Health
		sn.Status.Hostname = ns.Hostname
		sn.Status.Resources = &simplyblockv1alpha1.StorageNodeResources{
			CPU:     ns.CPU,
			Volumes: ns.Volumes,
		}
		sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{
			Management: ns.MgmtIp,
			Rpc:        ns.RpcPort,
			Lvol:       ns.LvolPort,
			NvmeOf:     ns.NvmfPort,
		}
		if patchErr := r.Status().Patch(ctx, sn, patch); patchErr != nil {
			log.Error(patchErr, "failed to pre-populate StorageNode status", "name", sn.Name)
		} else {
			log.Info("pre-populated StorageNode status from legacy nodes[]",
				"name", sn.Name, "uuid", ns.UUID, "status", ns.Status)
		}
		break
	}
	return nil
}

// aggregateStorageNodeStatus rolls up online/offline counts from owned
// StorageNode CRs into StorageNodeSet.status.
func (r *StorageNodeSetReconciler) aggregateStorageNodeStatus(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	var owned simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &owned,
		client.InNamespace(sns.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sns.Name},
	); err != nil {
		return err
	}

	var online, offline, suspended, creating, removed int
	for _, sn := range owned.Items {
		switch sn.Status.Status {
		case utils.NodeStatusOnline:
			online++
		case "offline":
			offline++
		case "suspended":
			suspended++
		case "in_creation":
			creating++
		case "removed":
			removed++
		}
	}

	patch := client.MergeFrom(sns.DeepCopy())
	sns.Status.TotalNodes = len(owned.Items)
	sns.Status.OnlineNodes = online
	sns.Status.OfflineNodes = offline
	sns.Status.SuspendedNodes = suspended
	sns.Status.CreatingNodes = creating
	sns.Status.RemovedNodes = removed
	return r.Status().Patch(ctx, sns, patch)
}

// storageNodeCRName builds a DNS-label-safe name for a StorageNode CR:
// "{sns}-{id}" where id is a random short id (atlas kube.NameWithID). The NUMA
// socket and per-socket node index are NOT encoded in the name — they live in
// the CR spec (SocketID/NodeIndex/SocketIndex) — so the name is stable when a
// storage node migrates between workers. Callers create with retry-on-collision.
func storageNodeCRName(snsName string) string {
	return atlaskube.NameWithID(snsName)
}

// sanitiseDNSLabel replaces characters not valid in a DNS label with '-' and
// strips leading/trailing hyphens.
func sanitiseDNSLabel(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// buildStorageNodeCR constructs a new StorageNode CR for the given worker,
// NUMA socket identifier, per-socket node index, and global ordinal.
func buildStorageNodeCR(
	sns *simplyblockv1alpha1.StorageNodeSet,
	name, worker, socketID string,
	nodeIdx, ordinal int,
) *simplyblockv1alpha1.StorageNode {
	globalOrdinal := int32(ordinal)
	ni := int32(nodeIdx)

	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sns.Namespace,
			Labels: map[string]string{
				"storage.simplyblock.io/storagenodeset": sns.Name,
				"storage.simplyblock.io/worker":         sanitiseDNSLabel(worker),
			},
		},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: sns.Name,
			WorkerNode:        worker,
			SocketID:          socketID,
			NodeIndex:         &ni,
			SocketIndex:       &globalOrdinal,
		},
	}

	if overrides, ok := sns.Spec.NodeConfigs[worker]; ok {
		sn.Spec.Overrides = &overrides
	}

	return sn
}

// emitOnStorageNodeForWorker emits an event on the StorageNode CR for the given
// worker, mirroring events that are emitted on the StorageNodeSet.
func (r *StorageNodeSetReconciler) emitOnStorageNodeForWorker(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
	workerNode string,
	eventType, reason, message string,
) {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(sns.Namespace),
		client.MatchingFields{"spec.workerNode": workerNode},
	); err != nil {
		return
	}
	for i := range snList.Items {
		if snList.Items[i].Spec.StorageNodeSetRef == sns.Name {
			r.Recorder.Event(&snList.Items[i], eventType, reason, message)
			return
		}
	}
}

// syncManualStorageNodeStatus merges manually created StorageNode CRs (those
// without a controller OwnerReference pointing to this StorageNodeSet, i.e.
// not in spec.workerNodes) into StorageNodeSet.status.nodes[] so their status
// is visible in the fleet view alongside operator-managed nodes.
func (r *StorageNodeSetReconciler) syncManualStorageNodeStatus(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
) error {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(sns.Namespace),
		client.MatchingFields{"spec.storageNodeSetRef": sns.Name},
	); err != nil {
		return err
	}

	// Build a set of workers already covered by spec.workerNodes.
	managed := make(map[string]struct{}, len(sns.Spec.WorkerNodes))
	for _, w := range sns.Spec.WorkerNodes {
		managed[w] = struct{}{}
	}

	changed := false
	patch := client.MergeFrom(sns.DeepCopy())

	for _, sn := range snList.Items {
		if _, ok := managed[sn.Spec.WorkerNode]; ok {
			continue // already tracked by reconcileWorkerNodes
		}
		if sn.Status.UUID == "" {
			continue // not yet provisioned
		}

		// Check if this node is already in status.nodes[].
		found := false
		for i := range sns.Status.Nodes {
			if sns.Status.Nodes[i].UUID == sn.Status.UUID {
				// Update in-place if fields changed.
				n := &sns.Status.Nodes[i]
				if n.Status != sn.Status.Status || n.Health != sn.Status.Health {
					n.Status = sn.Status.Status
					n.Health = sn.Status.Health
					n.Hostname = sn.Status.Hostname
					if p := sn.Status.Ports; p != nil {
						n.MgmtIp = p.Management
						n.RpcPort = p.Rpc
						n.LvolPort = p.Lvol
						n.NvmfPort = p.NvmeOf
					}
					if r := sn.Status.Resources; r != nil {
						n.CPU = r.CPU
						n.Volumes = r.Volumes
					}
					changed = true
				}
				found = true
				break
			}
		}
		if !found {
			entry := simplyblockv1alpha1.NodeStatus{
				Hostname: sn.Spec.WorkerNode,
				UUID:     sn.Status.UUID,
				Status:   sn.Status.Status,
				Health:   sn.Status.Health,
			}
			if p := sn.Status.Ports; p != nil {
				entry.MgmtIp = p.Management
				entry.RpcPort = p.Rpc
				entry.LvolPort = p.Lvol
				entry.NvmfPort = p.NvmeOf
			}
			if r := sn.Status.Resources; r != nil {
				entry.CPU = r.CPU
				entry.Volumes = r.Volumes
			}
			sns.Status.Nodes = append(sns.Status.Nodes, entry)
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, sns, patch)
}

// storageNodePostedAt returns the PostedAt timestamp from the StorageNode CR
// for the given worker, or nil if not found / not yet set.
func (r *StorageNodeSetReconciler) storageNodePostedAt(
	ctx context.Context,
	namespace, workerNode string,
) *metav1.Time {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.workerNode": workerNode},
	); err != nil {
		return nil
	}
	for _, sn := range snList.Items {
		if sn.Status.PostedAt != nil {
			return sn.Status.PostedAt
		}
	}
	return nil
}

// allStorageNodesOnline returns true if the number of StorageNode CRs for the
// given worker that have a non-empty UUID equals expectedPerHost. Used to gate
// pollNodeOnline so it is only called once every node has been posted AND
// received its UUID from the backend, avoiding premature timeout.
func (r *StorageNodeSetReconciler) allStorageNodesOnline(
	ctx context.Context,
	namespace, workerNode string,
	expectedPerHost int,
) bool {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.workerNode": workerNode},
	); err != nil {
		return false
	}
	online := 0
	for _, sn := range snList.Items {
		if sn.Status.UUID != "" {
			online++
		}
	}
	return online >= expectedPerHost
}

// storageNodeAlreadyPosted returns true if the StorageNode CR for the given
// worker node already has PostedAt set, meaning StorageNodeReconciler has taken
// over provisioning and this reconciler must not duplicate the POST.
func (r *StorageNodeSetReconciler) storageNodeAlreadyPosted(
	ctx context.Context,
	namespace, workerNode string,
) bool {
	var snList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snList,
		client.InNamespace(namespace),
		client.MatchingFields{"spec.workerNode": workerNode},
	); err != nil {
		return false
	}
	for _, sn := range snList.Items {
		if sn.Status.PostedAt != nil {
			return true
		}
	}
	return false
}

// effectiveSockets returns the list of socket identifiers to use. When
// SocketsToUse is empty, a single socket "0" is assumed.
func effectiveSockets(sns *simplyblockv1alpha1.StorageNodeSet) []string {
	if len(sns.Spec.SocketsToUse) == 0 {
		return []string{"0"}
	}
	return sns.Spec.SocketsToUse
}
