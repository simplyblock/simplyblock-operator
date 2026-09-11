// Unit tests for the VolumeGroupSnapshotOps admission webhook (design §7.4,
// test plan U-33): a volumeGroupSnapshotRef that resolves is admitted, and one
// naming no VolumeGroupSnapshot in the namespace is rejected at create.
package webhook

import (
	"context"
	"encoding/json"
	"testing"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func opsCreateRequest(t *testing.T, ops *simplyblockv1alpha1.VolumeGroupSnapshotOps) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: ops.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func TestVolumeGroupSnapshotOpsValidator_RefResolution(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := volumegroupsnapshotv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&volumegroupsnapshotv1beta1.VolumeGroupSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "vgs1", Namespace: "default"},
		},
	).Build()
	validator := &VolumeGroupSnapshotOpsValidator{Client: cl}

	ops := &simplyblockv1alpha1.VolumeGroupSnapshotOps{
		ObjectMeta: metav1.ObjectMeta{Name: "op1", Namespace: "default"},
		Spec: simplyblockv1alpha1.VolumeGroupSnapshotOpsSpec{
			VolumeGroupSnapshotRef: "vgs1",
			Action:                 simplyblockv1alpha1.VolumeGroupSnapshotOpsActionRestore,
		},
	}
	if resp := validator.Handle(context.Background(), opsCreateRequest(t, ops)); !resp.Allowed {
		t.Errorf("a resolvable ref was denied: %v", resp.Result)
	}

	ops.Spec.VolumeGroupSnapshotRef = "no-such-vgs"
	if resp := validator.Handle(context.Background(), opsCreateRequest(t, ops)); resp.Allowed {
		t.Error("an unresolvable ref was admitted")
	}
}
