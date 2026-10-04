// Mutual exclusion between two operations on one volume, through an annotation
// on the volume.
//
// Every other Ops kind in this group takes status.activeOpsRef on its target. A
// PersistentVolume is a core type this operator does not define and must not
// add fields to, so the lock moves from status to metadata and keeps everything
// else: acquisition is an optimistic-lock patch, release checks ownership, and
// release runs on every terminal path including deletion.
//
// What makes a lock outside the operation's own object safe is that it is
// self-describing. The risk in one is a holder that dies between taking the
// lock and recording that it took it, leaving a volume locked by nobody. That
// does not arise here, because the annotation's value is the holder's name and
// this kind is cluster-scoped, so the name identifies the holder completely:
// any reconciler can read it, get the named operation, and learn which of three
// situations it is in.
//
// design-persistentvolumeops.md §6 is the specification.

package volume

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// acquireLock takes the volume's lock for this operation, and reports whether
// it now holds it.
//
// A lock another operation holds is waited on rather than failed, which is what
// lets this kind queue like the rest of the group: a drain fanning out fifty
// migrations across a handful of volumes wants them to proceed in turn, not to
// have forty-odd of them fail and need retrying by whatever issued them.
func (r *PersistentVolumeOpsReconciler) acquireLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	pv *corev1.PersistentVolume,
) (bool, error) {
	held := pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
	if held == ops.Name {
		return true, nil
	}

	if held != "" {
		takeable, err := r.lockIsStale(ctx, held)
		if err != nil || !takeable {
			return false, err
		}
	}

	// The optimistic lock is what makes this a lock at all: two reconcilers
	// that both read the annotation free patch the same resourceVersion, and
	// one of them gets a 409 and comes back to find the volume taken. It is
	// also what keeps two reconcilers from both breaking a stale lock and both
	// concluding they won.
	patch := client.MergeFromWithOptions(pv.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pv.Annotations == nil {
		pv.Annotations = map[string]string{}
	}
	pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock] = ops.Name
	if err := r.Patch(ctx, pv, patch); err != nil {
		if apierrors.IsConflict(err) {
			// Somebody moved the volume between the read and the write.
			// Whether that was another operation taking the lock is decided by
			// reading it again rather than guessed at here.
			return false, nil
		}
		return false, fmt.Errorf("take the lock on volume %s: %w", pv.Name, err)
	}
	return true, nil
}

// lockIsStale reports whether a lock naming another operation may be broken.
//
// Two of the three situations the annotation can be in are stale. The named
// operation is terminal, so its release did not run — a crash between the two —
// and waiting for a release that will never come would block the volume
// forever. Or it does not exist at all, so it was deleted with its finalizer
// forced or removed out of band, and the annotation is the only thing left of
// it. The third is a live holder, which is waited on.
func (r *PersistentVolumeOpsReconciler) lockIsStale(ctx context.Context, held string) (bool, error) {
	var holder simplyblockv1alpha2.PersistentVolumeOps
	err := r.Get(ctx, types.NamespacedName{Name: held}, &holder)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read the operation %s holding the lock: %w", held, err)
	default:
		return terminal(holder.Status.Phase), nil
	}
}

// releaseLock clears the volume's lock, and only while it still names this
// operation.
//
// The ownership check is the whole of the safety. A pass that started before
// the lock changed hands would otherwise clear a lock somebody else now holds,
// which is worse than not releasing at all: two operations would then be
// copying one logical volume to two places with neither of them knowing.
//
// A volume that is gone is not an error. It took its lock with it, which is the
// state being asked for, and this runs on the deletion path where a failure
// would hold the operation open forever.
func (r *PersistentVolumeOpsReconciler) releaseLock(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) error {
	var pv corev1.PersistentVolume
	err := r.Get(ctx, types.NamespacedName{Name: ops.Spec.PersistentVolumeName}, &pv)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("read volume %s to release its lock: %w", ops.Spec.PersistentVolumeName, err)
	}

	if pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock] != ops.Name {
		return nil
	}

	patch := client.MergeFromWithOptions(pv.DeepCopy(), client.MergeFromWithOptimisticLock{})
	delete(pv.Annotations, simplyblockv1alpha2.PersistentVolumeOpsLock)
	if err := r.Patch(ctx, &pv, patch); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("release the lock on volume %s: %w", pv.Name, err)
	}
	return nil
}
