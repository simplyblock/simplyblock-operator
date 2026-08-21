package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func scRaw(t *testing.T, csv *simplyblockv1alpha1.CheckSumValidationSpec) runtime.RawExtension {
	t.Helper()
	sc := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{CheckSumValidation: csv},
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal StorageCluster: %v", err)
	}
	return runtime.RawExtension{Raw: b}
}

func TestStorageClusterValidator(t *testing.T) {
	v := &StorageClusterValidator{}

	tests := []struct {
		name    string
		csv     *simplyblockv1alpha1.CheckSumValidationSpec
		allowed bool
	}{
		{"checkSumValidation unset allowed", nil, true},
		{"both false allowed", &simplyblockv1alpha1.CheckSumValidationSpec{
			InlineChecksum: ptr.To(false), Atomic4k: ptr.To(false),
		}, true},
		{"inlineChecksum alone allowed", &simplyblockv1alpha1.CheckSumValidationSpec{
			InlineChecksum: ptr.To(true), Atomic4k: ptr.To(false),
		}, true},
		{"both true allowed", &simplyblockv1alpha1.CheckSumValidationSpec{
			InlineChecksum: ptr.To(true), Atomic4k: ptr.To(true),
		}, true},
		{"atomic4k without inlineChecksum denied", &simplyblockv1alpha1.CheckSumValidationSpec{
			InlineChecksum: ptr.To(false), Atomic4k: ptr.To(true),
		}, false},
		{"atomic4k with nil inlineChecksum denied", &simplyblockv1alpha1.CheckSumValidationSpec{
			Atomic4k: ptr.To(true),
		}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Object:    scRaw(t, tc.csv),
			}}
			resp := v.Handle(context.Background(), req)
			if resp.Allowed != tc.allowed {
				t.Fatalf("expected allowed=%v, got allowed=%v (result=%+v)", tc.allowed, resp.Allowed, resp.Result)
			}
		})
	}
}
