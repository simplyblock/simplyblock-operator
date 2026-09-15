// volumegroupsnapshotops_validator.go rejects, at kubectl apply, a
// VolumeGroupSnapshotOps whose volumeGroupSnapshotRef does not resolve
// (design-consistency-groups.md §7.4): the reference is immutable, so an
// unresolvable one would park the object in a terminal error for its whole
// life. Readiness is deliberately not checked here: a target that exists but
// is not yet ReadyToUse is the controller's to wait on.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha1-volumegroupsnapshotops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=volumegroupsnapshotops,verbs=create,versions=v1alpha1,name=vvolumegroupsnapshotops.simplyblock.io,admissionReviewVersions=v1

// VolumeGroupSnapshotOpsValidator resolves spec.volumeGroupSnapshotRef on
// create and refuses the object when no such VolumeGroupSnapshot exists in the
// namespace.
type VolumeGroupSnapshotOpsValidator struct {
	Client client.Client
}

func (v *VolumeGroupSnapshotOpsValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha1.VolumeGroupSnapshotOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var vgs volumegroupsnapshotv1beta1.VolumeGroupSnapshot
	err := v.Client.Get(ctx, types.NamespacedName{
		Name: ops.Spec.VolumeGroupSnapshotRef, Namespace: ops.Namespace,
	}, &vgs)
	if apierrors.IsNotFound(err) {
		return admission.Denied(fmt.Sprintf(
			"spec.volumeGroupSnapshotRef %q does not name a VolumeGroupSnapshot in namespace %q",
			ops.Spec.VolumeGroupSnapshotRef, ops.Namespace))
	}
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.Allowed("volumeGroupSnapshotRef resolves")
}
