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
)

func TestTheBaselineJobToleratesWhatTheStorageNodesDo(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tolerations := []corev1.Toleration{{
		Key:      "io.simplyblock.node-type",
		Operator: corev1.TolerationOpEqual,
		Value:    "storage-plane",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{Tolerations: tolerations},
		},
	}
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "simplyblock"},
		Status: simplyblockv1alpha2.StorageNodeStatus{
			UUID:     "22222222-2222-2222-2222-222222222222",
			Hostname: "worker-1",
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
	if got := jobs.Items[0].Spec.Template.Spec.Tolerations; len(got) != 1 || got[0].Key != "io.simplyblock.node-type" {
		t.Errorf("the Job tolerates %+v, want the cluster's taint", got)
	}
}
