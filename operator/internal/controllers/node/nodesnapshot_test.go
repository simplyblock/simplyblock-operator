// The workers enrolled and the workers configured come from one reading.
//
// The pass writes the per-node ConfigMap and then labels the workers, and a
// worker is only schedulable once it carries the label. That ordering is what
// stops a storage-node pod starting against an entry that is not there yet -- and
// it only holds while both halves are looking at the same set of nodes.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

// aClusterNode is one storage node of the cluster, on the worker given.
func aClusterNode(name, worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: "c",
			WorkerNode: worker,
			Config: simplyblockv1alpha2.StorageNodeConfig{
				Sizing: simplyblockv1alpha2.StorageNodeSizing{VCPUCount: ptrTo32(4)},
			},
		},
	}
}

func ptrTo32(v int32) *int32 { return &v }

// aSnapshotCluster carries the sizing ReconcileConfig refuses to render without.
func aSnapshotCluster() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptrTo32(30),
			VCPUCount:         ptrTo32(4),
		},
	}
}

// TestEveryEnrolledWorkerHasAnEntry is the window this closes.
//
// Regression: 2026-09-21-two-readings-of-one-node-set — the ConfigMap was built
// from one List and the workers were labeled from another, so a StorageNode that
// appeared between the two had its worker enrolled in a pass that never wrote its
// entry. The pod then started, found no entry, and reached the configure with
// nothing set: on the deployment this was found on, one worker of six sat in
// CrashLoopBackOff reporting an invalid max-lvol of 0 while the ConfigMap beside
// it had been correct for minutes.
//
// The second List is what the interceptor below adds a node to, which is the
// cache advancing mid-pass. Both halves reading one snapshot is what makes the
// two sets the same set rather than two sets that usually agree.
func TestEveryEnrolledWorkerHasAnEntry(t *testing.T) {
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	cluster := aSnapshotCluster()
	early := aClusterNode("n-w1", "w1")
	late := aClusterNode("n-w2", "w2")

	workers := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w2"}},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append([]client.Object{cluster, early, late}, workers...)...).
		Build()

	// The first reading of the node set sees one node, and every reading after it
	// sees both: the shape of a cache catching up in the middle of a pass.
	reads := 0
	racing := interceptor.NewClient(base, interceptor.Funcs{
		List: func(
			ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption,
		) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			nodes, ok := list.(*simplyblockv1alpha2.StorageNodeList)
			if !ok {
				return nil
			}
			reads++
			if reads == 1 {
				nodes.Items = []simplyblockv1alpha2.StorageNode{*early}
			}
			return nil
		},
	})

	r := &StorageNodeWorkloadReconciler{
		Client:    racing,
		Scheme:    scheme,
		Namespace: "simplyblock",
		Workload:  &Workload{Client: racing},
	}

	snapshot, err := r.clusterNodes(context.Background(), cluster)
	if err != nil {
		t.Fatalf("read the node set: %v", err)
	}
	if err := r.Workload.ReconcileConfig(context.Background(), cluster, snapshot); err != nil {
		t.Fatalf("write the per-node config: %v", err)
	}
	if err := r.enrollWorkers(context.Background(), cluster, snapshot); err != nil {
		t.Fatalf("enroll the workers: %v", err)
	}

	var config corev1.ConfigMap
	key := client.ObjectKey{Namespace: "simplyblock", Name: PerNodeConfigMapName("c")}
	if err := base.Get(context.Background(), key, &config); err != nil {
		t.Fatalf("read the per-node ConfigMap: %v", err)
	}

	for _, worker := range []string{"w1", "w2"} {
		var object corev1.Node
		if err := base.Get(context.Background(), client.ObjectKey{Name: worker}, &object); err != nil {
			t.Fatalf("read worker %s: %v", worker, err)
		}
		enrolled := len(object.Labels) > 0
		_, configured := config.Data[worker]
		if enrolled && !configured {
			t.Errorf("worker %s is enrolled with no entry, so a pod can start on it "+
				"with nothing to configure from", worker)
		}
	}
}
