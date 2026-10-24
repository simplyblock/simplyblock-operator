// Which handle a stage resolves a volume from (design-superclusters.md §8.3).
//
// A PersistentVolume a Relocate moved keeps the handle of a source the move
// deleted, and carries storage.simplyblock.io/relocated-to naming the volume
// the move put it on. The stage resolves from the mark, so a moved volume is
// reached without its source member's records, which a removed member no
// longer has; a later fail-over of the destination is followed from there as
// any other relationship is.

package node

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
)

// resolutionStart is the handle a stage of volumeID resolves from: the mark of
// its PersistentVolume when it carries a well-formed one, and the handle
// itself otherwise.
func resolutionStart(volumeID string, pv *corev1.PersistentVolume) string {
	if pv == nil {
		return volumeID
	}
	mark := pv.Annotations[kube.AnnoRelocatedTo]
	if _, ok := lvol.ParseHandle(lvol.VolumeHandle(mark)); !ok {
		return volumeID
	}
	return mark
}
