// The OperatorOps guard: a validating webhook that refuses a discovery run
// whose device filter names a size range nothing can read.
//
// The filter is the one part of a run that is a rule rather than a value, and
// the size range is the one part of the filter that has to be parsed. A range
// the parser cannot read is not a range that admits nothing: the pipeline
// builds no size rule at all, so the filter silently widens to every disk on
// every worker, and the draft a reviewer approves is the fleet's whole storage
// rather than the slice they asked for.
//
// Nothing downstream can catch it. The refusal has no device to attach itself
// to, so it reaches neither the refusal list nor the run's explanation, and the
// draft that results is indistinguishable from one written by a run that meant
// to take everything.
//
// Admission is therefore where it belongs, and it is where the parser's own
// caller already assumed it was: the comment in BasicDeviceRules says the spec
// is validated by the caller and the failure is reported separately, which was
// true of nothing until this existed.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/discovery"
)

// +kubebuilder:webhook:path=/validate-storage-simplyblock-io-v1alpha2-operatorops,mutating=false,failurePolicy=fail,sideEffects=None,groups=storage.simplyblock.io,resources=operatorops,verbs=create;update,versions=v1alpha2,name=voperatorops.simplyblock.io,admissionReviewVersions=v1

// OperatorOpsValidator refuses a run whose filter cannot be read.
//
// failurePolicy=Fail, because the webhook server runs inside the operator pod:
// its availability tracks the operator's, and a run admitted while the operator
// is down is a run nothing would perform anyway.
//
// It holds no client. The only thing the decision rests on is the object being
// admitted, which is what makes the answer the same on every replica.
type OperatorOpsValidator struct{}

// Handle refuses a create or an update whose device filter does not parse.
//
// Update is guarded as well as create, because the filter is not immutable and
// a run edited before it reaches Probing is a run whose filter still decides
// what the draft holds.
func (v *OperatorOpsValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}
	if len(req.Object.Raw) == 0 {
		return admission.Allowed("")
	}

	var ops simplyblockv1alpha2.OperatorOps
	if err := json.Unmarshal(req.Object.Raw, &ops); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if refusal := unreadableSizeRange(ops.Spec.Discover); refusal != "" {
		return admission.Denied(refusal)
	}
	return admission.Allowed("")
}

// unreadableSizeRange is the refusal for a size range that does not parse, and
// the empty string for a run that names none or names one that does.
func unreadableSizeRange(spec *simplyblockv1alpha2.DiscoverSpec) string {
	if spec == nil || spec.DeviceFilter == nil || spec.DeviceFilter.DriveSizeRange == "" {
		return ""
	}

	rangeSpec := spec.DeviceFilter.DriveSizeRange
	if _, _, err := discovery.ParseSizeRange(rangeSpec); err != nil {
		return fmt.Sprintf(
			"spec.discover.deviceFilter.driveSizeRange is %q, which cannot be read: %v. "+
				"A run whose range cannot be read applies no size filter at all, so the draft "+
				"would name every disk on every worker rather than the ones asked for. "+
				"Write a range as 100G-2T, as 500G- for no upper bound, as -2T for no lower "+
				"one, or as a single size such as 1T, which means exactly that size.",
			rangeSpec, err)
	}
	return ""
}
