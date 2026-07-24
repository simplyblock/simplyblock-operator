package autoplacement

import (
	"context"
	"testing"

	atlaskube "github.com/simplyblock/atlas/kube"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func namespacedTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := storagev1.AddToScheme(s); err != nil {
		t.Fatalf("storagev1 scheme: %v", err)
	}
	return s
}

func csiPV(name, handle, scName string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: scName,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       utils.CSIProvisioner,
					VolumeHandle: handle,
				},
			},
		},
	}
}

func namespacedSC(name string, maxNS string) *storagev1.StorageClass {
	params := map[string]string{}
	if maxNS != "" {
		params[atlaskube.ParamMaxNamespacePerSubsys] = maxNS
	}
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: atlaskube.DriverName,
		Parameters:  params,
	}
}

// TestBuildNamespacedSet verifies that only volumes whose StorageClass sets
// max_namespace_per_subsys > 1 are reported, scoped to the requested cluster,
// and that an unresolvable StorageClass fails open (volume not excluded).
func TestBuildNamespacedSet(t *testing.T) {
	scheme := namespacedTestScheme(t)

	// PVs live in the controller-runtime (cache-like) client, listed via the CSI
	// driver field index — exactly as in the operator.
	pvs := []client.Object{
		csiPV("pv-multi", "cluster-a:pool:vol-multi", "sc-multi"),
		csiPV("pv-single", "cluster-a:pool:vol-single", "sc-single"),
		csiPV("pv-default", "cluster-a:pool:vol-default", "sc-default"),
		csiPV("pv-othercluster", "cluster-b:pool:vol-other", "sc-multi"),
		csiPV("pv-missingsc", "cluster-a:pool:vol-missingsc", "sc-gone"),
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pvs...).
		WithIndex(&corev1.PersistentVolume{}, PVCSIDriverIndexField, func(o client.Object) []string {
			pv, ok := o.(*corev1.PersistentVolume)
			if !ok || pv.Spec.CSI == nil {
				return nil
			}
			return []string{pv.Spec.CSI.Driver}
		}).
		Build()

	// StorageClasses are resolved through a kube.LiveResolver over a client-go
	// clientset — the same path the operator uses. sc-gone is intentionally
	// absent so its volume exercises the fail-open branch.
	resolver := atlaskube.NewLiveResolver(kfake.NewSimpleClientset(
		namespacedSC("sc-multi", "32"), // multi-namespace
		namespacedSC("sc-single", "1"), // single
		namespacedSC("sc-default", ""), // default (1)
	))

	lvs := NewLogicalVolumeSelector(nil, cl, resolver)

	got, err := lvs.BuildNamespacedSet(context.Background(), "cluster-a")
	if err != nil {
		t.Fatalf("BuildNamespacedSet: %v", err)
	}

	if !got["vol-multi"] {
		t.Error("vol-multi should be reported as namespaced")
	}
	if got["vol-single"] || got["vol-default"] {
		t.Errorf("single/default volumes must not be namespaced: %v", got)
	}
	if got["vol-other"] {
		t.Error("vol-other belongs to cluster-b and must be excluded from a cluster-a scan")
	}
	if got["vol-missingsc"] {
		t.Error("volume with unresolvable StorageClass must fail open (not namespaced)")
	}
	if len(got) != 1 {
		t.Errorf("expected exactly 1 namespaced volume, got %d: %v", len(got), got)
	}
}
