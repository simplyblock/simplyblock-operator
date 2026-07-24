package kube

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/errs"
)

func sc(name string, provisioner string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
		Parameters:  params,
	}
}

func TestPropertiesFromStorageClass(t *testing.T) {
	props, err := PropertiesFromStorageClass(sc("full", DriverName, map[string]string{
		ParamPool:                  "pool-a",
		ParamFabric:                "tcp",
		ParamClusterID:             "cluster-1",
		ParamMaxSize:               "10G",
		ParamLvolPriorityClass:     "3",
		ParamMaxNamespacePerSubsys: "32",
		ParamCompression:           "true",
		ParamEncryption:            "true",
		ParamReplicate:             "false",
		ParamQoSRWIOPS:             "1000",
		ParamQoSWMBytes:            "50",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if props.Pool != "pool-a" || props.Fabric != "tcp" || props.ClusterID != "cluster-1" || props.MaxSize != "10G" {
		t.Errorf("string fields wrong: %+v", props)
	}
	if props.LvolPriorityClass != 3 || props.MaxNamespacePerSubsys != 32 {
		t.Errorf("int fields wrong: %+v", props)
	}
	if !props.Compression || !props.Encryption || props.Replicate {
		t.Errorf("bool fields wrong: %+v", props)
	}
	if props.QoS.RWIOPS != 1000 || props.QoS.WMBytes != 50 || props.QoS.RWMBytes != 0 {
		t.Errorf("qos wrong: %+v", props.QoS)
	}
	if !props.IsMultiNamespace() {
		t.Error("IsMultiNamespace() = false, want true for max_namespace_per_subsys=32")
	}
}

func TestPropertiesFromStorageClass_Defaults(t *testing.T) {
	// An empty simplyblock StorageClass yields zero-valued defaults and, crucially,
	// MaxNamespacePerSubsys defaults to 1 (single-namespace / not multi-namespace).
	props, err := PropertiesFromStorageClass(sc("bare", DriverName, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if props.MaxNamespacePerSubsys != 1 {
		t.Errorf("MaxNamespacePerSubsys = %d, want 1", props.MaxNamespacePerSubsys)
	}
	if props.IsMultiNamespace() {
		t.Error("IsMultiNamespace() = true, want false when parameter absent")
	}
}

func TestPropertiesFromStorageClass_MultiNamespaceBoundary(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"", false}, {"1", false}, {"2", true}, {"32", true}} {
		props, err := PropertiesFromStorageClass(sc("b", DriverName, map[string]string{ParamMaxNamespacePerSubsys: tc.val}))
		if err != nil {
			t.Fatalf("val %q: unexpected error: %v", tc.val, err)
		}
		if got := props.IsMultiNamespace(); got != tc.want {
			t.Errorf("val %q: IsMultiNamespace() = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestPropertiesFromStorageClass_NotManaged(t *testing.T) {
	if _, err := PropertiesFromStorageClass(nil); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("nil sc: err = %v, want ErrUnsupported", err)
	}
	if _, err := PropertiesFromStorageClass(sc("foreign", "other.csi.driver", nil)); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("foreign provisioner: err = %v, want ErrUnsupported", err)
	}
}

func TestPropertiesFromStorageClass_MalformedInt(t *testing.T) {
	if _, err := PropertiesFromStorageClass(sc("bad", DriverName, map[string]string{ParamMaxNamespacePerSubsys: "notanint"})); err == nil {
		t.Error("expected error for malformed max_namespace_per_subsys, got nil")
	}
}

func TestStorageClassNameFromPV(t *testing.T) {
	pv := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{StorageClassName: "sc-a"}}
	if name, ok := StorageClassNameFromPV(pv); !ok || name != "sc-a" {
		t.Errorf("spec: got (%q, %v), want (sc-a, true)", name, ok)
	}

	legacy := &corev1.PersistentVolume{}
	legacy.Annotations = map[string]string{"volume.beta.kubernetes.io/storage-class": "sc-legacy"}
	if name, ok := StorageClassNameFromPV(legacy); !ok || name != "sc-legacy" {
		t.Errorf("legacy: got (%q, %v), want (sc-legacy, true)", name, ok)
	}

	if _, ok := StorageClassNameFromPV(&corev1.PersistentVolume{}); ok {
		t.Error("empty PV: ok = true, want false")
	}
}

func managedPV(handle, scName string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: scName,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: handle},
			},
		},
	}
}

func TestResolvePropertiesForPV(t *testing.T) {
	r := NewLiveResolver(kfake.NewSimpleClientset(
		sc("ns-class", DriverName, map[string]string{ParamMaxNamespacePerSubsys: "16"}),
	))

	props, err := ResolvePropertiesForPV(context.Background(), r, managedPV("c:p:v", "ns-class"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !props.IsMultiNamespace() {
		t.Error("expected multi-namespace volume")
	}

	// Unmanaged PV → ErrUnsupported.
	plain := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{StorageClassName: "ns-class"}}
	if _, err := ResolvePropertiesForPV(context.Background(), r, plain); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("unmanaged PV: err = %v, want ErrUnsupported", err)
	}

	// Managed PV naming an unknown class → ErrNotFound propagated.
	if _, err := ResolvePropertiesForPV(context.Background(), r, managedPV("c:p:v", "missing")); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing class: err = %v, want ErrNotFound", err)
	}
}
