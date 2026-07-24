package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:webhook:path=/validate-v1-pvc-pinned-volume,mutating=false,failurePolicy=fail,sideEffects=None,groups="",resources=persistentvolumeclaims,verbs=create;update,versions=v1,name=pinned-volume-validator.simplyblock.io,admissionReviewVersions=v1

// PersistentVolumeClaimValidator is a validating admission webhook that rejects a
// change to the simplyblock.io/pinned-volume annotation whose new value is not a
// known storage-node UUID. The annotation pins a volume's backing logical volume
// to a specific storage node; an unknown UUID would otherwise be accepted and
// only surface later as a failed migration, so it is rejected at write time.
//
// The webhook is scoped (via matchConditions in the WebhookConfiguration) to only
// fire for PVCs that carry the annotation, so failurePolicy=Fail cannot block
// unrelated PVC writes when the operator is unavailable.
type PersistentVolumeClaimValidator struct {
	Client    client.Client
	APIClient *webapi.Client
}

func (v *PersistentVolumeClaimValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("pvc", req.Name, "namespace", req.Namespace)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := json.Unmarshal(req.Object.Raw, pvc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	desired := pvc.Annotations[simplyblockv1alpha1.AnnotationSelectedStorageNode]

	// Clearing/omitting the annotation (unpin) is always allowed.
	if desired == "" {
		return admission.Allowed("pinned-volume annotation not set")
	}

	// On update, only validate when the value actually changes — an unchanged
	// value must not start failing just because the storage node it names has
	// since been removed, and it spares the control-plane API an extra call.
	if len(req.OldObject.Raw) > 0 {
		oldPVC := &corev1.PersistentVolumeClaim{}
		if err := json.Unmarshal(req.OldObject.Raw, oldPVC); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if oldPVC.Annotations[simplyblockv1alpha1.AnnotationSelectedStorageNode] == desired {
			return admission.Allowed("pinned-volume annotation unchanged")
		}
	}

	ok, err := v.isKnownStorageNode(ctx, pvc, desired)
	if err != nil {
		// A lookup failure must not silently admit an unvalidated value; with
		// failurePolicy=Fail the API server already treats webhook errors as a
		// denial, so surface the reason.
		log.Error(err, "cannot validate pinned-volume target", "target", desired)
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("validate pinned-volume target %q: %w", desired, err))
	}
	if !ok {
		return admission.Denied(fmt.Sprintf(
			"pinned-volume target %q is not a known storage node UUID", desired))
	}
	return admission.Allowed("pinned-volume target is a known storage node")
}

// isKnownStorageNode reports whether nodeUUID is a storage node of the cluster
// backing the PVC. When the PVC is already bound the cluster is resolved from the
// bound PV's CSI volume handle; otherwise (the PV is not provisioned yet) the
// node is looked up across every StorageCluster known to the operator.
func (v *PersistentVolumeClaimValidator) isKnownStorageNode(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	nodeUUID string,
) (bool, error) {
	if pvc.Spec.VolumeName != "" {
		clusterUUID, err := v.clusterUUIDForPV(ctx, pvc.Spec.VolumeName)
		if err != nil {
			return false, err
		}
		if clusterUUID != "" {
			return v.nodeInCluster(ctx, clusterUUID, nodeUUID)
		}
		// Bound PV without a simplyblock CSI handle: fall through to the
		// cluster-wide scan rather than failing outright.
	}
	return v.nodeInAnyCluster(ctx, nodeUUID)
}

// clusterUUIDForPV returns the storage cluster UUID encoded in the bound PV's CSI
// volume handle ("<clusterUUID>:<poolUUID>:<volumeUUID>"), or "" when the PV is
// not a simplyblock CSI volume.
func (v *PersistentVolumeClaimValidator) clusterUUIDForPV(ctx context.Context, pvName string) (string, error) {
	pv := &corev1.PersistentVolume{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		return "", fmt.Errorf("get PV %q: %w", pvName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return "", nil
	}
	parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
	if len(parts) != 3 || parts[0] == "" {
		return "", nil
	}
	return parts[0], nil
}

// nodeInCluster reports whether nodeUUID is a storage node of clusterUUID.
func (v *PersistentVolumeClaimValidator) nodeInCluster(ctx context.Context, clusterUUID, nodeUUID string) (bool, error) {
	nodes, err := v.APIClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		return false, fmt.Errorf("list storage nodes for cluster %q: %w", clusterUUID, err)
	}
	for _, n := range nodes {
		if n.UUID == nodeUUID {
			return true, nil
		}
	}
	return false, nil
}

// nodeInAnyCluster reports whether nodeUUID is a storage node of any cluster the
// operator manages. Used when the PVC is not yet bound and the cluster cannot be
// resolved from a PV.
func (v *PersistentVolumeClaimValidator) nodeInAnyCluster(ctx context.Context, nodeUUID string) (bool, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := v.Client.List(ctx, &clusters); err != nil {
		return false, fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID == "" {
			continue
		}
		ok, err := v.nodeInCluster(ctx, cr.Status.UUID, nodeUUID)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
