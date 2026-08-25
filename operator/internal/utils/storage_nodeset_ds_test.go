package utils

import (
	"strings"
	"testing"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStorageNodeSetAPIAddress(t *testing.T) {
	got := StorageNodeSetAPIAddress("worker-1", "simplyblock")
	want := "worker-1.simplyblock-storage-node-api.simplyblock.svc.cluster.local:5000"
	if got != want {
		t.Fatalf("StorageNodeSetAPIAddress = %q, want %q", got, want)
	}
}

func TestBuildStorageNodeSetClusterRoleBindingUsesGivenNames(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "sn-a", Namespace: "cluster1"}}
	binding := BuildStorageNodeSetClusterRoleBinding(sn, "binding-name", "sa-name")

	if binding.Name != "binding-name" {
		t.Fatalf("expected ClusterRoleBinding name %q, got %q", "binding-name", binding.Name)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "sa-name" || binding.Subjects[0].Namespace != "cluster1" {
		t.Fatalf("expected subject sa-name in namespace cluster1, got %#v", binding.Subjects)
	}
}

func TestBuildSpdkProxyEndpointSlice_DottedNodeNameTruncates(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns"},
	}
	endpoints := []SpdkProxyEndpoint{
		{NodeName: "ip-10-0-1-23.us-east-1.compute.internal", PodIP: "10.0.1.23", RpcPort: 9001},
		{NodeName: "worker-1", PodIP: "10.0.1.24", RpcPort: 9001},
	}

	eps, err := BuildSpdkProxyEndpointSlice(sn, 9001, endpoints)
	if err != nil {
		t.Fatalf("BuildSpdkProxyEndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(eps.Endpoints))
	}

	got := map[string]string{}
	for _, e := range eps.Endpoints {
		if e.Hostname == nil || len(e.Addresses) != 1 {
			t.Fatalf("malformed endpoint %#v", e)
		}
		got[*e.Hostname] = e.Addresses[0]
	}
	if got["ip-10-0-1-23"] != "10.0.1.23" {
		t.Fatalf("expected dotted node name truncated to first label, got %#v", got)
	}
	if got["worker-1"] != "10.0.1.24" {
		t.Fatalf("expected single-label node name preserved, got %#v", got)
	}
}

func TestBuildSpdkProxyEndpointSlice_CollidingFirstLabelFails(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns"},
	}
	endpoints := []SpdkProxyEndpoint{
		{NodeName: "worker.us-east-1.local", PodIP: "10.0.0.1", RpcPort: 9001},
		{NodeName: "worker.eu-west-1.local", PodIP: "10.0.0.2", RpcPort: 9001},
	}

	eps, err := BuildSpdkProxyEndpointSlice(sn, 9001, endpoints)
	if err == nil {
		t.Fatalf("expected collision error, got slice %#v", eps)
	}
	if !strings.Contains(err.Error(), "worker.us-east-1.local") ||
		!strings.Contains(err.Error(), "worker.eu-west-1.local") {
		t.Fatalf("expected error to name both colliding nodes, got %q", err.Error())
	}
}

func TestBuildStorageNodeSetDaemonSetUserResourcesOverrideDefaults(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName:  "test-cluster",
			ClusterImage: "simplyblock/simplyblock:latest",
			ContainerResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			},
			InitContainerResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			},
		},
	}

	ds := BuildStorageNodeSetDaemonSet(sn, "sa-name", false, false, "", "")

	main := ds.Spec.Template.Spec.Containers[0]
	mainMem := main.Resources.Limits[corev1.ResourceMemory]
	if mainMem.String() != "4Gi" {
		t.Errorf("main container: expected user memory limit 4Gi, got %v", mainMem.String())
	}

	init := ds.Spec.Template.Spec.InitContainers[1] // [0]=node-env-writer, [1]=s-node-api-config-generator
	initMem := init.Resources.Limits[corev1.ResourceMemory]
	if initMem.String() != "128Mi" {
		t.Errorf("init container: expected user memory limit 128Mi, got %v", initMem.String())
	}
}
