// The join between a control-plane volume and the Kubernetes claim it backs.
//
// It is a file of its own because it is the only part of this package with a
// correctness question in it. The REST surface next door is plumbing that fails
// loudly when it is wrong; a wrong join fails silently, reporting one tenant's
// capacity under another tenant's claim name. So the rule it implements is
// stated once, here, and both directions of the lookup obey it:
//
//	a reading is served for a volume if and only if a simplyblock-provisioned
//	PersistentVolume carries that volume's handle and is bound to a claim.
//
// Neither half is optional. Without the driver check, another vendor's volume
// whose handle happened to collide would be answered for. Without the claim,
// there is no namespace to authorize the read against, which is the whole basis
// of the tenant isolation this API offers.
//
// The two directions exist because they are asked differently. A list walks the
// volume cache and needs a handle-to-claim lookup, which is what the shared
// kube.IndexPVByVolumeHandle index on the manager's cache resolves. A get already
// knows the claim and walks the other way, claim to volume, which is two cache
// reads rather than a scan of every volume.

package metricsapi

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"
)

// binding is a volume's Kubernetes identity: the claim that gives it a name and
// a namespace, and the volume that carries the handle between them.
type binding struct {
	handle           lvol.VolumeHandle
	persistentVolume string
	claim            types.NamespacedName
	// labels are the claim's, copied onto the served object so that a label
	// selector has something to match. The object has no labels of its own.
	labels map[string]string
	// created is the claim's creation timestamp, which is what the served
	// object's Age column shows. The reading itself has no age worth printing:
	// it is always the last sample, and Timestamp says how old that sample is.
	created metav1.Time
}

// bindingForHandle resolves a control-plane volume to the claim it backs, or
// reports ok=false when it backs none. A volume with no PersistentVolume was not
// provisioned through this cluster, and one whose PersistentVolume is unbound is
// not yet anybody's.
func bindingForHandle(ctx context.Context, reader client.Reader, handle lvol.VolumeHandle) (binding, bool, error) {
	var pvs corev1.PersistentVolumeList
	if err := reader.List(ctx, &pvs, client.MatchingFields{kube.IndexPVByVolumeHandle: string(handle)}); err != nil {
		return binding{}, false, fmt.Errorf("look up the persistent volume for handle %s: %w", handle, err)
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		// The index only keys simplyblock-provisioned volumes, so a hit is
		// already ours; the check is repeated because the invariant is this
		// file's and not the index's to keep.
		if !kube.IsManaged(pv) || pv.Spec.ClaimRef == nil {
			continue
		}
		claim := types.NamespacedName{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name}
		if claim.Namespace == "" || claim.Name == "" {
			continue
		}
		labels, created := claimMeta(ctx, reader, claim)
		return binding{
			handle:           handle,
			persistentVolume: pv.Name,
			claim:            claim,
			labels:           labels,
			created:          created,
		}, true, nil
	}
	return binding{}, false, nil
}

// bindingForClaim resolves a claim to the control-plane volume behind it. It
// reports ok=false for a claim that does not exist, is not bound, or is bound to
// another driver's volume. All three are the same answer to a client:
// this API has no reading for that name.
func bindingForClaim(ctx context.Context, reader client.Reader, claim types.NamespacedName) (binding, bool, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, claim, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return binding{}, false, nil
		}
		return binding{}, false, fmt.Errorf("get claim %s: %w", claim, err)
	}
	if pvc.Spec.VolumeName == "" {
		return binding{}, false, nil // pending, so there is nothing to measure yet
	}

	var pv corev1.PersistentVolume
	if err := reader.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return binding{}, false, nil
		}
		return binding{}, false, fmt.Errorf("get persistent volume %s: %w", pvc.Spec.VolumeName, err)
	}
	if !kube.IsManaged(&pv) {
		return binding{}, false, nil
	}

	return binding{
		handle:           lvol.VolumeHandle(pv.Spec.CSI.VolumeHandle),
		persistentVolume: pv.Name,
		claim:            claim,
		labels:           pvc.Labels,
		created:          pvc.CreationTimestamp,
	}, true, nil
}

// claimMeta reads the claim's labels and creation time for the list path, which
// reached the claim through its PersistentVolume and so has not read it yet. A
// claim that has since been deleted yields zero values rather than an error: the
// capacity reading is still correct, and only a selector or the Age column would
// be affected.
func claimMeta(ctx context.Context, reader client.Reader, claim types.NamespacedName) (map[string]string, metav1.Time) {
	var pvc corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, claim, &pvc); err != nil {
		return nil, metav1.Time{}
	}
	return pvc.Labels, pvc.CreationTimestamp
}
