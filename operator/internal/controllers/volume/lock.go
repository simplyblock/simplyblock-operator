// Mutual exclusion between two operations on one NVMe-oF subsystem, through an
// annotation on every volume of it.
//
// The subsystem rather than the volume is the unit, because the control plane
// migrates a subsystem: an operation naming one volume moves every volume
// published beside it, so the lock is taken on all of them.
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
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// lockOutcome is what one attempt at the lock found: whether the operation now
// holds it, and, when another operation is in the way, which one and on which
// volume, which is what the waiting operation reports.
type lockOutcome struct {
	acquired bool
	holder   string
	volume   string
}

// acquireLock takes the lock on every volume of the named volume's subsystem
// for this operation, and reports whether it now holds all of them.
//
// The subsystem is the unit because it is what the control plane migrates:
// every volume published under it moves at the one cutover, so an operation
// naming one of them is moving all of them, and an operation naming another
// may not run beside it. Locking only the named volume let two operations on
// siblings each ask for the whole subsystem to be moved.
//
// The volumes are taken in name order and all or nothing. A volume held by a
// live operation releases whatever this pass took and waits, so a queued
// operation holds nothing; the common order is what keeps two operations that
// both need one subsystem from each holding half of it.
//
// An operation already past Pending holds its subsystem, because it was
// admitted holding all of it and only its own terminal path releases any of
// it. Re-reading the membership on every pass would cost two control-plane
// calls per pass of an operation that can run for hours, to learn nothing.
//
// A lock another operation holds is waited on rather than failed, which is what
// lets this kind queue like the rest of the group: a drain fanning out fifty
// migrations across a handful of volumes wants them to proceed in turn, not to
// have forty-odd of them fail and need retrying by whatever issued them.
func (r *PersistentVolumeOpsReconciler) acquireLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	pv *corev1.PersistentVolume,
) (lockOutcome, error) {
	if ops.Status.Step.State != "" && pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock] == ops.Name {
		return lockOutcome{acquired: true}, nil
	}

	volumes, err := r.subsystemVolumes(ctx, pv)
	if err != nil {
		return lockOutcome{}, err
	}

	for _, volume := range volumes {
		held := volume.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
		if held == ops.Name {
			continue
		}
		if held != "" {
			takeable, err := r.lockIsStale(ctx, held)
			if err != nil {
				return lockOutcome{}, err
			}
			if !takeable {
				return lockOutcome{holder: held, volume: volume.Name}, r.releaseLock(ctx, ops)
			}
		}
		taken, err := r.lockVolume(ctx, ops, volume)
		if err != nil {
			return lockOutcome{}, err
		}
		if !taken {
			return lockOutcome{}, r.releaseLock(ctx, ops)
		}
	}
	return lockOutcome{acquired: true}, nil
}

// lockVolume writes this operation's name onto one volume, and reports whether
// the write landed.
//
// The optimistic lock is what makes this a lock at all: two reconcilers that
// both read the annotation free patch the same resourceVersion, and one of them
// gets a 409 and comes back to find the volume taken. It is also what keeps two
// reconcilers from both breaking a stale lock and both concluding they won.
func (r *PersistentVolumeOpsReconciler) lockVolume(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	pv *corev1.PersistentVolume,
) (bool, error) {
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

// subsystemVolumes is every PersistentVolume fronting a volume of the named
// volume's subsystem, the named one included, sorted by name.
//
// Membership is the control plane's: the volume's own record names the
// subsystem, and the subsystem's members are listed across every pool, then
// mapped back to PersistentVolumes through their CSI handles. A volume the
// control plane reports under no subsystem is a subsystem of its own. A
// member with no PersistentVolume is not one this operator can lock, and it
// does not need to be: nothing here would migrate it on its own.
func (r *PersistentVolumeOpsReconciler) subsystemVolumes(
	ctx context.Context, pv *corev1.PersistentVolume,
) ([]*corev1.PersistentVolume, error) {
	alone := []*corev1.PersistentVolume{pv}
	if pv.Spec.CSI == nil {
		return alone, nil
	}
	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !ok {
		return alone, nil
	}

	volume, err := r.API.Volume(ctx, handle.Handle())
	if err != nil {
		return nil, fmt.Errorf("read volume %s to find its subsystem: %w", handle.VolumeID, err)
	}
	if volume.NQN == "" {
		return alone, nil
	}
	members, err := r.API.SubsystemVolumes(ctx, handle.ClusterID, volume.NQN)
	if err != nil {
		return nil, fmt.Errorf("list the volumes of subsystem %s: %w", volume.NQN, err)
	}

	fronting, err := r.volumesFronting(ctx, expectedMembers(members, handle.VolumeID))
	if err != nil {
		return nil, err
	}
	// The named volume is the caller's copy rather than the listing's, so the
	// lock on it patches the resourceVersion the caller read.
	out := alone
	for _, other := range fronting {
		if other.Name != pv.Name {
			out = append(out, other)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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

// releaseLock clears this operation's lock from every volume carrying it, and
// only from those.
//
// The volumes are found by the lock itself rather than by the subsystem's
// membership, because membership is the control plane's and can change while
// an operation runs, while the annotation is exactly what this operation took.
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
	var volumes corev1.PersistentVolumeList
	if err := r.List(ctx, &volumes); err != nil {
		return fmt.Errorf("list the volumes to release the lock of %s: %w", ops.Name, err)
	}
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock] != ops.Name {
			continue
		}
		patch := client.MergeFromWithOptions(pv.DeepCopy(), client.MergeFromWithOptimisticLock{})
		delete(pv.Annotations, simplyblockv1alpha2.PersistentVolumeOpsLock)
		if err := r.Patch(ctx, pv, patch); err != nil &&
			!apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release the lock on volume %s: %w", pv.Name, err)
		}
	}
	return nil
}
