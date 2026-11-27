// Unit tests for the VolumeGroupSnapshotOps admission webhook (design §7.4,
// test plan U-33): a volumeGroupSnapshotRef that resolves is admitted, and one
// naming no VolumeGroupSnapshot in the namespace is rejected at create.
package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	volumegroupsnapshotv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumegroupsnapshot/v1beta1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/groupsnapshot"
	"github.com/simplyblock/simplyblock-operator/internal/groupsnapshot/test"
)

func opsCreateRequest(t *testing.T, ops *simplyblockv1alpha2.VolumeGroupSnapshotOps) admission.Request {
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

// The reference resolves the same way whichever version the cluster serves the
// kinds at: v1beta1 is the external-snapshotter 8.2 CRDs, v1 is OpenShift 5,
// where a typed v1beta1 read found nothing and refused every restore
// (GitHub #978).
func TestVolumeGroupSnapshotOpsValidator_RefResolution(t *testing.T) {
	for _, version := range []string{"v1beta1", "v1"} {
		t.Run(version, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := volumegroupsnapshotv1beta1.AddToScheme(scheme); err != nil {
				t.Fatalf("add scheme: %v", err)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithRESTMapper(test.Mapper(version)).
				WithObjects(vgsAt(version, "default", "vgs1")).Build()
			validator := &VolumeGroupSnapshotOpsValidator{Client: cl}

			ops := &simplyblockv1alpha2.VolumeGroupSnapshotOps{
				ObjectMeta: metav1.ObjectMeta{Name: "op1", Namespace: "default"},
				Spec: simplyblockv1alpha2.VolumeGroupSnapshotOpsSpec{
					VolumeGroupSnapshotRef: "vgs1",
					Action:                 simplyblockv1alpha2.VolumeGroupSnapshotOpsActionRestore,
				},
			}
			if resp := validator.Handle(context.Background(), opsCreateRequest(t, ops)); !resp.Allowed {
				t.Errorf("a resolvable ref was denied: %v", resp.Result)
			}

			ops.Spec.VolumeGroupSnapshotRef = "no-such-vgs"
			if resp := validator.Handle(context.Background(), opsCreateRequest(t, ops)); resp.Allowed {
				t.Error("an unresolvable ref was admitted")
			}
		})
	}
}

// A cluster that serves no version of the group has no VolumeGroupSnapshot to
// restore, and says so as a denial rather than as a server error.
func TestVolumeGroupSnapshotOpsValidator_NoGroupSnapshotAPI(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	validator := &VolumeGroupSnapshotOpsValidator{Client: cl}

	ops := &simplyblockv1alpha2.VolumeGroupSnapshotOps{
		ObjectMeta: metav1.ObjectMeta{Name: "op1", Namespace: "default"},
		Spec: simplyblockv1alpha2.VolumeGroupSnapshotOpsSpec{
			VolumeGroupSnapshotRef: "vgs1",
			Action:                 simplyblockv1alpha2.VolumeGroupSnapshotOpsActionRestore,
		},
	}
	resp := validator.Handle(context.Background(), opsCreateRequest(t, ops))
	if resp.Allowed || resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Errorf("response = %+v, want a denial", resp.Result)
	}
}

// vgsAt is a VolumeGroupSnapshot stored at version. The client library this
// module pins has no v1 type, so a v1 object is written unstructured.
func vgsAt(version, namespace, name string) client.Object {
	if version == "v1beta1" {
		return &volumegroupsnapshotv1beta1.VolumeGroupSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		}
	}
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(groupsnapshot.Group + "/" + version)
	u.SetKind(groupsnapshot.KindVolumeGroupSnapshot)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}
