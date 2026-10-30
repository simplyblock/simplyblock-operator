// Admission of a Relocate (design-superclusters.md §8.1, §9.3, §9.4). The
// target is another member of the supercluster the volume's member belongs
// to, and accepts new volumes; the volume is in no consistency group and is
// not encrypted. Each is fixed for the request's life: the target is
// immutable, a group's members move together or not at all, and a move
// deletes the only copy of an encrypted volume's key. Whether the target is
// ready is not checked here, because that changes from one moment to the
// next and the controller holds on it.

package webhook

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// relocateEligible refuses a Relocate no reconcile could carry out.
func (v *PersistentVolumeOpsValidator) relocateEligible(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, handle lvol.Handle,
) admission.Response {
	if ops.Spec.Relocate == nil {
		// The type's own rule covers this.
		return admission.Allowed("")
	}
	name := ops.Spec.PersistentVolumeName

	var pv corev1.PersistentVolume
	if err := v.Client.Get(ctx, types.NamespacedName{Name: name}, &pv); err != nil {
		return *errored(err)
	}
	if pv.Spec.CSI.VolumeAttributes[kube.ParamEncryption] == "true" {
		return *denied("EncryptedVolumeNotMovable: volume %s is encrypted, and a move deletes its "+
			"source and with it the only copy of the key, which replication does not carry.", name)
	}
	if group, err := v.consistencyGroupOf(ctx, &pv); err != nil {
		return *errored(err)
	} else if group != "" {
		return *denied("volume %s is in consistency group %s, whose members stay in one member "+
			"cluster; a move would split the group.", name, group)
	}

	source, err := v.clusterReporting(ctx, handle.ClusterID)
	if err != nil {
		return *errored(err)
	}
	if source == nil {
		return *denied("no StorageCluster reports cluster %s, which volume %s is in.", handle.ClusterID, name)
	}
	if source.Spec.SuperclusterRef == "" {
		return *denied("volume %s is in cluster %s, which is not a member of a supercluster; a "+
			"volume moves between members of one, and between unrelated clusters by replication.",
			name, source.Name)
	}

	targetName := ops.Spec.Relocate.TargetClusterRef
	if targetName == source.Name {
		return *denied("volume %s is already in member %s.", name, source.Name)
	}
	var target simplyblockv1alpha2.StorageCluster
	err = v.Client.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: targetName}, &target)
	switch {
	case apierrors.IsNotFound(err):
		return *denied("there is no StorageCluster %s/%s to move volume %s to.", source.Namespace, targetName, name)
	case err != nil:
		return *errored(err)
	}
	if target.Spec.SuperclusterRef != source.Spec.SuperclusterRef {
		return *denied("cluster %s is not a member of supercluster %s, which volume %s is in%s.",
			targetName, source.Spec.SuperclusterRef, name, membershipOf(&target))
	}
	if ptr.BoolFromOrFalse(target.Spec.DisablePlacement) {
		return *denied("member %s sets disablePlacement and accepts no new volumes.", targetName)
	}
	return admission.Allowed("")
}

// membershipOf says which supercluster a cluster belongs to, for a message.
func membershipOf(cluster *simplyblockv1alpha2.StorageCluster) string {
	if cluster.Spec.SuperclusterRef == "" {
		return ""
	}
	return "; it is a member of supercluster " + cluster.Spec.SuperclusterRef
}

// consistencyGroupOf is the consistency group the volume's claim names, or an
// empty string when it names none.
func (v *PersistentVolumeOpsValidator) consistencyGroupOf(
	ctx context.Context, pv *corev1.PersistentVolume,
) (string, error) {
	ref := pv.Spec.ClaimRef
	if ref == nil {
		return "", nil
	}
	var claim corev1.PersistentVolumeClaim
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &claim)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return claim.Labels[consistencyGroupLabel], nil
}

// clusterReporting is the StorageCluster, in any namespace, whose backend
// cluster has the given UUID, or nil.
func (v *PersistentVolumeOpsValidator) clusterReporting(
	ctx context.Context, uuid string,
) (*simplyblockv1alpha2.StorageCluster, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := v.Client.List(ctx, &clusters); err != nil {
		return nil, err
	}
	for i := range clusters.Items {
		if clusters.Items[i].Status.UUID == uuid {
			return &clusters.Items[i], nil
		}
	}
	return nil, nil
}
