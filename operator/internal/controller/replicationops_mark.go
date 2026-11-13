// Recording where a migrated volume lives now, on its PersistentVolume, so the
// CSI driver deletes that volume with the claim (see markMigratedVolumes).

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// markMigratedVolumes records on the PersistentVolume of every volume a
// --delete-source migration has finished where its data lives now, under the
// relocated-to annotation a Relocate writes too.
//
// A PersistentVolume's handle is fixed at provisioning, and after the cutover
// it names the deleted source. The CSI driver's DeleteVolume deletes a moved
// volume only through that mark: the chain walk it falls back to never deletes
// a pairing's active volume through an old handle, because under Ramen that
// volume may serve a workload on another cluster. Without the mark, deleting
// the claim of a migrated volume reported the source as not found and left the
// migrated volume behind (bf6 2026-10-29: vol-2 and vol-3).
//
// It is repeated on every poll and changes nothing once the mark is current;
// a volume whose mark could not be written is logged and tried again on the
// next poll.
func (r *ReplicationOpsReconciler) markMigratedVolumes(
	ctx context.Context,
	ops *simplyblockv1alpha1.ReplicationOps,
	apiClient *webapi.Client,
	results []simplyblockv1alpha1.ReplicationOpsResult,
) {
	if !ops.Spec.DeleteSource {
		return
	}
	log := logf.FromContext(ctx)
	lookup := relationshipLookup(apiClient)
	for _, res := range results {
		if res.Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) || res.PVCRef == "" {
			continue
		}
		if err := r.markMigratedVolume(ctx, ops.Namespace, res.PVCRef, lookup); err != nil {
			log.Info("Could not record where a migrated volume lives now; retrying on the next poll",
				"pvc", res.PVCRef, "err", err)
		}
	}
}

// markMigratedVolume writes the mark for the claim pvcRef. A claim or volume
// that is gone, or a handle that no relationship moved, needs no mark.
func (r *ReplicationOpsReconciler) markMigratedVolume(
	ctx context.Context, opsNamespace, pvcRef string, lookup controlplane.RelationshipLookup,
) error {
	ns, name := utils.SplitRef(pvcRef, opsNamespace)
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pvc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pvc.Spec.VolumeName == "" {
		return nil
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pv.Spec.CSI == nil {
		return nil
	}
	start, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle))
	if !ok {
		return nil
	}
	// A volume moved before resolves from its mark, which is one hop from the
	// volume that moved it last.
	if mark, ok := lvol.ParseHandle(lvol.VolumeHandle(pv.Annotations[kube.AnnoRelocatedTo])); ok {
		start = mark
	}
	cur, err := controlplane.ActiveVolume(ctx, start, lookup)
	if err != nil {
		return fmt.Errorf("resolve the volume of %s: %w", pv.Name, err)
	}
	want := string(cur.Handle())
	if want == pv.Spec.CSI.VolumeHandle || pv.Annotations[kube.AnnoRelocatedTo] == want {
		return nil
	}
	patch := client.MergeFromWithOptions(pv.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pv.Annotations == nil {
		pv.Annotations = map[string]string{}
	}
	pv.Annotations[kube.AnnoRelocatedTo] = want
	return r.Patch(ctx, &pv, patch)
}
