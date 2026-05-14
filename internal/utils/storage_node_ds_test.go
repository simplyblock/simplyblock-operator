package utils

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func TestStorageNodeAPIAddress(t *testing.T) {
	got := StorageNodeAPIAddress("worker-1", "simplyblock")
	want := "worker-1.simplyblock-storage-node-api.simplyblock.svc.cluster.local:5000"
	if got != want {
		t.Fatalf("StorageNodeAPIAddress = %q, want %q", got, want)
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
