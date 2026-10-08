// Where the baseline measurement Job is allowed to run.
//
// The Job is pinned to the storage node it measures, and pinning asks the
// scheduler for that node rather than excusing the pod from its taints. A fleet
// that dedicates machines to storage taints them, so the Job has to tolerate
// what the storage nodes themselves tolerate or it never runs on one.

package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/autoplacement"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func TestTheBaselineJobToleratesWhatTheStorageNodesDo(t *testing.T) {
	tolerations := []corev1.Toleration{{
		Key:      "io.simplyblock.node-type",
		Operator: corev1.TolerationOpEqual,
		Value:    "storage-plane",
		Effect:   corev1.TaintEffectNoSchedule,
	}}

	job := createdBaselineJob(t, tolerations)

	if got := job.Spec.Template.Spec.Tolerations; len(got) != 1 || got[0].Key != "io.simplyblock.node-type" {
		t.Errorf("the Job tolerates %+v, want the cluster's taint", got)
	}
}

// The measurement's pod is marked for the log collector, so a baseline that
// fails reaches Graylog before the Job's TTL removes the pod.
//
// Regression: 2026-10-06-graylog-receives-nothing — the baseline pods were
// never marked, so their logs were gone once the Job was collected.
func TestTheBaselineJobIsShippedToTheLogCollector(t *testing.T) {
	job := createdBaselineJob(t, nil)

	if got := job.Spec.Template.Annotations[utils.AnnotationLogCollector]; got != "true" {
		t.Errorf("baseline pod template %s = %q, want \"true\"", utils.AnnotationLogCollector, got)
	}
}

// The measurement Job runs on the worker that hosts the storage node, so its
// node selector is that worker's Kubernetes hostname and not the name the
// control plane reports for the node.
//
// Regression: 2026-10-08-baseline-job-selector-backend-hostname — the selector
// carried the control plane's "<host>_<rpcPort>" name, no node has that label,
// and every baseline Job stayed Pending, so no node ever got a baseline and
// automatic rebalancing never started.
func TestTheBaselineJobIsPinnedToTheWorkerHostingTheNode(t *testing.T) {
	job := createdBaselineJob(t, nil)

	got := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
	if got != "worker-1.example.com" {
		t.Errorf("the Job is pinned to kubernetes.io/hostname=%q, want the worker \"worker-1.example.com\"", got)
	}
}

// The sidecar on a worker reads the ConfigMap entry named after the worker's
// hostname, so a node's measurement target is filed under the worker.
//
// Regression: 2026-10-08-baseline-job-selector-backend-hostname — the entry was
// filed under the control plane's "<host>_<rpcPort>" name, which no sidecar's
// $HOSTNAME matches, so the continuous latency probe never received a target.
func TestTheProbeTargetIsFiledUnderTheWorkerHostingTheNode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "simplyblock"},
	}
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "worker-1.example.com"},
		Status: simplyblockv1alpha2.StorageNodeStatus{
			UUID:     "22222222-2222-2222-2222-222222222222",
			Hostname: "worker-1_4420",
			LatencyMetrics: &simplyblockv1alpha2.NodeLatencyMetrics{
				NodeUUID:      "22222222-2222-2222-2222-222222222222",
				BaselineP99NS: 1000,
			},
		},
	}
	r := &StorageNodeLatencyReconciler{
		Client:      fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, node).Build(),
		Scheme:      scheme,
		Provisioner: &AutomaticBenchmarkProvisioner{},
	}

	hostConfigs := map[string][]autoplacement.NodeConfig{}
	r.processNodeBaseline(context.Background(), cluster, cluster, "", node, "rebalancer:test", hostConfigs)

	if _, ok := hostConfigs["worker-1.example.com"]; !ok {
		keys := make([]string, 0, len(hostConfigs))
		for k := range hostConfigs {
			keys = append(keys, k)
		}
		t.Errorf("probe target filed under %v, want the worker \"worker-1.example.com\"", keys)
	}
}

// createdBaselineJob runs the reconciler's Job creation against a fake client
// for one storage node of a cluster with the given tolerations, and returns
// the one Job it created.
func createdBaselineJob(t *testing.T, tolerations []corev1.Toleration) batchv1.Job {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{Tolerations: tolerations},
		},
	}
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "simplyblock"},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "worker-1.example.com"},
		Status: simplyblockv1alpha2.StorageNodeStatus{
			UUID: "22222222-2222-2222-2222-222222222222",
			// The control plane reports the machine name with the node's RPC
			// port appended, which no Kubernetes node is labeled with.
			Hostname: "worker-1_4420",
		},
	}

	r := &StorageNodeLatencyReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, node).Build(),
		Scheme: scheme,
	}
	conn := benchmarkConnInfo{NQN: "nqn.2023-01.io.simplyblock:test", Addr: "10.0.0.1", Port: 4420}

	if err := r.createBaselineJob(context.Background(), cluster, node, conn, "rebalancer:test"); err != nil {
		t.Fatalf("create the baseline Job: %v", err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("simplyblock")); err != nil {
		t.Fatalf("list the Jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("created %d Jobs, want 1", len(jobs.Items))
	}
	return jobs.Items[0]
}
