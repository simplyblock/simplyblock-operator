package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha1-storagecluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=storageclusters,verbs=create;update,versions=v1alpha1,name=vstoragecluster.simplyblock.io,admissionReviewVersions=v1

// StorageClusterValidator is a validating admission webhook that rejects a
// StorageCluster whose spec.checkSumValidation.atomic4k is true while
// inlineChecksum is not. atomic4k only has meaning as an escape hatch for the
// inline-checksum fallback path (it tells the data plane to skip its own
// >=4K block-size gate for devices like AWS NVMe); with checksum validation
// off it configures nothing on the backend and just misleads readers of the
// CR into thinking 4K-atomic handling is active.
//
// Both fields are otherwise immutable once spec.checkSumValidation is set
// (enforced by a CEL rule on StorageClusterSpec), so this only needs to catch
// the combination at create/first-set time, not guard against later drift.
type StorageClusterValidator struct{}

func (v *StorageClusterValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	var sc simplyblockv1alpha1.StorageCluster
	if err := json.Unmarshal(req.Object.Raw, &sc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	csv := sc.Spec.CheckSumValidation
	if csv == nil {
		return admission.Allowed("")
	}
	if ptr.BoolFromOrFalse(csv.Atomic4k) && !ptr.BoolFromOrFalse(csv.InlineChecksum) {
		return admission.Denied(
			"spec.checkSumValidation.atomic4k requires spec.checkSumValidation.inlineChecksum to be true")
	}
	return admission.Allowed("")
}
