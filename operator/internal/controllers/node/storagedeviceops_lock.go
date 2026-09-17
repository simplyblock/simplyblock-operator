// Mutual exclusion between two operations on one device.
//
// The lock is status.activeOpsRef on the StorageDevice, which
// design-storagedevice.md §4.2 declared against this kind arriving and left
// empty until it did. Two operations on one device would be two restarts of one
// controller, and the second would be issued against a device halfway through
// the first.
//
// It is self-describing, which is what makes a lock outside the holder's own
// object safe: the value is the holder's name, so any reconciler can read it,
// get the named operation, and learn whether the holder is running, finished, or
// gone. The risk a lock like this carries is a holder that dies between taking
// it and recording that it took it, and that is what the staleness check below
// answers.

package node

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// acquireLock takes the device's lock for this operation, and reports whether it
// now holds it.
//
// A lock another operation holds is waited on rather than failed, which is what
// lets operations queue: somebody restarting three devices of one node in
// sequence wants the second to wait, not to fail and need reissuing.
func (r *StorageDeviceOpsReconciler) acquireLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (bool, error) {
	held := device.Status.ActiveOpsRef
	if held == ops.Name {
		return true, nil
	}

	if held != "" {
		takeable, err := r.lockIsStale(ctx, ops.Namespace, held)
		if err != nil || !takeable {
			return false, err
		}
	}

	// The optimistic lock is what makes this a lock at all: two reconcilers
	// that both read the field free patch the same resourceVersion, and one of
	// them gets a conflict and comes back to find the device taken.
	patch := client.MergeFromWithOptions(device.DeepCopy(),
		client.MergeFromWithOptimisticLock{})
	device.Status.ActiveOpsRef = ops.Name
	if err := r.Status().Patch(ctx, device, patch); err != nil {
		if apierrors.IsConflict(err) {
			// Somebody else took it in the same instant. Waiting is the answer,
			// and the next pass reads who has it.
			return false, nil
		}
		return false, fmt.Errorf("take the lock on device %s: %w", device.Name, err)
	}
	return true, nil
}

// lockIsStale reports whether the operation named as the holder has finished or
// no longer exists, which is what lets the lock be taken from it.
//
// A holder that is still running is not stale however long it has been running:
// a restart that is taking its time is exactly the case where breaking the lock
// would issue a second one.
func (r *StorageDeviceOpsReconciler) lockIsStale(
	ctx context.Context, namespace, holder string,
) (bool, error) {
	var running simplyblockv1alpha2.StorageDeviceOps
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: holder}, &running)
	switch {
	case apierrors.IsNotFound(err):
		// The holder is gone, so nothing will ever release it.
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read the operation holding the lock: %w", err)
	}

	switch running.Status.Phase {
	case simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageDeviceOpsPhaseFailed,
		simplyblockv1alpha2.StorageDeviceOpsPhaseAborted:
		return true, nil
	}
	return false, nil
}

// releaseLock gives the device back, and does nothing where this operation is
// not the holder.
//
// The ownership check is what stops a late release from freeing a device the
// next operation has already taken: an operation that failed slowly could
// otherwise clear a lock somebody else is relying on.
func (r *StorageDeviceOpsReconciler) releaseLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) error {
	var fresh simplyblockv1alpha2.StorageDevice
	err := r.Get(ctx, client.ObjectKeyFromObject(device), &fresh)
	switch {
	case apierrors.IsNotFound(err):
		// The device object is gone, which §5.2 does when the node stops
		// reporting the device. There is nothing to release.
		return nil
	case err != nil:
		return fmt.Errorf("read device %s to release it: %w", device.Name, err)
	}

	if fresh.Status.ActiveOpsRef != ops.Name {
		return nil
	}

	patch := client.MergeFromWithOptions(fresh.DeepCopy(),
		client.MergeFromWithOptimisticLock{})
	fresh.Status.ActiveOpsRef = ""
	if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
		return fmt.Errorf("release the lock on device %s: %w", device.Name, err)
	}
	return nil
}
