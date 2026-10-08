// The per-cluster migration slot: one PersistentVolumeOps per storage cluster
// leaves Pending at a time, and every other one waits there.
//
// The control plane runs one data migration per cluster and refuses a second
// while the first runs. Without the slot, every operation a drain fans out
// leaves Pending at once, creates or retries its migration inside its first
// step, and reports Running for a migration that is not running.
//
// The slot is an annotation on the StorageCluster naming the holder, kept the
// way the volume lock in lock.go is kept: an optimistic-lock patch to take it,
// an ownership check to release it, and a holder that is terminal or gone is a
// slot that may be broken. It lives on the StorageCluster because the cluster
// is the unit the control plane serializes on, and the operation, being
// cluster-scoped, is named completely by the annotation's value.

package volume

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// acquireSlot takes the cluster's migration slot for the operation and returns
// the name of the operation holding it when that is another one.
//
// The cluster is read through the uncached reader, so the patch is made against
// what the API server holds and a conflict means another writer.
func (r *PersistentVolumeOpsReconciler) acquireSlot(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	cluster *simplyblockv1alpha2.StorageCluster,
) (holder string, err error) {
	var fresh simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name}
	if err := r.Reader.Get(ctx, key, &fresh); err != nil {
		return "", fmt.Errorf("read cluster %s to take its migration slot: %w", cluster.Name, err)
	}

	held := fresh.Annotations[simplyblockv1alpha2.StorageClusterMigrationSlot]
	if held == ops.Name {
		return "", nil
	}
	if held != "" {
		stale, err := r.lockIsStale(ctx, held)
		if err != nil {
			return "", err
		}
		if !stale {
			return held, nil
		}
	}

	patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[simplyblockv1alpha2.StorageClusterMigrationSlot] = ops.Name
	if err := r.Patch(ctx, &fresh, patch); err != nil {
		if apierrors.IsConflict(err) {
			// Another operation may have taken it between the read and the
			// write. Which one is learned on the next pass, by reading again.
			return "", errSlotContended
		}
		return "", fmt.Errorf("take the migration slot of cluster %s: %w", cluster.Name, err)
	}
	return "", nil
}

// errSlotContended is a slot that changed between the read and the patch.
var errSlotContended = fmt.Errorf("the cluster's migration slot changed while it was being taken")

// releaseSlot clears the operation's name from every StorageCluster whose slot
// carries it, and only from those.
//
// The clusters are found by the annotation rather than through the operation's
// volume, because the release runs on paths where the volume is gone and on
// the terminal path where nothing was resolved.
func (r *PersistentVolumeOpsReconciler) releaseSlot(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) error {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.Reader.List(ctx, &clusters); err != nil {
		return fmt.Errorf("list the clusters to release the migration slot of %s: %w", ops.Name, err)
	}
	for i := range clusters.Items {
		if clusters.Items[i].Annotations[simplyblockv1alpha2.StorageClusterMigrationSlot] != ops.Name {
			continue
		}
		key := client.ObjectKeyFromObject(&clusters.Items[i])
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cluster simplyblockv1alpha2.StorageCluster
			if err := r.Reader.Get(ctx, key, &cluster); err != nil {
				return err
			}
			if cluster.Annotations[simplyblockv1alpha2.StorageClusterMigrationSlot] != ops.Name {
				return nil
			}
			patch := client.MergeFromWithOptions(cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
			delete(cluster.Annotations, simplyblockv1alpha2.StorageClusterMigrationSlot)
			return r.Patch(ctx, &cluster, patch)
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release the migration slot of cluster %s: %w", key.Name, err)
		}
	}
	return nil
}
