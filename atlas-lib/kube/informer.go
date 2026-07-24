package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

// ResolverConfig holds the inputs for an informer-backed Resolver. It is a
// struct (rather than positional parameters) so new options — additional
// informers, namespace scoping, custom index names — can be added later
// without breaking callers.
type ResolverConfig struct {
	// PersistentVolumes is the shared informer for cluster PVs. Required.
	PersistentVolumes cache.SharedIndexInformer
	// PersistentVolumeClaims is the shared informer for PVCs. Required.
	PersistentVolumeClaims cache.SharedIndexInformer
	// VolumeAttachments is the shared informer for VolumeAttachments.
	// Optional: leave nil if the consumer does not need attachment
	// queries (e.g. the CSI node driver, which resolves devices locally
	// rather than via VolumeAttachment). When nil, AttachmentsForPV
	// returns errs.ErrUnsupported and ResolveBinding omits Node/Attached.
	VolumeAttachments cache.SharedIndexInformer
	// StorageClasses is the shared informer for StorageClasses. Optional:
	// leave nil if the consumer does not resolve provisioning Properties.
	// When nil, StorageClassByName returns errs.ErrUnsupported.
	StorageClasses cache.SharedIndexInformer
}

// InformerResolver implements Resolver against client-go shared informers.
// It works with any source whose informers satisfy cache.SharedIndexInformer
// — a standalone SharedInformerFactory (CSI driver) or a controller-runtime
// manager cache (operator) — so both consumers share one resolution
// implementation instead of keeping a second cache.
type InformerResolver struct {
	pv  cache.SharedIndexInformer
	pvc cache.SharedIndexInformer
	va  cache.SharedIndexInformer
	sc  cache.SharedIndexInformer
}

var _ Resolver = (*InformerResolver)(nil)

// NewResolver registers the required indexers on the supplied informers and
// returns a Resolver backed by them. It must be called before the informers
// are started: indexers cannot be added to a running informer.
func NewResolver(cfg ResolverConfig) (*InformerResolver, error) {
	if cfg.PersistentVolumes == nil || cfg.PersistentVolumeClaims == nil {
		return nil, fmt.Errorf("kube: ResolverConfig requires PV and PVC informers: %w", errs.ErrUnsupported)
	}
	if err := cfg.PersistentVolumes.AddIndexers(cache.Indexers{
		IndexPVByVolumeHandle: indexPVByVolumeHandle,
	}); err != nil {
		return nil, fmt.Errorf("kube: add PV volume-handle indexer: %w", err)
	}
	if cfg.VolumeAttachments != nil {
		if err := cfg.VolumeAttachments.AddIndexers(cache.Indexers{
			IndexVAByPV: indexVAByPV,
		}); err != nil {
			return nil, fmt.Errorf("kube: add VolumeAttachment PV indexer: %w", err)
		}
	}
	if cfg.StorageClasses != nil {
		if err := cfg.StorageClasses.AddIndexers(cache.Indexers{
			IndexSCByName: indexSCByName,
		}); err != nil {
			return nil, fmt.Errorf("kube: add StorageClass name indexer: %w", err)
		}
	}
	return &InformerResolver{
		pv:  cfg.PersistentVolumes,
		pvc: cfg.PersistentVolumeClaims,
		va:  cfg.VolumeAttachments,
		sc:  cfg.StorageClasses,
	}, nil
}

// NewResolverFromFactory is a convenience constructor for the common case
// of a standalone client-go SharedInformerFactory (e.g. the CSI driver).
func NewResolverFromFactory(f informers.SharedInformerFactory) (*InformerResolver, error) {
	return NewResolver(ResolverConfig{
		PersistentVolumes:      f.Core().V1().PersistentVolumes().Informer(),
		PersistentVolumeClaims: f.Core().V1().PersistentVolumeClaims().Informer(),
		VolumeAttachments:      f.Storage().V1().VolumeAttachments().Informer(),
		StorageClasses:         f.Storage().V1().StorageClasses().Informer(),
	})
}

// PVByVolumeHandle returns the managed PV whose CSI volume handle equals h.
func (r *InformerResolver) PVByVolumeHandle(ctx context.Context, h lvol.VolumeHandle) (*corev1.PersistentVolume, error) {
	objs, err := r.pv.GetIndexer().ByIndex(IndexPVByVolumeHandle, string(h))
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if pv, ok := obj.(*corev1.PersistentVolume); ok {
			return pv, nil
		}
	}
	return nil, fmt.Errorf("pv for volume %q: %w", h, errs.ErrNotFound)
}

// PVForClaim returns the PV bound to the given PVC.
func (r *InformerResolver) PVForClaim(ctx context.Context, claim types.NamespacedName) (*corev1.PersistentVolume, error) {
	obj, exists, err := r.pvc.GetIndexer().GetByKey(claim.String())
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("pvc %q: %w", claim, errs.ErrNotFound)
	}
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok || pvc.Spec.VolumeName == "" {
		return nil, fmt.Errorf("pvc %q not bound: %w", claim, errs.ErrNotFound)
	}
	return r.pvByName(pvc.Spec.VolumeName)
}

// AttachmentsForPV lists the VolumeAttachments referencing pvName. It
// returns errs.ErrUnsupported if the resolver was built without a
// VolumeAttachment informer.
func (r *InformerResolver) AttachmentsForPV(ctx context.Context, pvName string) ([]storagev1.VolumeAttachment, error) {
	if r.va == nil {
		return nil, fmt.Errorf("kube: VolumeAttachment informer not configured: %w", errs.ErrUnsupported)
	}
	objs, err := r.va.GetIndexer().ByIndex(IndexVAByPV, pvName)
	if err != nil {
		return nil, err
	}
	out := make([]storagev1.VolumeAttachment, 0, len(objs))
	for _, obj := range objs {
		if va, ok := obj.(*storagev1.VolumeAttachment); ok {
			out = append(out, *va)
		}
	}
	return out, nil
}

// StorageClassByName returns the StorageClass named name from the cache. Only
// simplyblock-provisioned classes are indexed (see StorageClassNameKeys), so a
// foreign class — even if present in the informer store — resolves to
// errs.ErrNotFound. It returns errs.ErrUnsupported if the resolver was built
// without a StorageClass informer.
func (r *InformerResolver) StorageClassByName(ctx context.Context, name string) (*storagev1.StorageClass, error) {
	if r.sc == nil {
		return nil, fmt.Errorf("kube: StorageClass informer not configured: %w", errs.ErrUnsupported)
	}
	objs, err := r.sc.GetIndexer().ByIndex(IndexSCByName, name)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if sc, ok := obj.(*storagev1.StorageClass); ok {
			return sc, nil
		}
	}
	return nil, fmt.Errorf("storageclass %q: %w", name, errs.ErrNotFound)
}

func (r *InformerResolver) pvByName(name string) (*corev1.PersistentVolume, error) {
	obj, exists, err := r.pv.GetIndexer().GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("pv %q: %w", name, errs.ErrNotFound)
	}
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return nil, fmt.Errorf("pv %q: %w", name, errs.ErrNotFound)
	}
	return pv, nil
}

// indexPVByVolumeHandle / indexVAByPV adapt the pure key functions to the
// cache.IndexFunc signature client-go requires.
func indexPVByVolumeHandle(obj interface{}) ([]string, error) {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return nil, nil
	}
	return VolumeHandleKeys(pv), nil
}

func indexVAByPV(obj interface{}) ([]string, error) {
	va, ok := obj.(*storagev1.VolumeAttachment)
	if !ok {
		return nil, nil
	}
	return AttachmentPVKeys(va), nil
}

func indexSCByName(obj interface{}) ([]string, error) {
	sc, ok := obj.(*storagev1.StorageClass)
	if !ok {
		return nil, nil
	}
	return StorageClassNameKeys(sc), nil
}
