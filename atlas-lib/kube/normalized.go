// Reading a volume's handle the way §16.4 says a reader should: the annotation
// when it is there and agrees, and the field otherwise.
//
// The rule itself is lvol.NormalizeHandle, which compares two handles and knows
// nothing about Kubernetes. What is here is the other half, which is where the
// two strings come from — and the reason the annotated half takes a map rather
// than an object is that both kinds carrying the annotation are shaped
// differently and only one of them is in the core API. A VolumeSnapshotContent
// belongs to the external snapshotter's module, and this module does not depend
// on it for a map lookup.

package kube

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// NormalizedHandle applies §16.4's rule to a handle and the annotations of the
// object carrying it.
//
// It is the entry point for a kind this module does not know: a caller holding
// a VolumeSnapshotContent passes its source handle and its annotations, and gets
// the same answer a PersistentVolume would.
func NormalizedHandle(
	field lvol.VolumeHandle, annotations map[string]string,
) (lvol.Normalized, bool) {
	return lvol.NormalizeHandle(field, lvol.VolumeHandle(annotations[AnnoVolumeHandle]))
}

// NormalizedVolumeHandleFromPV reads a PersistentVolume's handle with the
// annotation preferred, and reports errs.ErrUnsupported for a volume this
// driver does not own.
//
// It stands beside VolumeHandleFromPV rather than replacing it, because the two
// answer different questions and both have callers. VolumeHandleFromPV asks
// what the object's spec says, which is what the upgrade needs in order to find
// the volumes whose spelling is legacy at all. This asks which pool the volume
// is in, which is what everything else needs.
func NormalizedVolumeHandleFromPV(pv *corev1.PersistentVolume) (lvol.Normalized, error) {
	raw, err := VolumeHandleFromPV(pv)
	if err != nil {
		return lvol.Normalized{}, err
	}
	normalized, wellFormed := NormalizedHandle(raw, pv.GetAnnotations())
	if !wellFormed {
		return lvol.Normalized{}, fmt.Errorf(
			"pv %q carries the handle %q, which is not well formed: %w",
			pvName(pv), raw, errs.ErrUnsupported)
	}
	return normalized, nil
}
