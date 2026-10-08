// The storage-node pin on a claim, read as a constraint on an operation that
// would move its volume.
//
// A pin is the user's statement of where a volume lives, and the pinned-volume
// controller is the only thing that acts on it: a changed pin raises a move to
// the new node, and a removed pin leaves the volume where it is. Every other
// move of a pinned volume is refused, and so is a move of a volume whose
// subsystem holds a pinned sibling, because the control plane migrates the
// subsystem as a whole and the sibling would move with it.
//
// It lives beside the lock rather than in the pinned-volume controller because
// the refusal is a property of the operation, whoever raised it: a drain, the
// rebalancer, and a hand-written operation are all held to it.

package volume

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// pinConflict is a pin that stands in the operation's way, and what the
// operation does about it.
type pinConflict struct {
	// superseded is a move the pinned-volume controller raised whose pin no
	// longer names its target: the pin was removed, or changed again before
	// the move started. It ends as Aborted, because nothing went wrong; the
	// request it carried was withdrawn.
	superseded bool
	message    string
}

// pinConflictOf reads the pins of the given volumes and returns the first one
// the operation may not move past, or nil when none stands in its way.
//
// The claims are read through the uncached reader: a pin the cache has not
// caught up with is a volume moved against the user's statement, and the
// check runs only while the operation is Pending, so the reads are few.
func (r *PersistentVolumeOpsReconciler) pinConflictOf(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	targetUUID string,
	volumes []*corev1.PersistentVolume,
) (*pinConflict, error) {
	_, pinDriven := ops.Labels[simplyblockv1alpha2.PinnedVolumeLabel]

	for _, pv := range volumes {
		pin, err := r.pinOf(ctx, pv)
		if err != nil {
			return nil, err
		}
		named := pv.Name == ops.Spec.PersistentVolumeName

		switch {
		case named && pinDriven && pin == "":
			return &pinConflict{superseded: true, message: fmt.Sprintf(
				"the pin of volume %s was removed before the move started; "+
					"the volume stays where it is", pv.Name)}, nil
		case named && pinDriven && pin != targetUUID:
			return &pinConflict{superseded: true, message: fmt.Sprintf(
				"volume %s was repinned to storage node %s before the move started; "+
					"the move to the new pin replaces this one", pv.Name, pin)}, nil
		case pin != "" && pin != targetUUID && named:
			return &pinConflict{message: fmt.Sprintf(
				"volume %s is pinned to storage node %s, and a pinned volume cannot be moved; "+
					"change or remove the pin on its claim instead", pv.Name, pin)}, nil
		case pin != "" && pin != targetUUID:
			return &pinConflict{message: fmt.Sprintf(
				"volume %s shares an NVMe-oF subsystem with volume %s and is pinned to "+
					"storage node %s, and a pinned volume cannot be moved; "+
					"change or remove the pin on its claim instead",
				pv.Name, ops.Spec.PersistentVolumeName, pin)}, nil
		}
	}
	return nil, nil
}

// pinOf is the storage node the volume's claim pins it to, or the empty string
// for a volume with no claim, an unpinned one, or one whose claim was replaced
// by another of the same name.
func (r *PersistentVolumeOpsReconciler) pinOf(
	ctx context.Context, pv *corev1.PersistentVolume,
) (string, error) {
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.Name == "" {
		return "", nil
	}
	var claim corev1.PersistentVolumeClaim
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &claim)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read claim %s/%s to check its pin: %w", ref.Namespace, ref.Name, err)
	case ref.UID != "" && claim.UID != ref.UID:
		// A retained volume's claim was deleted and a new, unrelated claim
		// took its name. That claim's pin is not this volume's.
		return "", nil
	}
	return kube.PinnedNode(claim.Annotations), nil
}

// refuseForPin ends an operation a pin stands in the way of.
func (r *PersistentVolumeOpsReconciler) refuseForPin(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, conflict *pinConflict,
) error {
	if conflict.superseded {
		r.event(ops, corev1.EventTypeNormal, ReasonOperationAborted, "%s", conflict.message)
		_, err := r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted, conflict.message)
		return err
	}
	r.event(ops, corev1.EventTypeWarning, ReasonVolumePinned, "%s", conflict.message)
	_, err := r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, conflict.message)
	return err
}

// errPinSuperseded is a pin-driven move whose pin was removed or changed while
// it validated. It ends the operation as Aborted once the migration is taken
// back, because the request it carried was withdrawn rather than refused.
type errPinSuperseded struct{ message string }

func (e *errPinSuperseded) Error() string { return e.message }

// pinsStillAllow reads the subsystem's membership and pins again at the end of
// Validating, the last point before the migration is activated.
//
// The check at Pending saw the members of that moment, and the control plane
// lets a volume join the subsystem until activation. A member that joined
// pinned to another node would move with the subsystem at cutover.
func (r *PersistentVolumeOpsReconciler) pinsStillAllow(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) error {
	volumes, err := r.subsystemVolumes(ctx, subject.pv)
	if err != nil {
		return err
	}
	conflict, err := r.pinConflictOf(ctx, ops, subject.targetUUID, volumes)
	switch {
	case err != nil:
		return err
	case conflict == nil:
		return nil
	case conflict.superseded:
		return &errPinSuperseded{message: conflict.message}
	default:
		return fatalf("%s", conflict.message)
	}
}
