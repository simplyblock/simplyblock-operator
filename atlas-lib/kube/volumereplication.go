// Correlation of a PVC to the csi-addons VolumeReplication that protects it,
// used by the CSI node plugin to decide, per PVC at stage time, whether a volume
// is sync-managed and to find the VR it then re-points at the pod's site
// (design-sync-replication-csi-addons.md §6.2, §6.6). The VolumeReplication is
// read as unstructured through a dynamic informer, so this package takes no
// dependency on the csi-addons CRD Go types.
package kube

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/simplyblock/atlas/errs"
)

// VolumeReplicationGVR is the GroupVersionResource of the csi-addons
// VolumeReplication CRD. The node plugin watches it with a dynamic informer and
// supplies that informer to NewResolver as ResolverConfig.VolumeReplications.
var VolumeReplicationGVR = schema.GroupVersionResource{
	Group:    "replication.storage.openshift.io",
	Version:  "v1alpha1",
	Resource: "volumereplications",
}

// SyncSiteAnnotation is the annotation the node plugin sets on a sync
// VolumeReplication to name the site the volume is served from. The site rides
// on the object rather than in its (immutable) class, so it can follow the pod
// (design-sync-replication-csi-addons.md §6.2). The driver's promote and demote
// read the target site from it.
const SyncSiteAnnotation = "simplyblock.io/sync-site"

// VolumeReplicationRef is the slice of a VolumeReplication the node plugin acts
// on: which class names its site, and its current replicationState, plus its own
// identity. Extracted from the unstructured object so callers never touch the
// CRD's wire shape.
type VolumeReplicationRef struct {
	Name             string
	Namespace        string
	ClassName        string // spec.volumeReplicationClass
	ReplicationState string // spec.replicationState (primary|secondary)
	// ObservedState and Completed are the status the node plugin's gate reads: a
	// promote on this node's site is done when the volume is Primary and the
	// Completed condition is true (design §6.4, §8).
	ObservedState string // status.state (Primary|Secondary|Unknown)
	Completed     bool   // status.conditions[type=Completed].status == True
	// Site is the value of the SyncSiteAnnotation: the site the node set this
	// volume to be served from, which the promote and demote address (design
	// §6.2). Empty when the annotation is absent.
	Site string
}

// VolumeReplicationRefFrom extracts the fields of interest, spec and status, from
// a VolumeReplication unstructured object.
func VolumeReplicationRefFrom(u *unstructured.Unstructured) VolumeReplicationRef {
	if u == nil {
		return VolumeReplicationRef{}
	}
	class, _, _ := unstructured.NestedString(u.Object, "spec", "volumeReplicationClass")
	state, _, _ := unstructured.NestedString(u.Object, "spec", "replicationState")
	observed, _, _ := unstructured.NestedString(u.Object, "status", "state")
	return VolumeReplicationRef{
		Name:             u.GetName(),
		Namespace:        u.GetNamespace(),
		ClassName:        class,
		ReplicationState: state,
		ObservedState:    observed,
		Completed:        conditionTrue(u, "Completed"),
		Site:             u.GetAnnotations()[SyncSiteAnnotation],
	}
}

// conditionTrue reports whether the named status condition is present and True.
func conditionTrue(u *unstructured.Unstructured, condType string) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		if t != condType {
			continue
		}
		s, _, _ := unstructured.NestedString(m, "status")
		return s == "True"
	}
	return false
}

// VolumeReplicationClaimKeys returns the index key for a VolumeReplication: the
// namespaced PVC it names in spec.dataSource, when that dataSource is a
// PersistentVolumeClaim. A VR and its PVC share a namespace (dataSource is a
// namespace-local reference), so the key is the VR's namespace and the
// dataSource name. Returns nil for a VR with no PVC dataSource.
func VolumeReplicationClaimKeys(u *unstructured.Unstructured) []string {
	if u == nil {
		return nil
	}
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "dataSource", "kind")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "dataSource", "name")
	if kind != "PersistentVolumeClaim" || name == "" {
		return nil
	}
	return []string{types.NamespacedName{Namespace: u.GetNamespace(), Name: name}.String()}
}

// indexVRByClaim adapts VolumeReplicationClaimKeys to the cache.IndexFunc
// signature client-go requires.
func indexVRByClaim(obj interface{}) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	return VolumeReplicationClaimKeys(u), nil
}

// VolumeReplicationForClaim returns the VolumeReplication that protects the given
// PVC. It returns errs.ErrNotFound when no VR references the claim, and
// errs.ErrUnsupported when the resolver was built without a VolumeReplication
// informer. Determining whether that VR is synchronous (its class's method) is
// the caller's next step, via StorageClassByName's VolumeReplicationClass analog.
func (r *InformerResolver) VolumeReplicationForClaim(ctx context.Context, claim types.NamespacedName) (VolumeReplicationRef, error) {
	if r.vr == nil {
		return VolumeReplicationRef{}, fmt.Errorf("kube: no VolumeReplication informer configured: %w", errs.ErrUnsupported)
	}
	objs, err := r.vr.GetIndexer().ByIndex(IndexVRByClaim, claim.String())
	if err != nil {
		return VolumeReplicationRef{}, fmt.Errorf("kube: index VolumeReplication by claim %s: %w", claim, err)
	}
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		return VolumeReplicationRefFrom(u), nil
	}
	return VolumeReplicationRef{}, fmt.Errorf("kube: no VolumeReplication protects claim %s: %w", claim, errs.ErrNotFound)
}

// SyncVolumeReplicationForClaim returns the VolumeReplication protecting the PVC
// when it is synchronous, which is the per-PVC signal the node plugin uses to
// decide a volume follows its pod (design §6.6). A VolumeReplication is
// synchronous when its class carries method: sync.
//
// It returns errs.ErrNotFound when no VolumeReplication protects the PVC, when
// the one that does names an async class, or when that class cannot be resolved,
// all of which stage as an ordinary volume. It returns errs.ErrUnsupported when
// the resolver lacks the VolumeReplication or VolumeReplicationClass informer.
//
// The caller must gate a not-found on both informers having synced before
// treating the volume as plain: a not-found read from a cold cache would stage a
// sync volume with no promote and no gate, the wrong-site attach the gate exists
// to prevent (design §6.6, the fail-closed-until-synced rule).
func (r *InformerResolver) SyncVolumeReplicationForClaim(ctx context.Context, claim types.NamespacedName) (VolumeReplicationRef, error) {
	vr, err := r.VolumeReplicationForClaim(ctx, claim)
	if err != nil {
		// ErrNotFound (no VR, stages ordinarily) or ErrUnsupported (no informer).
		return VolumeReplicationRef{}, err
	}
	class, err := r.VolumeReplicationClassByName(ctx, vr.ClassName)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			// The VR names a class that does not resolve. It cannot assert sync, so
			// the volume stages ordinarily. On a sync cluster the caller's
			// informer-synced gate (design §6.6) keeps a not-yet-synced class from
			// being read as a genuinely absent one.
			return VolumeReplicationRef{}, fmt.Errorf(
				"kube: VolumeReplicationClass %q for claim %s does not resolve: %w",
				vr.ClassName, claim, errs.ErrNotFound)
		}
		return VolumeReplicationRef{}, err // ErrUnsupported or an index error.
	}
	if !class.IsSync() {
		return VolumeReplicationRef{}, fmt.Errorf(
			"kube: VolumeReplication for claim %s is not synchronous: %w", claim, errs.ErrNotFound)
	}
	return vr, nil
}
