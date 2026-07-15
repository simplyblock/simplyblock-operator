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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

	// Build the expected set of (worker, socket) pairs.
	sockets := effectiveSockets(sns)
	type workerSocket struct{ worker, socket string }
	expected := make(map[workerSocket]struct{}, len(sns.Spec.WorkerNodes)*len(sockets))
	for _, worker := range sns.Spec.WorkerNodes {
		for _, socket := range sockets {
			expected[workerSocket{worker, socket}] = struct{}{}
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

	// Delete stale CRs (worker removed from spec.workerNodes or socket removed).
	// Manually created StorageNode CRs (no controller OwnerReference pointing to
	// this StorageNodeSet) are preserved — they can reference the fleet config
	// without being listed in spec.workerNodes.
	for i := range owned.Items {
		sn := &owned.Items[i]
		key := workerSocket{sn.Spec.WorkerNode, socketLabel(sn.Spec.SocketIndex)}
		if _, ok := expected[key]; ok {
			continue
		}
		// Skip if this CR was not created by the operator (no controller owner).
		owner := metav1.GetControllerOf(sn)
		if owner == nil || owner.Name != sns.Name {
			continue
		}
		if err := r.Delete(ctx, sn); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete stale StorageNode CR", "name", sn.Name)
		} else {
			log.Info("deleted stale StorageNode CR", "name", sn.Name,
				"worker", sn.Spec.WorkerNode, "socket", socketLabel(sn.Spec.SocketIndex))
		}
	}

	// Determine which workers host FDB pods — these must be added sequentially.
	fdbWorkers := r.fdbWorkerSet(ctx, sns)

	// Track whether any FDB worker's StorageNode CR is still awaiting its UUID.
	// If so, stop creating new FDB CRs — the StorageNodeReconciler will POST
	// the current one and we wait until it comes online before creating the next.
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

	// Create or sync each expected (worker, socket).
	for _, worker := range sns.Spec.WorkerNodes {
		for _, socket := range sockets {
			// For FDB workers: only create the next CR when no other FDB
			// worker is still being provisioned (UUID not yet assigned).
			if fdbWorkers[worker] && fdbInFlight {
				// Check if THIS worker already has a CR (already in progress or done).
				crName := storageNodeCRName(sns.Name, worker, socket)
				alreadyExists := false
				for _, sn := range owned.Items {
					if sn.Name == crName {
						alreadyExists = true
						break
					}
				}
				if !alreadyExists {
					log.Info("FDB worker: deferring StorageNode CR creation until previous FDB node is online",
						"worker", worker)
					continue // skip — revisit on next reconcile
				}
			}
			if err := r.ensureStorageNodeCR(ctx, sns, worker, socket); err != nil {
				log.Error(err, "failed to ensure StorageNode CR", "worker", worker, "socket", socket)
			}
		}
	}

	// Aggregate status: TotalNodes / OnlineNodes / OfflineNodes.
	if err := r.aggregateStorageNodeStatus(ctx, sns); err != nil {
		log.Error(err, "failed to aggregate StorageNode status into StorageNodeSet")
	}

	return nil
}

// ensureStorageNodeCR creates or patches a StorageNode CR for (worker, socket).
func (r *StorageNodeSetReconciler) ensureStorageNodeCR(
	ctx context.Context,
	sns *simplyblockv1alpha1.StorageNodeSet,
	worker, socket string,
) error {
	log := logf.FromContext(ctx)
	name := storageNodeCRName(sns.Name, worker, socket)

	var existing simplyblockv1alpha1.StorageNode
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: sns.Namespace}, &existing)

	if apierrors.IsNotFound(err) {
		// Create.
		sn := buildStorageNodeCR(sns, name, worker, socket)
		if setErr := controllerutil.SetControllerReference(sns, sn, r.Scheme); setErr != nil {
			return fmt.Errorf("setting owner reference on StorageNode %s: %w", name, setErr)
		}
		if createErr := r.Create(ctx, sn); createErr != nil {
			return fmt.Errorf("creating StorageNode %s: %w", name, createErr)
		}
		log.Info("created StorageNode CR", "name", name, "worker", worker, "socket", socket)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting StorageNode %s: %w", name, err)
	}

	// Sync overrides from nodeConfigs — the StorageNodeSet is the source of truth.
	overrides, hasConfig := sns.Spec.NodeConfigs[worker]
	desired := existing.Spec.Overrides
	if hasConfig {
		desired = &overrides
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Overrides = desired
	if err := r.Patch(ctx, &existing, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("patching StorageNode overrides %s: %w", name, err)
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

// storageNodeCRName builds a deterministic, DNS-label-safe name for a StorageNode CR.
// Pattern: {sns-name}-{sanitised-worker}-{socket}, truncated to 63 chars with
// an FNV-32 suffix to prevent collisions on long names.
func storageNodeCRName(snsName, worker, socket string) string {
	// Sanitise worker hostname: lowercase, replace non-alnum with '-'.
	sanitised := sanitiseDNSLabel(worker)
	raw := snsName + "-" + sanitised + "-" + socket
	raw = strings.ToLower(raw)

	const maxLen = 63
	if len(raw) <= maxLen {
		return raw
	}
	// Append 7-char FNV hash suffix before truncation.
	h := fnv32Hash(worker + socket)
	suffix := fmt.Sprintf("-%06x", h)
	keep := maxLen - len(suffix)
	if keep < 1 {
		keep = 1
	}
	return raw[:keep] + suffix
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

// buildStorageNodeCR constructs a new StorageNode CR for the given worker and socket.
func buildStorageNodeCR(
	sns *simplyblockv1alpha1.StorageNodeSet,
	name, worker, socket string,
) *simplyblockv1alpha1.StorageNode {
	var idx int32
	if socket != "" {
		fmt.Sscanf(socket, "%d", &idx) //nolint:errcheck
	}
	socketIndex := &idx

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
			SocketIndex:       socketIndex,
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
					n.MgmtIp = sn.Status.MgmtIp
					n.Hostname = sn.Status.Hostname
					n.CPU = sn.Status.CPU
					n.Volumes = sn.Status.Volumes
					n.RpcPort = sn.Status.RpcPort
					n.LvolPort = sn.Status.LvolPort
					n.NvmfPort = sn.Status.NvmfPort
					changed = true
				}
				found = true
				break
			}
		}
		if !found {
			sns.Status.Nodes = append(sns.Status.Nodes, simplyblockv1alpha1.NodeStatus{
				Hostname: sn.Spec.WorkerNode,
				UUID:     sn.Status.UUID,
				Status:   sn.Status.Status,
				Health:   sn.Status.Health,
				MgmtIp:   sn.Status.MgmtIp,
				CPU:      sn.Status.CPU,
				Volumes:  sn.Status.Volumes,
				RpcPort:  sn.Status.RpcPort,
				LvolPort: sn.Status.LvolPort,
				NvmfPort: sn.Status.NvmfPort,
			})
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

// socketLabel converts a *int32 socket index back to the string representation
// used in storageNodeCRName, for matching against expected keys.
func socketLabel(idx *int32) string {
	if idx == nil {
		return "0"
	}
	return fmt.Sprintf("%d", *idx)
}
