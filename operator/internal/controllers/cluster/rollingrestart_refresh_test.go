package cluster

// The pod refresh of a rolling restart on this site: a storage-node pod is
// replaced once per walk, and its replacement is never replaced again.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// workerAt is the Kubernetes worker of the fleet's node i, and the
// storage-node pod running on it, created at created.
func workerAt(i int, created time.Time, ready bool) (*corev1.Node, *corev1.Pod) {
	name := []string{"worker-a", "worker-b"}[i]
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ipFor(i)}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sn-" + name, Namespace: testNamespace, CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{storageNodePodAppLabel: storageNodePodApp, storageNodePodClusterLabel: testClusterName},
		},
		Spec: corev1.PodSpec{NodeName: name},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return node, pod
}

func podExists(t *testing.T, r *StorageClusterOpsReconciler, name string) bool {
	t.Helper()
	var pod corev1.Pod
	return r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &pod) == nil
}

// The pod the walk found when the operation started is deleted.
func TestAPodOlderThanTheWalkIsDeleted(t *testing.T) {
	started := time.Date(2026, 10, 26, 14, 41, 29, 0, time.UTC)
	ops := refreshingOps(started)
	worker, pod := workerAt(0, started.Add(-72*time.Hour), true)
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA)), &recorder{}, newTestCluster(), ops, worker, pod)

	if done, err := r.refreshPod(context.Background(), ops, testClusterUUID, newRollingFleet(nodeA).nodes(), nodeA); err != nil || done {
		t.Fatalf("refreshPod = %v, %v; want the old pod deleted and the step still running", done, err)
	}
	if podExists(t, r, pod.Name) {
		t.Error("the pod older than the walk was not deleted")
	}
}

// Regression: 2026-10-26-refresh-deletes-the-replacement: the step counted as
// finished only once it saw no pod, and the DaemonSet recreates the pod at
// once, so the walk deleted every replacement too, a storage-node pod every
// twenty to thirty seconds, until the step's deadline (live, 11-rolling-restart.sh).
// A pod created after the walk started is the refresh.
func TestAReplacementPodIsTheRefreshAndIsNotDeleted(t *testing.T) {
	started := time.Date(2026, 10, 26, 14, 41, 29, 0, time.UTC)
	ops := refreshingOps(started)
	worker, pod := workerAt(0, started.Add(7*time.Minute), false)
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA)), &recorder{}, newTestCluster(), ops, worker, pod)
	nodes := newRollingFleet(nodeA).nodes()

	if done, err := r.refreshPod(context.Background(), ops, testClusterUUID, nodes, nodeA); err != nil || !done {
		t.Fatalf("refreshPod = %v, %v; want the replacement counted as the refresh", done, err)
	}
	if !podExists(t, r, pod.Name) {
		t.Fatal("the replacement pod was deleted")
	}
	if done, err := r.awaitPod(context.Background(), ops, testClusterUUID, nodes, nodeA); err != nil || done {
		t.Errorf("awaitPod = %v, %v; want held until the replacement is Ready", done, err)
	}
}

// awaitPod does not take the old pod, still Ready, for its replacement.
func TestTheOldPodIsNotTakenForItsReplacement(t *testing.T) {
	started := time.Date(2026, 10, 26, 14, 41, 29, 0, time.UTC)
	ops := refreshingOps(started)
	worker, pod := workerAt(0, started.Add(-time.Hour), true)
	r := newOpsReconciler(t, rollingAPI(newRollingFleet(nodeA)), &recorder{}, newTestCluster(), ops, worker, pod)

	if done, err := r.awaitPod(context.Background(), ops, testClusterUUID, newRollingFleet(nodeA).nodes(), nodeA); err != nil || done {
		t.Errorf("awaitPod = %v, %v; want held: the Ready pod predates the walk", done, err)
	}
}
