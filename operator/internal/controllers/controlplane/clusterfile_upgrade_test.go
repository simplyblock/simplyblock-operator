// The upgrade of a cluster-file mount that another field manager co-owns, run
// against a real apiserver because only one tracks field ownership. A Deployment
// the Helm chart created and the operator adopted keeps Helm as a co-owner of
// the old subPath mount, so server-side apply alone never removes it.

package controlplane

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// legacyClusterFileMounts is the web API Deployment with every cluster-file mount
// rewritten to the subPath
// mount the chart and earlier operators wrote.
func legacyClusterFileMounts(t *testing.T, cp *simplyblockv1alpha2.ControlPlane) *appsv1.Deployment {
	t.Helper()
	d := findDeployment(t, managementAPIObjects(cp), ComponentWebAPI).DeepCopy()
	for ci := range d.Spec.Template.Spec.Containers {
		for mi, m := range d.Spec.Template.Spec.Containers[ci].VolumeMounts {
			if m.Name == clusterFileVolume {
				d.Spec.Template.Spec.Containers[ci].VolumeMounts[mi] = corev1.VolumeMount{
					Name: clusterFileVolume, MountPath: clusterFilePath, SubPath: clusterFileName,
				}
			}
		}
	}
	return d
}

// Regression: 466-cluster-file-subpath — on an install handed over from the
// Helm chart, the subPath mount survived the switch to the directory mount, and
// the two together leave fdb.cluster an empty directory no client can read.
func TestAHelmCoOwnedSubPathMountIsReplacedByTheDirectoryMount(t *testing.T) {
	ctx := context.Background()
	apiClient := apiServer(t)
	cp := localControlPlane()
	cp.Namespace = freshNamespace(t, apiClient)
	cp.ResourceVersion = ""
	// The apiserver has no cert-manager kinds, and TLS has nothing to do with the mount.
	cp.Spec.Source.Local.TLS = simplyblockv1alpha2.ControlPlaneTLS{EnableTLS: ptr.To(false)}
	if err := apiClient.Create(ctx, cp); err != nil {
		t.Fatalf("create the control plane: %v", err)
	}

	legacy := legacyClusterFileMounts(t, cp)
	if err := apiClient.Create(ctx, legacy.DeepCopy(), client.FieldOwner("helm")); err != nil {
		t.Fatalf("create the chart's Deployment: %v", err)
	}
	if err := applyOne(ctx, apiClient, cp, scheme.Scheme, legacy); err != nil {
		t.Fatalf("adopt it as an earlier operator did: %v", err)
	}

	r := &ControlPlaneReconciler{Client: apiClient, Scheme: scheme.Scheme}
	if _, _, err := r.performInstallStep(ctx, cp, stepApplyingAPI); err != nil {
		t.Fatalf("apply the management API: %v", err)
	}

	d := &appsv1.Deployment{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(legacy), d); err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		var mounts []corev1.VolumeMount
		for _, m := range c.VolumeMounts {
			if m.Name == clusterFileVolume {
				mounts = append(mounts, m)
			}
		}
		if len(mounts) == 0 {
			continue
		}
		if len(mounts) != 1 || mounts[0].MountPath != clusterFileDir || mounts[0].SubPath != "" {
			t.Errorf("container %s mounts the cluster file as %+v, want only the directory %s",
				c.Name, mounts, clusterFileDir)
		}
	}
}

// A container that already carries the directory mount, next to an old one,
// loses the old one rather than gaining a second directory mount, and an old
// mount repeated is replaced once and removed after that.
func TestClusterFileMountOpsNeverLeaveTwoMounts(t *testing.T) {
	old := corev1.VolumeMount{Name: clusterFileVolume, MountPath: clusterFilePath, SubPath: clusterFileName}
	dir := corev1.VolumeMount{Name: clusterFileVolume, MountPath: clusterFileDir}
	other := corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}

	for _, tc := range []struct {
		name   string
		mounts []corev1.VolumeMount
		want   []string
	}{
		{"old only", []corev1.VolumeMount{other, old}, []string{"test 1", "replace 1"}},
		{"old beside the directory", []corev1.VolumeMount{old, dir}, []string{"test 0", "remove 0"}},
		{"old twice", []corev1.VolumeMount{old, other, old}, []string{"test 2", "remove 2", "test 0", "replace 0"}},
		{"directory only", []corev1.VolumeMount{dir, other}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := clusterFileMountOps("containers", []corev1.Container{{Name: "c", VolumeMounts: tc.mounts}})
			var got []string
			for _, op := range ops {
				got = append(got, op.Op+" "+op.Path[len(op.Path)-1:])
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ops = %v, want %v", got, tc.want)
			}
		})
	}
}
