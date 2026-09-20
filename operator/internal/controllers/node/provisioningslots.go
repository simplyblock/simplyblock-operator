// The node-add slot: taking one, releasing it, and reaping the ones nobody will.
//
// A node add reboots its host, so the cluster caps how many may be outstanding at
// once. The cap used to be an election — every waiting node ordered the waiting
// workers and advanced if its own place was within the number free — which needs
// every node to be reading the same set. Reconciles are served from an informer
// cache, and a cache filled moments ago is not that set: a node whose siblings
// have not arrived yet elects itself, and so does every other one.
//
// A slot is therefore taken rather than deduced, and the record of who holds one
// lives in status.provisioningSlots on the StorageCluster. One list on one object
// is what makes the taking atomic: the append is an optimistic-locked patch, so
// exactly one node wins a given resourceVersion and the rest are told to count
// again. The same list on the six node objects could not do it, because six
// resourceVersions are six separate agreements.
//
// The ordering that used to be the cap stays, in awaitSlot, as what it can
// honestly be: a tie-break that keeps the same worker winning across passes so a
// node told to wait is not overtaken. Correctness is the patch.

package node

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// takeSlot records this node's hold on the cluster's node-add concurrency and
// reports whether it got one.
//
// The cluster passed in is the one this reconcile read, and its resourceVersion
// is what the patch is conditioned on. A cluster that moved since the read means
// somebody else took a slot in between, so the patch is refused and the caller
// counts again on the next pass rather than appending to a list it has not seen.
func (r *StorageNodeReconciler) takeSlot(
	ctx context.Context,
	node *simplyblockv1alpha2.StorageNode,
	cluster *simplyblockv1alpha2.StorageCluster,
	held []simplyblockv1alpha2.ProvisioningSlot,
	limit int32,
) (bool, error) {
	if int32(len(held)) >= limit {
		return false, nil
	}

	taking := append(slices.Clone(held), simplyblockv1alpha2.ProvisioningSlot{
		Worker:  node.Spec.WorkerNode,
		Node:    node.Name,
		TakenAt: metav1.Now(),
	})
	err := r.writeSlots(ctx, cluster, taking)
	if apierrors.IsConflict(err) {
		// Somebody else patched the list between this reconcile reading the
		// cluster and writing to it, which is the race the lock exists for and
		// not a failure. The count this node made is stale, so it makes it again
		// on the next pass rather than appending to a list it never saw.
		return false, nil
	}
	return err == nil, err
}

// releaseSlot gives up the slot this node holds, and only that one.
//
// A release that cleared the list would free whichever add happened to be
// running alongside this one, which is the same discipline the cluster operation
// lock is released under: an owner clears its own entry or nothing.
//
// It is called on every path that ends a node's add — the UUID arriving, the
// deadline running out, the object going away — and it is safe to call when no
// slot is held, because the list is then already what it should be.
func (r *StorageNodeReconciler) releaseSlot(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cluster simplyblockv1alpha2.StorageCluster
		key := types.NamespacedName{Name: node.Spec.ClusterRef, Namespace: node.Namespace}
		if err := r.Get(ctx, key, &cluster); err != nil {
			// A cluster that is gone holds no slots.
			return client.IgnoreNotFound(err)
		}

		kept := slices.DeleteFunc(
			slices.Clone(cluster.Status.ProvisioningSlots),
			func(slot simplyblockv1alpha2.ProvisioningSlot) bool {
				return slot.Node == node.Name
			})
		if len(kept) == len(cluster.Status.ProvisioningSlots) {
			return nil
		}
		return r.writeSlots(ctx, &cluster, kept)
	})
}

// heldSlots is the recorded list with the entries nobody will ever release
// removed.
//
// Three things end a hold without the holder getting to say so: the object is
// deleted, the add it was taken for has already produced a UUID, and the node has
// failed. None of them are reachable from the holder's own reconcile in the case
// that matters — a deleted object has no reconcile left — so the next node to ask
// for a slot is what collects them. A slot nothing reaps is a cap that never
// reopens.
func (r *StorageNodeReconciler) heldSlots(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.ProvisioningSlot, error) {
	held := make([]simplyblockv1alpha2.ProvisioningSlot, 0, len(cluster.Status.ProvisioningSlots))
	for _, slot := range cluster.Status.ProvisioningSlots {
		var holder simplyblockv1alpha2.StorageNode
		key := types.NamespacedName{Name: slot.Node, Namespace: cluster.Namespace}
		if err := r.Get(ctx, key, &holder); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if holder.Status.UUID != "" ||
			holder.Status.Phase == simplyblockv1alpha2.StorageNodePhaseFailed ||
			!holder.DeletionTimestamp.IsZero() {
			continue
		}
		held = append(held, slot)
	}
	return held, nil
}

// writeSlots patches the list under the resourceVersion the cluster was read at,
// which is the whole of the mutual exclusion.
func (r *StorageNodeReconciler) writeSlots(
	ctx context.Context,
	cluster *simplyblockv1alpha2.StorageCluster,
	slots []simplyblockv1alpha2.ProvisioningSlot,
) error {
	patch := client.MergeFromWithOptions(
		cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	cluster.Status.ProvisioningSlots = slots
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("record the node-add slots of cluster %s: %w", cluster.Name, err)
	}
	return nil
}

// slotHolders is the workers currently holding a slot, which is what the
// FoundationDB rule and the blocked message are phrased over.
func slotHolders(slots []simplyblockv1alpha2.ProvisioningSlot) []string {
	workers := make([]string, 0, len(slots))
	for _, slot := range slots {
		workers = append(workers, slot.Worker)
	}
	slices.Sort(workers)
	return workers
}

// describeSlots renders the holders for the event that reports a node waiting,
// with how long each has been holding.
//
// The age is the part worth reading: a cap doing its job shows a slot seconds
// old, and a cap that has stuck shows one held for hours, and the two are the
// same event without it.
func describeSlots(slots []simplyblockv1alpha2.ProvisioningSlot) string {
	parts := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot.TakenAt.IsZero() {
			parts = append(parts, slot.Worker)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s for %s",
			slot.Worker, time.Since(slot.TakenAt.Time).Truncate(time.Second)))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}
