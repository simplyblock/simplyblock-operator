package webhook

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/kube"
)

func vdoStorageClass(name string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: kube.DriverName,
		Parameters:  params,
	}
}

func vdoPVCRaw(t *testing.T, storageClass, size string) runtime.RawExtension {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-pvc", Namespace: "sb"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	raw, err := json.Marshal(pvc)
	if err != nil {
		t.Fatalf("marshal PVC: %v", err)
	}
	return runtime.RawExtension{Raw: raw}
}

func newVDOSizeValidator(t *testing.T, classes ...*storagev1.StorageClass) *VDOSizeFloorValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storagev1: %v", err)
	}
	objs := make([]client.Object, len(classes))
	for i, c := range classes {
		objs[i] = c
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &VDOSizeFloorValidator{Client: cl}
}

// TestVDOSizeFloorValidator. A class that asks for client-side compression or
// deduplication needs a volume VDO itself can hold; a plain class is never
// checked at all, regardless of size.
func TestVDOSizeFloorValidator(t *testing.T) {
	compressionSC := vdoStorageClass("compressed", map[string]string{kube.ParamClientCompression: "true"})
	dedupSC := vdoStorageClass("deduped", map[string]string{kube.ParamClientDeduplication: "true"})
	plainSC := vdoStorageClass("plain", map[string]string{})

	tests := []struct {
		name    string
		sc      string
		size    string
		allowed bool
	}{
		{"plain class tiny volume allowed", "plain", "1Gi", true},
		{"compression class below floor denied", "compressed", "4Gi", false},
		{"compression class at floor allowed", "compressed", "5Gi", true},
		{"compression class above floor allowed", "compressed", "100Gi", true},
		{"dedup class below floor denied", "deduped", "1Gi", false},
		{"dedup class at floor allowed", "deduped", "5Gi", true},
		{"unknown class allowed", "ghost", "1Gi", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newVDOSizeValidator(t, compressionSC, dedupSC, plainSC)
			req := admission.Request{}
			req.Object = vdoPVCRaw(t, tc.sc, tc.size)
			resp := v.Handle(context.Background(), req)
			if resp.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (msg: %s)", resp.Allowed, tc.allowed, resp.Result.Message)
			}
		})
	}
}

// TestVDOSizeFloorValidatorNoStorageClass. A PVC naming no class at all cannot
// be resolved to a provisioning parameter set, so there is nothing to check —
// the request is left to whatever validation Kubernetes itself applies.
func TestVDOSizeFloorValidatorNoStorageClass(t *testing.T) {
	v := newVDOSizeValidator(t)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-pvc", Namespace: "sb"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	raw, err := json.Marshal(pvc)
	if err != nil {
		t.Fatalf("marshal PVC: %v", err)
	}
	req := admission.Request{}
	req.Object = runtime.RawExtension{Raw: raw}
	resp := v.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("Allowed = false, want true when no storage class is set (msg: %s)", resp.Result.Message)
	}
}
