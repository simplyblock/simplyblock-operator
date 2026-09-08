// Conversion of StorageNodeOps between this version and the v1alpha2 hub.
//
// Four properties move (design-property-renames.md §2.1, §2.4, and §2.5):
// storageNodeRef becomes nodeRef, drain becomes remove, targetWorkerNode and
// newSsdPcie regroup under migrate, and the action enum is recased. See
// controlplane_conversion.go for why an unmapped enum value is passed through
// rather than rejected.

package v1alpha1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// storageNodeOpsActionToHub maps this version's lowercase actions onto the hub's
// PascalCase ones. HostMaintenance has no v1alpha1 spelling: it is an action the
// redesign adds rather than renames, so it converts down to itself through the
// pass-through rule.
var storageNodeOpsActionToHub = map[string]string{
	"shutdown": string(v1alpha2.StorageNodeOpsActionShutdown),
	"restart":  string(v1alpha2.StorageNodeOpsActionRestart),
	"suspend":  string(v1alpha2.StorageNodeOpsActionSuspend),
	"resume":   string(v1alpha2.StorageNodeOpsActionResume),
	"remove":   string(v1alpha2.StorageNodeOpsActionRemove),
	"migrate":  string(v1alpha2.StorageNodeOpsActionMigrate),
}

// storageNodeOpsActionFromHub is the inverse, derived so the two cannot disagree.
var storageNodeOpsActionFromHub = invertStringMap(storageNodeOpsActionToHub)

// ConvertTo converts this StorageNodeOps to the v1alpha2 hub.
func (src *StorageNodeOps) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageNodeOps)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec = v1alpha2.StorageNodeOpsSpec{
		NodeRef: src.Spec.StorageNodeRef,
		Action: v1alpha2.StorageNodeOpsAction(
			mapOrPassThrough(storageNodeOpsActionToHub, src.Spec.Action),
		),
		Force:          src.Spec.Force,
		ReattachVolume: src.Spec.ReattachVolume,
	}

	// The migrate block exists only if one of the two fields that regroup into it
	// was set. Either one alone is enough: extra drives without a target worker
	// still have to reach the control-plane restart, and an empty block would be
	// a value nobody could have authored, since the target worker is required.
	if src.Spec.TargetWorkerNode != "" || len(src.Spec.NewSsdPcie) > 0 {
		dst.Spec.Migrate = &v1alpha2.MigrateSpec{
			TargetWorkerNode: src.Spec.TargetWorkerNode,
			NewSsdPcie:       src.Spec.NewSsdPcie,
		}
	}
	if src.Spec.Drain != nil {
		dst.Spec.Remove = &v1alpha2.RemoveSpec{
			SystemVolumeFilterRegex: src.Spec.Drain.SystemVolumeFilterRegex,
		}
	}

	dst.Status = v1alpha2.StorageNodeOpsStatus{
		Phase:           v1alpha2.StorageNodeOpsPhase(src.Status.Phase),
		SubPhase:        v1alpha2.StorageNodeOpsSubPhase(src.Status.SubPhase),
		Message:         src.Status.Message,
		VolumesMigrated: src.Status.VolumesMigrated,
		VolumesPending:  src.Status.VolumesPending,
		Triggered:       src.Status.Triggered,
		StartedAt:       src.Status.StartedAt,
		CompletedAt:     src.Status.CompletedAt,
	}

	return nil
}

// ConvertFrom converts the v1alpha2 hub into this StorageNodeOps.
func (dst *StorageNodeOps) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageNodeOps)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec = StorageNodeOpsSpec{
		StorageNodeRef: src.Spec.NodeRef,
		Action:         mapOrPassThrough(storageNodeOpsActionFromHub, string(src.Spec.Action)),
		Force:          src.Spec.Force,
		ReattachVolume: src.Spec.ReattachVolume,
	}
	if m := src.Spec.Migrate; m != nil {
		dst.Spec.TargetWorkerNode = m.TargetWorkerNode
		dst.Spec.NewSsdPcie = m.NewSsdPcie
	}
	if r := src.Spec.Remove; r != nil {
		dst.Spec.Drain = &DrainOpsSpec{SystemVolumeFilterRegex: r.SystemVolumeFilterRegex}
	}

	dst.Status = StorageNodeOpsStatus{
		Phase:           StorageNodeOpsPhase(src.Status.Phase),
		SubPhase:        StorageNodeOpsSubPhase(src.Status.SubPhase),
		Message:         src.Status.Message,
		VolumesMigrated: src.Status.VolumesMigrated,
		VolumesPending:  src.Status.VolumesPending,
		Triggered:       src.Status.Triggered,
		StartedAt:       src.Status.StartedAt,
		CompletedAt:     src.Status.CompletedAt,
	}

	return nil
}
