// The watch that keeps the per-pod DNS names current.
//
// reconcileSpdkProxyEndpoints builds its EndpointSlices from a live list of the
// spdk-proxy pods, so a pod is the only thing that can make them wrong. Nothing
// else observes that: the controller's other sources are the cluster, the nodes
// and the objects it owns. A pod appearing -- or reappearing on a different RPC
// port after a retry -- therefore has to enqueue the pass itself, or the slice
// is published only if some unrelated event happens to follow it.
//
// Found live 2026-09-23: five of six workers came up, the sixth had its pod
// recreated on a new port, no slice followed, its name never resolved, and the
// node add retried to exhaustion on a DNS lookup.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// aWatchReconciler is a reconciler holding the given objects, for exercising the
// mapping and filtering the pod watch is wired with.
func aWatchReconciler(t *testing.T, objects ...client.Object) *StorageNodeWorkloadReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme)
	return &StorageNodeWorkloadReconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Scheme:    scheme,
		Namespace: "simplyblock",
	}
}

func aProxyPod(name, namespace string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
	}
}

// TestAProxyPodEnqueuesItsCluster is the regression this file exists for: a pod
// that has just appeared must name the cluster whose pass republishes the DNS.
func TestAProxyPodEnqueuesItsCluster(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
	}
	pod := aProxyPod("snode-spdk-pod-4432-abc", "simplyblock",
		map[string]string{"role": utils.LabelSpdkProxyRole, "app": "spdk-app-4432"})

	r := aWatchReconciler(t, cluster, pod)

	requests := r.clusterOfProxyPod(context.Background(), pod)
	if len(requests) != 1 {
		t.Fatalf("a new spdk-proxy pod enqueued %d clusters, so nothing republishes "+
			"its EndpointSlice and its DNS name never resolves; want 1", len(requests))
	}
	if requests[0].Name != "c" || requests[0].Namespace != "simplyblock" {
		t.Errorf("enqueued %s/%s, want simplyblock/c",
			requests[0].Namespace, requests[0].Name)
	}
}

// TestAPodOfAnotherNamespaceIsNotThisClustersConcern keeps the mapping no wider
// than the work it triggers: the pass only ever lists its own namespace.
func TestAPodOfAnotherNamespaceIsNotThisClustersConcern(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
	}
	pod := aProxyPod("snode-spdk-pod-4420-abc", "other",
		map[string]string{"role": utils.LabelSpdkProxyRole})

	r := aWatchReconciler(t, cluster, pod)

	if requests := r.clusterOfProxyPod(context.Background(), pod); len(requests) != 0 {
		t.Errorf("a pod in namespace %q enqueued %d clusters of another namespace",
			pod.Namespace, len(requests))
	}
}

// TestANonProxyPodEnqueuesNothing: the map function is the second line of
// defence behind the predicate, and both read the same label.
func TestANonProxyPodEnqueuesNothing(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
	}
	for _, labels := range []map[string]string{
		nil,
		{"role": "something-else"},
		{"app": "spdk-app-4420"},
	} {
		pod := aProxyPod("p", "simplyblock", labels)
		r := aWatchReconciler(t, cluster, pod)
		if requests := r.clusterOfProxyPod(context.Background(), pod); len(requests) != 0 {
			t.Errorf("a pod labelled %v woke the workload pass", labels)
		}
	}
}

// TestANonPodEnqueuesNothing guards the type assertion.
func TestANonPodEnqueuesNothing(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
	}
	r := aWatchReconciler(t, cluster)

	if requests := r.clusterOfProxyPod(context.Background(), cluster); len(requests) != 0 {
		t.Errorf("a %T enqueued %d requests through the pod mapping",
			cluster, len(requests))
	}
}

// TestThePredicateAdmitsOnlyProxyPods: without it every pod in the namespace
// wakes the pass, and a namespace running anything else pays for it.
func TestThePredicateAdmitsOnlyProxyPods(t *testing.T) {
	admits := isSpdkProxyPod()

	proxy := aProxyPod("snode-spdk-pod-4420-abc", "simplyblock",
		map[string]string{"role": utils.LabelSpdkProxyRole})
	if !admits.Create(event.CreateEvent{Object: proxy}) {
		t.Error("the predicate dropped an spdk-proxy pod, so its slice is never published")
	}

	for _, labels := range []map[string]string{nil, {"role": "csi-node"}} {
		other := aProxyPod("p", "simplyblock", labels)
		if admits.Create(event.CreateEvent{Object: other}) {
			t.Errorf("the predicate admitted a pod labelled %v", labels)
		}
	}
}
