package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func snRaw(t *testing.T, worker string) runtime.RawExtension {
	t.Helper()
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-1", Namespace: "default"},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{StorageNodeSetRef: "set", WorkerNode: worker},
	}
	b, err := json.Marshal(sn)
	if err != nil {
		t.Fatalf("marshal StorageNode: %v", err)
	}
	return runtime.RawExtension{Raw: b}
}

func TestStorageNodeValidator(t *testing.T) {
	const ns = "simplyblock-operator-system"
	v := &StorageNodeValidator{OperatorNamespace: ns}

	tests := []struct {
		name      string
		op        admissionv1.Operation
		oldWorker string
		newWorker string
		username  string
		allowed   bool
	}{
		{"non-update allowed", admissionv1.Create, "", "worker-2", "someone", true},
		{"workerNode unchanged allowed", admissionv1.Update, "worker-2", "worker-2", "kubernetes-admin", true},
		{"operator may repoint", admissionv1.Update, "worker-2", "worker-4", "system:serviceaccount:" + ns + ":simplyblock-operator", true},
		{"user may not repoint", admissionv1.Update, "worker-2", "worker-4", "kubernetes-admin", false},
		{"other-namespace SA may not repoint", admissionv1.Update, "worker-2", "worker-4", "system:serviceaccount:kube-system:foo", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: tc.op,
				Object:    snRaw(t, tc.newWorker),
				OldObject: snRaw(t, tc.oldWorker),
				UserInfo:  authenticationv1.UserInfo{Username: tc.username},
			}}
			resp := v.Handle(context.Background(), req)
			if resp.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, resp.Result.Message)
			}
		})
	}
}
