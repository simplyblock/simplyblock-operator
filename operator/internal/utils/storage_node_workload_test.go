package utils

import (
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
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

func TestBuildStorageNodeSetClusterRoleBindingNameIncludesNamespace(t *testing.T) {
	cluster1Binding := BuildStorageNodeSetClusterRoleBinding("cluster1")
	cluster2Binding := BuildStorageNodeSetClusterRoleBinding("cluster2")

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

// TestBuildStorageNodeDaemonSetConfigGeneratorMountsDevAndSys guards the mounts
// node_configure.py's lblk eligibility check needs.
//
// It arrived with the lblk work against the v1alpha1 builder and is written
// against the v1alpha2 one here, because that is the builder this operator
// runs: the StorageCluster is the DaemonSet's parent now, and the check is
// about the container rather than about which kind describes it.
func TestBuildStorageNodeDaemonSetConfigGeneratorMountsDevAndSys(t *testing.T) {
	sn := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "sn", Namespace: "ns"},
	}
	ds := BuildStorageNodeDaemonSet(sn, false, false, "", "", "")

	init := ds.Spec.Template.Spec.InitContainers[1] // [0]=node-env-writer, [1]=s-node-api-config-generator
	var hasDev, hasSys bool
	for _, m := range init.VolumeMounts {
		if m.Name == "dev-vol" && m.MountPath == "/dev" {
			hasDev = true
		}
		if m.Name == "host-sys" && m.MountPath == "/sys" {
			hasSys = true
		}
	}
	if !hasDev {
		t.Errorf("s-node-api-config-generator must mount /dev for lblk eligibility checks")
	}
	if !hasSys {
		t.Errorf("s-node-api-config-generator must mount /sys for block-device inspection")
	}
}

func TestBuildSpdkProxyEndpointSlice_DottedNodeNameTruncates(t *testing.T) {
	sn := &simplyblockv1alpha2.StorageCluster{
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
	sn := &simplyblockv1alpha2.StorageCluster{
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

func TestBuildStorageNodeDaemonSetUserResourcesOverrideDefaults(t *testing.T) {
	sn := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{
				ContainerResources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
				},
				InitContainerResources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			},
		},
	}

	ds := BuildStorageNodeDaemonSet(sn, false, false, "", "", "simplyblock/simplyblock:latest")

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

// What the pod carries as an environment variable is the fleet's value, and
// what the node's own entry states has to win over it. The entry is sourced
// rather than injected, so a variable it sets is a shell variable: sudo passes
// the environment, which a sourced assignment is not part of until it is
// exported.
func TestTheMainContainerExportsTheNodesReservedCPUs(t *testing.T) {
	ds := BuildStorageNodeDaemonSet(&simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "simplyblock"},
	}, false, false, "", "", "node-agent:test")

	containers := ds.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("the pod has %d containers, want 1", len(containers))
	}
	command := strings.Join(containers[0].Command, "\n")

	if !strings.Contains(command, ". /etc/node-env/env.sh") {
		t.Fatalf("the container does not source the node's entry: %s", command)
	}
	if !strings.Contains(command, "export RESERVED_SYSTEM_CPUS") {
		t.Errorf("the container sources the entry and exports nothing from it, "+
			"so the node's own reserved CPUs never reach the agent: %s", command)
	}
	// Only that one. Exporting the entry wholesale would put the device lists
	// and the class flag into the agent's environment, where nothing asked for
	// them and a name could collide.
	if strings.Contains(command, "set -a") {
		t.Errorf("the container exports the whole entry: %s", command)
	}
}
