package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// LiveResolver implements Resolver with direct,
// uncached reads against the API server via a client-go clientset. It is the
// counterpart to InformerResolver for consumers that have no shared-informer
// cache — one-shot CLIs, tests, or paths where a stale cache is unacceptable.
//
// Every call hits the API server; the queries with no server-side selector
// (PVByVolumeHandle, AttachmentsForPV) list and filter in memory, so on hot
// paths prefer InformerResolver, which answers the same queries from an index.
type LiveResolver struct {
	cs kubernetes.Interface
}

var _ Resolver = (*LiveResolver)(nil)

// NewLiveResolver returns a LiveResolver backed by the given clientset.
func NewLiveResolver(cs kubernetes.Interface) *LiveResolver {
	return &LiveResolver{cs: cs}
}

// PVByVolumeHandle returns the managed PV whose CSI volume handle equals h. The
// API has no server-side selector for the handle, so it lists PVs and filters
// using the same key function the informer index uses.
func (r *LiveResolver) PVByVolumeHandle(ctx context.Context, h lvol.VolumeHandle) (*corev1.PersistentVolume, error) {
	list, err := r.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		pv := &list.Items[i]
		for _, key := range VolumeHandleKeys(pv) {
			if key == string(h) {
				return pv, nil
			}
		}
	}
	return nil, fmt.Errorf("pv for volume %q: %w", h, errs.ErrNotFound)
}

// PVForClaim returns the PV bound to claim.
func (r *LiveResolver) PVForClaim(ctx context.Context, claim types.NamespacedName) (*corev1.PersistentVolume, error) {
	pvc, err := r.cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		return nil, notFound(err, fmt.Sprintf("pvc %q", claim))
	}
	if pvc.Spec.VolumeName == "" {
		return nil, fmt.Errorf("pvc %q not bound: %w", claim, errs.ErrNotFound)
	}
	pv, err := r.cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return nil, notFound(err, fmt.Sprintf("pv %q", pvc.Spec.VolumeName))
	}
	return pv, nil
}

// AttachmentsForPV lists the VolumeAttachments referencing pvName.
func (r *LiveResolver) AttachmentsForPV(ctx context.Context, pvName string) ([]storagev1.VolumeAttachment, error) {
	list, err := r.cs.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]storagev1.VolumeAttachment, 0)
	for i := range list.Items {
		va := &list.Items[i]
		if name, ok := AttachmentPVName(va); ok && name == pvName {
			out = append(out, *va)
		}
	}
	return out, nil
}

// StorageClassByName returns the StorageClass named name.
func (r *LiveResolver) StorageClassByName(ctx context.Context, name string) (*storagev1.StorageClass, error) {
	sc, err := r.cs.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, notFound(err, fmt.Sprintf("storageclass %q", name))
	}
	return sc, nil
}

// notFound maps an API not-found error to the shared errs.ErrNotFound sentinel
// (so callers can errors.Is it uniformly across resolvers), passing other
// errors through unchanged.
func notFound(err error, what string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s: %w", what, errs.ErrNotFound)
	}
	return err
}
