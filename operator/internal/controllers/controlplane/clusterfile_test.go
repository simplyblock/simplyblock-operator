// The tests of how the control plane mounts the FoundationDB cluster file: as a
// directory the kubelet refreshes, and without rolling a workload when the
// coordinators move.

package controlplane

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func clusterFileConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: clusterFileConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{clusterFileKey: data},
	}
}

func TestNoWorkloadCarriesAReloaderAnnotation(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp, clusterFileConfigMap("sb:abc@10.0.0.1:4500"))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var deployments appsv1.DeploymentList
	if err := c.List(context.Background(), &deployments, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, d := range deployments.Items {
		for _, annotations := range []map[string]string{d.Annotations, d.Spec.Template.Annotations} {
			for key := range annotations {
				if strings.HasPrefix(key, "reloader.stakater.com/") {
					t.Errorf("%s carries %s, which nothing acts on", d.Name, key)
				}
			}
		}
	}
}

func podSpecsOf(objects []client.Object) map[string]corev1.PodSpec {
	specs := map[string]corev1.PodSpec{}
	for _, o := range objects {
		switch w := o.(type) {
		case *appsv1.Deployment:
			specs[w.Name] = w.Spec.Template.Spec
		case *appsv1.StatefulSet:
			specs[w.Name] = w.Spec.Template.Spec
		case *appsv1.DaemonSet:
			specs[w.Name] = w.Spec.Template.Spec
		case *batchv1.Job:
			specs[w.Name] = w.Spec.Template.Spec
		}
	}
	return specs
}

func TestTheClusterFileIsMountedAsADirectory(t *testing.T) {
	cp := localControlPlane()
	objects := append(managementAPIObjects(cp), datastoreObjects(cp)...)
	objects = append(objects, indexJob(cp))

	mounts := 0
	for name, spec := range podSpecsOf(objects) {
		for _, container := range append(spec.InitContainers, spec.Containers...) {
			for _, m := range container.VolumeMounts {
				if m.Name != clusterFileVolume {
					continue
				}
				mounts++
				if m.SubPath != "" || m.MountPath != "/etc/foundationdb" {
					t.Errorf("%s/%s mounts the cluster file at %q with subPath %q, want the directory "+
						"/etc/foundationdb without subPath, so the kubelet refreshes it in place",
						name, container.Name, m.MountPath, m.SubPath)
				}
			}
		}
	}
	if mounts == 0 {
		t.Fatal("no workload mounts the cluster file")
	}
}

func TestAClusterFileChangeDoesNotRollTheWorkloads(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
	cm := clusterFileConfigMap("sb:abc@10.0.0.1:4500")
	c := newClient(t, cp, cm)
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t)}

	templates := func() map[string]corev1.PodTemplateSpec {
		var list appsv1.DeploymentList
		if err := c.List(ctx, &list, client.InNamespace(testNamespace)); err != nil {
			t.Fatalf("list: %v", err)
		}
		out := map[string]corev1.PodTemplateSpec{}
		for _, d := range list.Items {
			out[d.Name] = d.Spec.Template
		}
		return out
	}

	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	before := templates()

	cm.Data[clusterFileKey] = "sb:abc@10.0.0.9:4500"
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("move the coordinator: %v", err)
	}
	if err := r.applyEverything(ctx, cp); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for name, after := range templates() {
		if !equality.Semantic.DeepEqual(before[name].Annotations, after.Annotations) {
			t.Errorf("%s's pod template changed from %v to %v after a coordinator move, which rolls its pods",
				name, before[name].Annotations, after.Annotations)
		}
	}
}
