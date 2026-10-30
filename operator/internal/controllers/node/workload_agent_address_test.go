// The address the storage-node API EndpointSlice publishes for a worker: the
// worker's own IP while the agent runs on the host network, and the agent pod's
// IP once the cluster declares spec.storageNodes.network (design §5.2).

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	atlaskube "github.com/simplyblock/atlas/kube"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const agentPodAddress = "10.244.1.7"

func anAgentPod(podIP string) *corev1.Pod {
	return anAgentPodOn(opsWorker, podIP)
}

// anAgentPodOn is the agent pod of a worker, at the address given.
func anAgentPodOn(worker, podIP string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-" + worker,
			Namespace: opsNamespace,
			Labels: map[string]string{
				atlaskube.LabelApp:                atlaskube.AppStorageNode,
				atlaskube.LabelSimplyblockCluster: opsCluster,
			},
		},
		Spec:   corev1.PodSpec{NodeName: worker},
		Status: corev1.PodStatus{PodIP: podIP},
	}
}

func publishedAfterReconcile(t *testing.T, network *simplyblockv1alpha2.StorageNetworkSpec, pods ...*corev1.Pod) map[string]string {
	t.Helper()

	cluster := anOpsCluster()
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{Network: network}

	scheme := testsupport.NewScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme)
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster, anOpsNode(), aWorker(opsWorker, true))
	for _, pod := range pods {
		builder = builder.WithObjects(pod)
	}
	apiClient := builder.Build()
	r := &StorageNodeWorkloadReconciler{Client: apiClient, Scheme: scheme, Namespace: opsNamespace}

	nodes, err := r.clusterNodes(context.Background(), cluster)
	if err != nil {
		t.Fatalf("clusterNodes: %v", err)
	}
	if err := r.reconcileEndpointSlice(context.Background(), cluster, nodes); err != nil {
		t.Fatalf("reconcileEndpointSlice: %v", err)
	}
	return publishedWorkers(t, apiClient)
}

func aDeclaredNetwork() *simplyblockv1alpha2.StorageNetworkSpec {
	return onDefinition("fabric-data")
}

// I-39
func TestTheSliceCarriesTheAgentPodAddressOnceANetworkIsDeclared(t *testing.T) {
	published := publishedAfterReconcile(t, aDeclaredNetwork(), anAgentPod(agentPodAddress))

	if got := published[utils.NodeHostnameLabel(opsWorker)]; got != agentPodAddress {
		t.Fatalf("published %q for the worker, want the agent pod's %q (all: %v)", got, agentPodAddress, published)
	}
}

// I-39: a pod with no address yet is not published, as a worker with no address is not.
func TestTheSliceOmitsAnAgentPodWithNoAddressYet(t *testing.T) {
	published := publishedAfterReconcile(t, aDeclaredNetwork(), anAgentPod(""))

	if len(published) != 0 {
		t.Fatalf("published %v for a pod without an address, want nothing", published)
	}
}

// I-40: the agent is always in the pod network, so a cluster with no network
// declared carries the pod's address as well.
func TestTheSliceCarriesTheAgentPodAddressWhenNoNetworkIsDeclared(t *testing.T) {
	published := publishedAfterReconcile(t, nil, anAgentPod(agentPodAddress))

	if got := published[utils.NodeHostnameLabel(opsWorker)]; got != agentPodAddress {
		t.Fatalf("published %q, want the agent pod's %q (all: %v)", got, agentPodAddress, published)
	}
}

// The slice carries the agent pod's address, so a pod that restarts on a new
// address has to wake the pass for its cluster.
func TestAnAgentPodWakesThePassForItsCluster(t *testing.T) {
	r := &StorageNodeWorkloadReconciler{}

	requests := r.clusterOfAgentPod(context.Background(), anAgentPod(agentPodAddress))

	if len(requests) != 1 || requests[0].Name != opsCluster || requests[0].Namespace != opsNamespace {
		t.Fatalf("requests = %v, want one for %s/%s", requests, opsNamespace, opsCluster)
	}
}

func TestAPodThatIsNotAnAgentWakesNothing(t *testing.T) {
	r := &StorageNodeWorkloadReconciler{}
	pod := anAgentPod(agentPodAddress)
	delete(pod.Labels, atlaskube.LabelApp)

	if requests := r.clusterOfAgentPod(context.Background(), pod); len(requests) != 0 {
		t.Fatalf("requests = %v for a pod without the agent label, want none", requests)
	}
	if isAgentPod().Create(event.CreateEvent{Object: pod}) {
		t.Fatal("isAgentPod accepted a pod without the agent label")
	}
}
