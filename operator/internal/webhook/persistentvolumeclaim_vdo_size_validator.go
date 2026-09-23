// A validating admission webhook that rejects a PVC asking for client-side
// compression or deduplication (VDO) below the volume size VDO itself
// requires, so the failure surfaces at admission time instead of later as an
// lvcreate error inside NodeStageVolume
// (design-issue-277-client-side-compression.md, P0-7/§14 Q2).
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"
)

// vdoMinimumVolumeSize is the floor VDO itself enforces (roughly 4.72GiB),
// rounded up to a size an operator can reason about; anything smaller fails
// inside lvcreate with no VDO stack left behind.
var vdoMinimumVolumeSize = resource.MustParse("5Gi")

// +kubebuilder:webhook:path=/validate-v1-pvc-vdo-size,mutating=false,failurePolicy=Ignore,sideEffects=None,groups="",resources=persistentvolumeclaims,verbs=create,versions=v1,name=vdo-size-floor-validator.simplyblock.io,admissionReviewVersions=v1
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// VDOSizeFloorValidator rejects a PVC below vdoMinimumVolumeSize when the
// StorageClass it names asks for client-side compression or deduplication.
//
// failurePolicy=Ignore, unlike the pin validator's Fail: a lookup failure
// here should fall back to today's behavior — a late lvcreate failure — not
// block every PVC creation in the cluster while the operator is unavailable.
// The same reasoning applies inside Handle itself: a StorageClass that
// cannot be resolved, or that PropertiesFromStorageClass rejects as not this
// driver's, is allowed through rather than denied, since there is nothing
// this webhook can say about a class it cannot read.
type VDOSizeFloorValidator struct {
	Client client.Client
}

func (v *VDOSizeFloorValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("pvc", req.Name, "namespace", req.Namespace)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := json.Unmarshal(req.Object.Raw, pvc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return admission.Allowed("no storage class: nothing to check")
	}

	sc := &storagev1.StorageClass{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		log.V(1).Info("cannot resolve storage class, allowing",
			"storageClass", *pvc.Spec.StorageClassName, "err", err)
		return admission.Allowed("storage class not resolvable: nothing to check")
	}

	props, err := kube.PropertiesFromStorageClass(sc)
	if err != nil {
		return admission.Allowed("not a simplyblock storage class: nothing to check")
	}
	if !props.WantsVDO() {
		return admission.Allowed("volume does not use client-side compression or deduplication")
	}

	requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if requested.Cmp(vdoMinimumVolumeSize) < 0 {
		return admission.Denied(fmt.Sprintf(
			"client-side compression/deduplication needs at least %s, but %q requested %s",
			vdoMinimumVolumeSize.String(), pvc.Name, requested.String()))
	}
	return admission.Allowed("volume size satisfies the client-side compression/deduplication floor")
}
