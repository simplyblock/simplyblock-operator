// What a node announces when the pod it runs in cannot be placed.
//
// A storage node that never comes up looks identical from the control plane's
// side whether its worker is wedged, its API is down, or Kubernetes never
// managed to start the pod at all. The last of those is the one the backend
// cannot see, and the scheduler has already written down exactly why.

package node

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// aPendingStoragePod is a storage-node pod the scheduler has looked at and
// refused. It has no spec.nodeName, because being unplaced is the whole point,
// and it names its worker the way the DaemonSet controller does.
func aPendingStoragePod(worker, reason, message string) *corev1.Pod {
	pod := aStoragePod(worker)
	pod.Spec.NodeName = ""
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{{
					Key:      "metadata.name",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{worker},
				}},
			}},
		},
	}}
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  reason,
			Message: message,
		}},
	}
	return pod
}

// aStoragePod is one the scheduler has placed.
func aStoragePod(worker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simplyblock-storage-node-" + worker,
			Namespace: "simplyblock",
			Labels: map[string]string{
				kube.LabelApp:                kube.AppStorageNode,
				kube.LabelSimplyblockCluster: "a-cluster",
				kube.LabelStorageNodeSet:     "a-cluster",
			},
		},
		Spec: corev1.PodSpec{NodeName: worker},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

// aHeldNode is a node whose cluster has no UUID, so the reconcile reaches the
// hold of §4.2 and performs no control-plane call. The scheduling report is not
// part of provisioning, so this is the cheapest pass that carries it.
func aHeldNode() *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "a-cluster-worker-1-0",
			Namespace:  "simplyblock",
			Finalizers: []string{NodeFinalizer},
		},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "a-cluster",
			WorkerNode: "worker-1",
		},
	}
	return node
}

func aSchedulingReporter(
	t *testing.T, node *simplyblockv1alpha2.StorageNode, pods ...client.Object,
) *StorageNodeReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a-cluster", Namespace: "simplyblock"},
	}
	objects := append([]client.Object{cluster, node}, pods...)
	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageNode{}).
		Build()

	return &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
	}
}

func reconcileNode(t *testing.T, r *StorageNodeReconciler,
	node *simplyblockv1alpha2.StorageNode) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(node),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// The event exists so that a node holding for a reason the control plane cannot
// see says which reason it is.
func TestAnUnplaceablePodIsAnnouncedOnItsNode(t *testing.T) {
	node := aHeldNode()
	r := aSchedulingReporter(t, node, aPendingStoragePod("worker-1",
		corev1.PodReasonUnschedulable,
		"0/3 nodes are available: 1 Insufficient hugepages-2Mi."))

	reconcileNode(t, r, node)

	var announcement string
	for _, event := range drainReasons(r.Recorder.(*events.FakeRecorder)) {
		if strings.Contains(event, PodSchedulingFailed) {
			announcement = event
		}
	}
	if announcement == "" {
		t.Fatal("nothing announced PodSchedulingFailed, so the hold names no cause")
	}
	if !strings.Contains(announcement, "Insufficient hugepages-2Mi") {
		t.Errorf("the event says %q, want the scheduler's own reason in it", announcement)
	}
	if !strings.Contains(announcement, "worker-1") {
		t.Errorf("the event says %q, want the worker it could not be placed on", announcement)
	}
}

// A pod that is running is not news, and an event raised every pass for a
// healthy node is an alert nobody can act on.
func TestAPlacedPodIsAnnouncedNowhere(t *testing.T) {
	node := aHeldNode()
	r := aSchedulingReporter(t, node, aStoragePod("worker-1"))

	reconcileNode(t, r, node)

	if announced(r.Recorder.(*events.FakeRecorder), PodSchedulingFailed) {
		t.Error("a placed pod was announced as a scheduling failure")
	}
}

// The DaemonSet covers every worker of the cluster, so one node's reconcile sees
// every other node's pod. A failure on a different worker is a different node's
// event, and reporting it here would announce it once per node in the cluster.
func TestAnotherWorkersFailureIsNotThisNodes(t *testing.T) {
	node := aHeldNode()
	r := aSchedulingReporter(t, node, aStoragePod("worker-1"),
		aPendingStoragePod("worker-2", corev1.PodReasonUnschedulable,
			"0/3 nodes are available: 1 Insufficient cpu."))

	reconcileNode(t, r, node)

	if announced(r.Recorder.(*events.FakeRecorder), PodSchedulingFailed) {
		t.Error("another worker's unplaceable pod was announced on this node")
	}
}

// A gated pod is one the scheduler has not tried to place yet, which is a wait
// rather than a failure. Announcing it would raise a warning for every pod that
// passes through a scheduling gate on its way to running.
func TestAGatedPodIsAWaitRatherThanAFailure(t *testing.T) {
	node := aHeldNode()
	r := aSchedulingReporter(t, node, aPendingStoragePod("worker-1",
		corev1.PodReasonSchedulingGated, "Scheduling is blocked by a gate"))

	reconcileNode(t, r, node)

	if announced(r.Recorder.(*events.FakeRecorder), PodSchedulingFailed) {
		t.Error("a gated pod was announced as a scheduling failure")
	}
}
