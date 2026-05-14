package utils

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func TestStorageNodeAPIAddress(t *testing.T) {
	got := StorageNodeAPIAddress("worker-1", "simplyblock", "simplyblock-storage-node-api-group-a")
	want := "worker-1.simplyblock-storage-node-api-group-a.simplyblock.svc.cluster.local:5000"
	if got != want {
		t.Fatalf("StorageNodeAPIAddress = %q, want %q", got, want)
	}
}

func TestBuildStorageNodeResourcesUseNodeGroupName(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "group-a", Namespace: "ns"},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			ClusterName: "cluster-a",
		},
	}

	ds := BuildStorageNodeDaemonSet(sn, true, false, TLSProviderCertManager, "42")
	if ds.Name != "simplyblock-storage-node-ds-group-a" {
		t.Fatalf("DaemonSet name = %q", ds.Name)
	}
	if ds.Spec.Template.Spec.NodeSelector["io.simplyblock.node-type"] != "simplyblock-storage-plane-group-a" {
		t.Fatalf("unexpected node selector %#v", ds.Spec.Template.Spec.NodeSelector)
	}
	if ds.Labels["simplyblock-storage-node"] != "group-a" {
		t.Fatalf("expected node-group label, got %#v", ds.Labels)
	}
	if ds.Spec.Template.Spec.Volumes[len(ds.Spec.Template.Spec.Volumes)-1].Secret.SecretName != "simplyblock-storage-node-api-tls-group-a" {
		t.Fatalf("unexpected TLS Secret volume %#v", ds.Spec.Template.Spec.Volumes)
	}

	svc := BuildStorageNodeService(sn, true, TLSProviderOpenShift)
	if svc.Name != "simplyblock-storage-node-api-group-a" {
		t.Fatalf("Service name = %q", svc.Name)
	}
	if svc.Annotations[OpenShiftServingCertAnnotation] != "simplyblock-storage-node-api-tls-group-a" {
		t.Fatalf("unexpected service annotations %#v", svc.Annotations)
	}
}

func TestBuildStorageNodeClusterRoleBindingNameIncludesNamespace(t *testing.T) {
	cluster1Binding := BuildStorageNodeClusterRoleBinding("cluster1")
	cluster2Binding := BuildStorageNodeClusterRoleBinding("cluster2")

	if cluster1Binding.Name == cluster2Binding.Name {
		t.Fatalf("expected per-namespace ClusterRoleBinding names, got %q", cluster1Binding.Name)
	}
	if cluster1Binding.Name != "simplyblock-storage-node-binding-cluster1" {
		t.Fatalf("unexpected cluster1 ClusterRoleBinding name %q", cluster1Binding.Name)
	}
	if len(cluster1Binding.Subjects) != 1 || cluster1Binding.Subjects[0].Namespace != "cluster1" {
		t.Fatalf("expected cluster1 service account subject, got %#v", cluster1Binding.Subjects)
	}
}

func TestBuildSpdkProxyEndpointSlice_DottedNodeNameTruncates(t *testing.T) {
	sn := &simplyblockv1alpha1.StorageNode{
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
	sn := &simplyblockv1alpha1.StorageNode{
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
