package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	vpvcCluster = "cluster-uuid"
	vpvcPool    = "pool-uuid"
	vpvcVolume  = "vol-uuid"
	vpvcNodeA   = "node-a"
	vpvcPVName  = "pv-data"
)

func vpvcAPIServer(t *testing.T, nodes []string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("[")
		for i, n := range nodes {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"` + n + `"}`)
		}
		b.WriteString("]")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func pvcRaw(t *testing.T, pinned string, bound bool) runtime.RawExtension {
	t.Helper()
	ann := map[string]string{}
	if pinned != "" {
		ann[kube.AnnoSelectedStorageNode] = pinned
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-pvc", Namespace: "sb", Annotations: ann},
	}
	if bound {
		pvc.Spec.VolumeName = vpvcPVName
	}
	raw, err := json.Marshal(pvc)
	if err != nil {
		t.Fatalf("marshal PVC: %v", err)
	}
	return runtime.RawExtension{Raw: raw}
}

func newPVCValidator(t *testing.T, apiURL string) *PersistentVolumeClaimValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: vpvcPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: vpvcCluster + ":" + vpvcPool + ":" + vpvcVolume},
			},
		},
	}
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "sb"},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: vpvcCluster},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, cluster).Build()
	return &PersistentVolumeClaimValidator{Client: cl, APIClient: webapi.NewClient(apiURL)}
}

func TestPVCValidator(t *testing.T) {
	api := vpvcAPIServer(t, []string{vpvcNodeA})

	tests := []struct {
		name    string
		newPin  string
		oldPin  string
		bound   bool
		allowed bool
	}{
		{"no annotation allowed", "", "", true, true},
		{"unchanged allowed", vpvcNodeA, vpvcNodeA, true, true},
		{"valid target bound allowed", vpvcNodeA, "", true, true},
		{"valid target unbound allowed", vpvcNodeA, "", false, true},
		{"invalid target bound denied", "ghost", "", true, false},
		{"invalid target unbound denied", "ghost", "", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newPVCValidator(t, api)
			req := admission.Request{}
			req.Object = pvcRaw(t, tc.newPin, tc.bound)
			if tc.oldPin != "" {
				req.OldObject = pvcRaw(t, tc.oldPin, tc.bound)
			}
			resp := v.Handle(context.Background(), req)
			if resp.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, resp.Result.Message)
			}
		})
	}
}
