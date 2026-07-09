package kube

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// Resolver performs the live Kubernetes lookups needed to correlate a
// logical volume with its PV/PVC/VolumeAttachment. A consumer implements
// it with a client-go lister or a controller-runtime client; this package
// supplies the pure extraction/aggregation logic on top.
type Resolver interface {
	// PVByVolumeHandle finds the PV whose CSI volume handle equals h.
	PVByVolumeHandle(ctx context.Context, h lvol.VolumeHandle) (*corev1.PersistentVolume, error)
	// PVForClaim returns the PV bound to the given PVC.
	PVForClaim(ctx context.Context, claim types.NamespacedName) (*corev1.PersistentVolume, error)
	// AttachmentsForPV lists VolumeAttachments referencing the PV name.
	AttachmentsForPV(ctx context.Context, pvName string) ([]storagev1.VolumeAttachment, error)
}

// ResolveBinding assembles the full Binding for a logical volume from the
// PV, its claim, and any VolumeAttachment, using r for the live lookups.
func ResolveBinding(ctx context.Context, r Resolver, h lvol.VolumeHandle) (Binding, error) {
	pv, err := r.PVByVolumeHandle(ctx, h)
	if err != nil {
		return Binding{}, err
	}

	b := Binding{VolumeHandle: h, PersistentVolumeName: pv.Name}
	if claim, ok := ClaimRefFromPV(pv); ok {
		b.PersistentVolumeClaim = claim
	}

	attachments, err := r.AttachmentsForPV(ctx, pv.Name)
	if errors.Is(err, errs.ErrUnsupported) {
		// Resolver has no VolumeAttachment informer; leave Node/Attached zero.
		return b, nil
	}
	if err != nil {
		return Binding{}, err
	}
	for i := range attachments {
		va := &attachments[i]
		if va.Status.Attached {
			b.Node = AttachmentNode(va)
			b.Attached = true
			break
		}
	}
	return b, nil
}
