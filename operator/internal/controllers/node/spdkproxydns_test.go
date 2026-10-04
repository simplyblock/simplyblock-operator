// The per-pod DNS names the control plane reaches an SPDK process by.
//
// A node's SPDK process is created by the control plane and then driven over RPC
// at worker-N.simplyblock-spdk-proxy.<ns>.svc.cluster.local:<rpc port>. That name
// comes from a headless Service plus an EndpointSlice carrying one endpoint per
// worker, with the worker's name as the endpoint hostname. The Service alone
// resolves nothing.

package node

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// anSPDKPod is one SPDK process as the control plane creates it: host-networked,
// so its pod address is the worker's, and carrying its RPC port in the app label.
func anSPDKPod(name, worker, address string, rpcPort int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "simplyblock",
			Labels: map[string]string{
				"app":  fmt.Sprintf("spdk-app-%d", rpcPort),
				"role": "simplyblock-storage-node",
			},
		},
		Spec: corev1.PodSpec{
			HostNetwork: true,
			NodeName:    worker,
			Containers: []corev1.Container{{
				Name: "spdk-proxy-container",
				Env:  []corev1.EnvVar{{Name: "RPC_PORT", Value: fmt.Sprintf("%d", rpcPort)}},
			}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             address,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "spdk-proxy-container", Ready: true}},
		},
	}
}

// aProxyReconciler is the workload reconciler over the pods given.
func aProxyReconciler(
	t *testing.T, pods ...*corev1.Pod,
) (*StorageNodeWorkloadReconciler, *simplyblockv1alpha2.StorageCluster) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme)

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
	}
	objects := make([]client.Object, 0, 1+len(pods))
	objects = append(objects, cluster)
	for _, p := range pods {
		objects = append(objects, p)
	}

	return &StorageNodeWorkloadReconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Scheme:    scheme,
		Namespace: "simplyblock",
	}, cluster
}

// TestTheSPDKProxyNamesArePublished is the defect.
//
// Regression: 2026-09-21-the-spdk-proxy-service-had-no-endpoints — the headless
// Service was applied and its EndpointSlice never was. BuildSpdkProxyEndpointSlice
// existed and nothing called it, so every per-pod name resolved to nothing and
// every node add stalled in the control plane's RPC retry loop: "Failed to
// resolve the worker's per-pod name. The
// pod was up, the Service was there, and the add never finished.
func TestTheSPDKProxyNamesArePublished(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
	)

	if err := r.reconcileSpdkProxyEndpoints(context.Background(), cluster); err != nil {
		t.Fatalf("publish the proxy endpoints: %v", err)
	}

	var slice discoveryv1.EndpointSlice
	key := client.ObjectKey{Namespace: "simplyblock", Name: "spdk-proxy-endpoints-4420"}
	if err := r.Get(context.Background(), key, &slice); err != nil {
		t.Fatalf("the proxy EndpointSlice was not published: %v", err)
	}

	if len(slice.Endpoints) != 1 {
		t.Fatalf("the slice carries %d endpoints", len(slice.Endpoints))
	}
	endpoint := slice.Endpoints[0]
	if endpoint.Hostname == nil || *endpoint.Hostname != "worker-0" {
		t.Errorf("the endpoint hostname is %v, and the control plane resolves worker-0",
			endpoint.Hostname)
	}
	if len(endpoint.Addresses) != 1 || endpoint.Addresses[0] != "10.0.0.10" {
		t.Errorf("the endpoint addresses are %v", endpoint.Addresses)
	}
	if slice.Labels["kubernetes.io/service-name"] != "simplyblock-spdk-proxy" {
		t.Errorf("the slice is not attached to the headless Service: %v", slice.Labels)
	}
}

// One slice per RPC port, because each socket of each worker answers on its own
// and a slice carries one port.
func TestEachRPCPortGetsItsOwnSlice(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "10.0.0.10", 4420),
		anSPDKPod("snode-spdk-pod-4422-abc", "worker-1.ocp.simplyblock.ai", "10.0.0.11", 4422),
	)

	if err := r.reconcileSpdkProxyEndpoints(context.Background(), cluster); err != nil {
		t.Fatalf("publish the proxy endpoints: %v", err)
	}

	for port, worker := range map[int]string{4420: "worker-0", 4422: "worker-1"} {
		var slice discoveryv1.EndpointSlice
		key := client.ObjectKey{
			Namespace: "simplyblock",
			Name:      fmt.Sprintf("spdk-proxy-endpoints-%d", port),
		}
		if err := r.Get(context.Background(), key, &slice); err != nil {
			t.Fatalf("port %d was not published: %v", port, err)
		}
		if len(slice.Endpoints) != 1 || *slice.Endpoints[0].Hostname != worker {
			t.Errorf("port %d published %d endpoints", port, len(slice.Endpoints))
		}
		if slice.Ports[0].Port == nil || int(*slice.Ports[0].Port) != port {
			t.Errorf("the slice for port %d carries port %v", port, slice.Ports[0].Port)
		}
	}
}

// A pod with no address yet is left out rather than published with none: an
// endpoint with no address resolves to nothing and is worse than an absent name,
// because a caller gets a resolution rather than a retry.
func TestAPodWithNoAddressIsNotPublished(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.ocp.simplyblock.ai", "", 4420),
	)

	if err := r.reconcileSpdkProxyEndpoints(context.Background(), cluster); err != nil {
		t.Fatalf("publish the proxy endpoints: %v", err)
	}

	var slice discoveryv1.EndpointSlice
	key := client.ObjectKey{Namespace: "simplyblock", Name: "spdk-proxy-endpoints-4420"}
	err := r.Get(context.Background(), key, &slice)
	if err == nil && len(slice.Endpoints) != 0 {
		t.Errorf("a pod with no address was published as %v", slice.Endpoints[0].Addresses)
	}
}

// Two workers whose names share a first DNS segment cannot both be published,
// because the endpoint hostname is that segment. It is an error rather than a
// silent overwrite of one worker's address with the other's.
func TestACollidingWorkerNameIsRefused(t *testing.T) {
	r, cluster := aProxyReconciler(t,
		anSPDKPod("snode-spdk-pod-4420-abc", "worker-0.site-a.example", "10.0.0.10", 4420),
		anSPDKPod("snode-spdk-pod-4420-def", "worker-0.site-b.example", "10.0.0.20", 4420),
	)

	err := r.reconcileSpdkProxyEndpoints(context.Background(), cluster)
	if err == nil {
		t.Fatal("two workers sharing a DNS label were published without complaint")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("the error does not name the collision: %v", err)
	}
}

// The builder is the one this uses, so the name it produces is the name the
// control plane was configured to resolve.
func TestTheSliceNameMatchesTheBuilder(t *testing.T) {
	slice, err := utils.BuildSpdkProxyEndpointSlice(
		&simplyblockv1alpha2.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "simplyblock"},
		},
		4420,
		[]utils.SpdkProxyEndpoint{{NodeName: "worker-0.x", PodIP: "10.0.0.10", RpcPort: 4420}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if slice.Name != "spdk-proxy-endpoints-4420" {
		t.Errorf("the builder names the slice %q", slice.Name)
	}
}

// TestThePassPublishesTheProxyEndpoints is the gap the builder fell through.
//
// Regression: 2026-09-21-the-spdk-proxy-slice-had-no-caller —
// BuildSpdkProxyEndpointSlice survived the retirement of StorageNodeSet and the
// reconcile that called it did not. The builder kept its unit tests and went on
// passing them, so nothing anywhere failed while every per-pod name resolved to
// nothing.
//
// A case that calls the reconcile directly cannot catch that: it is the wiring
// that was missing, not the behavior. This asserts the step is in the pass.
func TestThePassPublishesTheProxyEndpoints(t *testing.T) {
	r, _ := aProxyReconciler(t)

	steps := r.workloadSteps(nil)
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.what)
	}
	if !slices.Contains(names, "the spdk-proxy endpoints") {
		t.Error("no step of the workload pass publishes the spdk-proxy endpoints, " +
			"so the control plane resolves nothing under TLS")
	}
}
