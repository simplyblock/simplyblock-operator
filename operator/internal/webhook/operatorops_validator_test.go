// That a discovery run naming a size range nothing can read is refused at the
// request rather than accepted and quietly widened.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// discoverWith is a run carrying the device filter given.
func discoverWith(filter *simplyblockv1alpha2.DeviceFilter) *simplyblockv1alpha2.OperatorOps {
	return &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{Name: "oops-1", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action:   simplyblockv1alpha2.OperatorOpsActionDiscover,
			Discover: &simplyblockv1alpha2.DiscoverSpec{DeviceFilter: filter},
		},
	}
}

// reviewOf is the admission request for creating the run.
func reviewOf(t *testing.T, ops *simplyblockv1alpha2.OperatorOps) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("render the run: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func TestADriveSizeRangeNothingCanReadIsRefused(t *testing.T) {
	validator := &OperatorOpsValidator{}

	for _, spec := range []string{"2T-1T", "abc", "1X", "1T-2T-3T", "0100G", ""} {
		if spec == "" {
			continue
		}
		run := discoverWith(&simplyblockv1alpha2.DeviceFilter{DriveSizeRange: spec})
		response := validator.Handle(context.Background(), reviewOf(t, run))

		if response.Allowed {
			t.Errorf("a run whose driveSizeRange is %q was admitted", spec)
			continue
		}
		if !strings.Contains(response.Result.Message, "driveSizeRange") {
			t.Errorf("the refusal of %q does not name the field: %s", spec, response.Result.Message)
		}
	}
}

func TestADriveSizeRangeThatReadsIsAdmitted(t *testing.T) {
	validator := &OperatorOpsValidator{}

	for _, spec := range []string{"100G-2T", "500G-", "-2T", "1T", "2048", "1TiB-4TiB"} {
		run := discoverWith(&simplyblockv1alpha2.DeviceFilter{DriveSizeRange: spec})
		response := validator.Handle(context.Background(), reviewOf(t, run))

		if !response.Allowed {
			t.Errorf("a run whose driveSizeRange is %q was refused: %s",
				spec, response.Result.Message)
		}
	}
}

func TestARunWithNoRangeToReadIsAdmitted(t *testing.T) {
	validator := &OperatorOpsValidator{}

	for _, run := range []*simplyblockv1alpha2.OperatorOps{
		discoverWith(nil),
		discoverWith(&simplyblockv1alpha2.DeviceFilter{}),
		discoverWith(&simplyblockv1alpha2.DeviceFilter{EnableLogicalBlockDevices: ptr.To(true)}),
		{
			ObjectMeta: metav1.ObjectMeta{Name: "oops-2", Namespace: "simplyblock"},
			Spec: simplyblockv1alpha2.OperatorOpsSpec{
				Action: simplyblockv1alpha2.OperatorOpsActionDiscover,
			},
		},
	} {
		if response := validator.Handle(context.Background(), reviewOf(t, run)); !response.Allowed {
			t.Errorf("a run naming no size range was refused: %s", response.Result.Message)
		}
	}
}

func TestTheRefusalSaysWhatARangeLooksLike(t *testing.T) {
	// A reviewer whose filter was refused needs the form, not only the verdict.
	validator := &OperatorOpsValidator{}
	run := discoverWith(&simplyblockv1alpha2.DeviceFilter{DriveSizeRange: "2T-1T"})

	response := validator.Handle(context.Background(), reviewOf(t, run))

	for _, fragment := range []string{"100G-2T", "500G-", "-2T"} {
		if !strings.Contains(response.Result.Message, fragment) {
			t.Errorf("the refusal does not show the %s form: %s", fragment, response.Result.Message)
		}
	}
}

func TestAnObjectThatDoesNotDecodeIsAnError(t *testing.T) {
	validator := &OperatorOpsValidator{}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: []byte("{ not an object")},
	}}

	if response := validator.Handle(context.Background(), request); response.Allowed {
		t.Error("an object that does not decode was admitted")
	}
}
