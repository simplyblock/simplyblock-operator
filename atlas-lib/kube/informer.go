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
// struct (rather than positional parameters) so new options such as additional
// informers, namespace scoping, or custom index names can be added later
// without breaking callers.
type ResolverConfig struct {
	// PersistentVolumes is the shared informer for cluster PVs. Required.
	PersistentVolumes cache.SharedIndexInformer
	// PersistentVolumeClaims is the shared informer for PVCs. Required.
	PersistentVolumeClaims cache.SharedIndexInformer
	// VolumeAttachments is the shared informer for VolumeAttachments.
	// Optional: leave nil if the consumer does not need attachment
	// queries (e.g., the CSI node driver, which resolves devices locally
	// rather than via VolumeAttachment). When nil, AttachmentsForPV
	// returns errs.ErrUnsupported and ResolveBinding omits Node/Attached.
	VolumeAttachments cache.SharedIndexInformer
	// StorageClasses is the shared informer for StorageClasses. Optional:
	// leave nil if the consumer does not resolve provisioning Properties.
	// When nil, StorageClassByName returns errs.ErrUnsupported.
	StorageClasses cache.SharedIndexInformer
	// VolumeReplications is the shared informer for csi-addons VolumeReplication
	// objects, supplied as unstructured (a dynamic informer), so this package
	// takes no dependency on the csi-addons CRD Go types. Optional: leave nil
	// when the consumer does not resolve sync-replication membership (the
	// operator, or a driver build without sync support). When nil,
	// VolumeReplicationForClaim returns errs.ErrUnsupported.
	VolumeReplications cache.SharedIndexInformer
	// VolumeReplicationClasses is the shared informer for csi-addons
	// VolumeReplicationClass objects, supplied as unstructured. Optional, and
	// paired with VolumeReplications: it is what tells whether a VolumeReplication
	// is synchronous (its class's method parameter). When nil,
	// VolumeReplicationClassByName and SyncVolumeReplicationForClaim return
	// errs.ErrUnsupported.
	VolumeReplicationClasses cache.SharedIndexInformer
}

// InformerResolver implements Resolver against client-go shared informers.
// It works with any source whose informers satisfy cache.SharedIndexInformer,
// whether a standalone SharedInformerFactory (CSI driver) or a controller-runtime
// manager cache (operator), so both consumers share one resolution
// implementation instead of keeping a second cache.
type InformerResolver struct {
	pv      cache.SharedIndexInformer
	pvc     cache.SharedIndexInformer
	va      cache.SharedIndexInformer
	sc      cache.SharedIndexInformer
	vr      cache.SharedIndexInformer
	vrClass cache.SharedIndexInformer
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
	if cfg.VolumeReplications != nil {
		if err := cfg.VolumeReplications.AddIndexers(cache.Indexers{
			IndexVRByClaim: indexVRByClaim,
		}); err != nil {
			return nil, fmt.Errorf("kube: add VolumeReplication claim indexer: %w", err)
		}
	}
	if cfg.VolumeReplicationClasses != nil {
		if err := cfg.VolumeReplicationClasses.AddIndexers(cache.Indexers{
			IndexVRClassByName: indexVRClassByName,
		}); err != nil {
			return nil, fmt.Errorf("kube: add VolumeReplicationClass indexer: %w", err)
		}
	}
	return &InformerResolver{
		pv:      cfg.PersistentVolumes,
		pvc:     cfg.PersistentVolumeClaims,
		va:      cfg.VolumeAttachments,
		sc:      cfg.StorageClasses,
		vr:      cfg.VolumeReplications,
		vrClass: cfg.VolumeReplicationClasses,
	}, nil
}

// NewResolverFromFactory is a convenience constructor for the common case
// of a standalone client-go SharedInformerFactory (e.g., the CSI driver).
func NewResolverFromFactory(f informers.SharedInformerFactory) (*InformerResolver, error) {
	return NewResolver(ResolverConfig{
		PersistentVolumes:      f.Core().V1().PersistentVolumes().Informer(),
		PersistentVolumeClaims: f.Core().V1().PersistentVolumeClaims().Informer(),
		VolumeAttachments:      f.Storage().V1().VolumeAttachments().Informer(),
		StorageClasses:         f.Storage().V1().StorageClasses().Informer(),
	})
}

// NewSyncResolver builds a resolver for the CSI node plugin's sync-replication
// path, which needs only the VolumeReplication and VolumeReplicationClass
// informers: it takes the PVC identity from the CSI volume context, not from a
// PV/PVC lookup, so it runs no cluster-wide PV/PVC watch on every node. Only the
// Sync* and VolumeReplication* methods are valid on the result; PVByVolumeHandle
// and the other PV/PVC/attachment/class lookups return errs.ErrUnsupported. The
// informers must not be running yet, since indexers are added here.
func NewSyncResolver(volumeReplications, volumeReplicationClasses cache.SharedIndexInformer) (*InformerResolver, error) {
	if volumeReplications == nil || volumeReplicationClasses == nil {
		return nil, fmt.Errorf(
			"kube: NewSyncResolver requires the VolumeReplication and VolumeReplicationClass informers: %w",
			errs.ErrUnsupported)
	}
	if err := volumeReplications.AddIndexers(cache.Indexers{IndexVRByClaim: indexVRByClaim}); err != nil {
		return nil, fmt.Errorf("kube: add VolumeReplication claim indexer: %w", err)
	}
	if err := volumeReplicationClasses.AddIndexers(cache.Indexers{
		IndexVRClassByName: indexVRClassByName,
	}); err != nil {
		return nil, fmt.Errorf("kube: add VolumeReplicationClass indexer: %w", err)
	}
	return &InformerResolver{vr: volumeReplications, vrClass: volumeReplicationClasses}, nil
}

// PVByVolumeHandle returns the managed PV whose CSI volume handle equals h. It
// returns errs.ErrUnsupported when the resolver was built without a PV informer
// (e.g., a sync-only resolver from NewSyncResolver).
func (r *InformerResolver) PVByVolumeHandle(ctx context.Context, h lvol.VolumeHandle) (*corev1.PersistentVolume, error) {
	if r.pv == nil {
		return nil, fmt.Errorf("kube: PersistentVolume informer not configured: %w", errs.ErrUnsupported)
	}
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

// PVForClaim returns the PV bound to the given PVC. It returns
// errs.ErrUnsupported when the resolver was built without the PV and PVC
// informers.
func (r *InformerResolver) PVForClaim(ctx context.Context, claim types.NamespacedName) (*corev1.PersistentVolume, error) {
	if r.pvc == nil || r.pv == nil {
		return nil, fmt.Errorf("kube: PV/PVC informers not configured: %w", errs.ErrUnsupported)
	}
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

// VolumeReplicationForVolumeHandle returns the VolumeReplication protecting the
// volume with backend handle h, resolved through the PV that carries the handle
// and the PVC it is bound to. It is the controller-side entry point into the
// VolumeReplication (the node side starts from the PVC identity in the CSI volume
// context, VolumeReplicationForClaim), so the driver's promote and demote can
// read the active site (SyncSiteAnnotation) off the object. It returns
// errs.ErrUnsupported when the resolver lacks the PV or VolumeReplication
// informer, and errs.ErrNotFound when no PV carries the handle, the PV names no
// claim, or no VolumeReplication protects that claim.
func (r *InformerResolver) VolumeReplicationForVolumeHandle(ctx context.Context, h lvol.VolumeHandle) (VolumeReplicationRef, error) {
	pv, err := r.PVByVolumeHandle(ctx, h)
	if err != nil {
		return VolumeReplicationRef{}, err
	}
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.Namespace == "" || ref.Name == "" {
		return VolumeReplicationRef{}, fmt.Errorf("kube: pv %s for volume %q names no claim: %w", pv.GetName(), h, errs.ErrNotFound)
	}
	return r.VolumeReplicationForClaim(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name})
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
// foreign class, even if present in the informer store, resolves to
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
