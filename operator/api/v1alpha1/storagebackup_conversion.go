// Conversion of StorageBackup between this version and the v1alpha2 hub.
//
// One property is renamed (design-property-renames.md §2.1): spec.clusterName
// becomes spec.clusterRef. Everything else is carried across field for field,
// which is spelled out rather than done with a cast because the two structs are
// separate types and a cast would stop compiling the moment either one grows a
// field the other lacks — which is the whole point of having two versions.

package v1alpha1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ConvertTo converts this StorageBackup to the v1alpha2 hub.
func (src *StorageBackup) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.StorageBackup)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec = v1alpha2.StorageBackupSpec{
		ClusterRef:        src.Spec.ClusterName,
		SnapshotName:      src.Spec.SnapshotName,
		SourceClusterUUID: src.Spec.SourceClusterUUID,
	}
	if src.Spec.PVCRef != nil {
		dst.Spec.PVCRef = &v1alpha2.PersistentVolumeClaimRef{
			Name:      src.Spec.PVCRef.Name,
			Namespace: src.Spec.PVCRef.Namespace,
		}
	}

	dst.Status = v1alpha2.StorageBackupStatus(src.Status)

	return nil
}

// ConvertFrom converts the v1alpha2 hub into this StorageBackup.
func (dst *StorageBackup) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.StorageBackup)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec = StorageBackupSpec{
		ClusterName:       src.Spec.ClusterRef,
		SnapshotName:      src.Spec.SnapshotName,
		SourceClusterUUID: src.Spec.SourceClusterUUID,
	}
	if src.Spec.PVCRef != nil {
		dst.Spec.PVCRef = &PersistentVolumeClaimRef{
			Name:      src.Spec.PVCRef.Name,
			Namespace: src.Spec.PVCRef.Namespace,
		}
	}

	dst.Status = StorageBackupStatus(src.Status)

	return nil
}
