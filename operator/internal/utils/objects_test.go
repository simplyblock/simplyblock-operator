package utils

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestRequiredNodesFromErasureCodingScheme(t *testing.T) {
	got, err := RequiredNodesFromErasureCodingScheme("2x1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Fatalf("got %d want 3", got)
	}

	if _, err := RequiredNodesFromErasureCodingScheme("invalid"); err == nil {
		t.Fatalf("expected error for invalid erasure coding scheme")
	}
}

func TestCountOnlineHealthyNodes(t *testing.T) {
	nodes := []simplyblockv1alpha1.NodeStatus{
		{Status: "online", Health: true},
		{Status: "online", Health: false},
		{Status: "offline", Health: true},
		{Status: "online", Health: true},
	}
	got := CountOnlineHealthyNodes(nodes)
	if got != 2 {
		t.Fatalf("got %d want 2", got)
	}
}

func TestShouldActivateCluster(t *testing.T) {
	cr := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			WorkerNodes: []string{"n1", "n2", "n3"},
		},
	}
	if !ShouldActivateCluster(2, 3, cr) { // required=mod+1 => 3
		t.Fatalf("should activate when all workers are online and requirement met")
	}
}

func TestClusterStatusHelpers(t *testing.T) {
	active := &simplyblockv1alpha2.StorageCluster{
		Status: simplyblockv1alpha2.StorageClusterStatus{Status: "active"},
	}
	if !ClusterAlreadyActive(active) {
		t.Fatalf("ClusterAlreadyActive should be true")
	}

	expanding := &simplyblockv1alpha2.StorageCluster{
		Status: simplyblockv1alpha2.StorageClusterStatus{Status: "in_expansion"},
	}
	if !ClusterInExpansion(expanding) {
		t.Fatalf("ClusterInExpansion should be true")
	}
}

func TestResolveClusterAndPoolUUID(t *testing.T) {
	s := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	// The pool is read as the hub, which is the shape every controller reads;
	// the cluster is not, because StorageCluster has no v1alpha2 yet.
	if err := simplyblockv1alpha2.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	clusterA := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "ns1"},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: "uuid-a"},
	}
	clusterNoUUID := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-b", Namespace: "ns1"},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{},
	}

	poolA := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "gold", Namespace: "ns1"},
		Spec: simplyblockv1alpha2.StoragePoolSpec{
			ClusterRef: "cluster-a",
		},
		Status: simplyblockv1alpha2.StoragePoolStatus{UUID: "pool-uuid-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(clusterA, clusterNoUUID, poolA).
		Build()

	ctx := context.Background()

	clusterUUID, err := ResolveClusterUUID(ctx, c, "ns1", "cluster-a")
	if err != nil {
		t.Fatalf("ResolveClusterUUID unexpected error: %v", err)
	}
	if clusterUUID != "uuid-a" {
		t.Fatalf("ResolveClusterUUID got %q want uuid-a", clusterUUID)
	}

	if _, err := ResolveClusterUUID(ctx, c, "ns1", "cluster-b"); err == nil {
		t.Fatalf("ResolveClusterUUID should fail when UUID not ready")
	}

	poolUUID, err := ResolvePoolUUID(ctx, c, "ns1", "cluster-a", "gold")
	if err != nil {
		t.Fatalf("ResolvePoolUUID unexpected error: %v", err)
	}
	if poolUUID != "pool-uuid-a" {
		t.Fatalf("ResolvePoolUUID got %q want pool-uuid-a", poolUUID)
	}

	if _, err := ResolvePoolUUID(ctx, c, "ns1", "cluster-a", "silver"); err == nil {
		t.Fatalf("ResolvePoolUUID should fail for missing pool")
	}

	found, err := ResolveClusterCRByUUID(ctx, c, "ns1", "uuid-a")
	if err != nil {
		t.Fatalf("ResolveClusterCRByUUID unexpected error: %v", err)
	}
	if found.Name != "cluster-a" {
		t.Fatalf("ResolveClusterCRByUUID got %q want cluster-a", found.Name)
	}

	if _, err := ResolveClusterCRByUUID(ctx, c, "ns1", "no-such-uuid"); err == nil {
		t.Fatalf("ResolveClusterCRByUUID should fail for an unknown UUID")
	}

	if _, err := ResolveClusterCRByUUID(ctx, c, "ns2", "uuid-a"); err == nil {
		t.Fatalf("ResolveClusterCRByUUID should not find a cluster from a different namespace")
	}
}

// A cluster that is redistributing data refuses every volume migration, and the
// operator has to know that before it asks rather than from the 400 it gets
// back. This holds the two ways it can know: the status the control plane
// reports, and the task list it mirrors when that status is not yet set.
func TestClusterRebalancing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		tasks  []simplyblockv1alpha2.ClusterTask
		want   bool
	}{
		{
			name:   "the control plane says so",
			status: "rebalancing",
			want:   true,
		},
		{
			name:   "a node is being added, which redistributes data",
			status: "active",
			tasks: []simplyblockv1alpha2.ClusterTask{
				{ID: "a", Type: "node_add", Status: "running"},
			},
			want: true,
		},
		{
			name:   "a node add that has not started yet still blocks",
			status: "active",
			tasks: []simplyblockv1alpha2.ClusterTask{
				{ID: "a", Type: "node_add", Status: "new"},
			},
			want: true,
		},
		{
			name:   "a finished node add does not",
			status: "active",
			tasks: []simplyblockv1alpha2.ClusterTask{
				{ID: "a", Type: "node_add", Status: "done"},
			},
			want: false,
		},
		{
			// Every node_add of a deployment can be done while the expansion it
			// started is still suspended, which is what the cluster that
			// prompted this reported.
			name:   "an expansion that has not finished blocks after its node adds have",
			status: "active",
			tasks: []simplyblockv1alpha2.ClusterTask{
				{ID: "a", Type: "node_add", Status: "done"},
				{ID: "b", Type: "cluster_expand", Status: "suspended"},
			},
			want: true,
		},
		{
			name:   "a deferred device migration blocks",
			status: "active",
			tasks: []simplyblockv1alpha2.ClusterTask{
				{ID: "a", Type: "new_device_migration", Status: "suspended"},
			},
			want: true,
		},
		{
			name:   "a task of another kind does not",
			status: "active",
			tasks: []simplyblockv1alpha2.ClusterTask{
				{ID: "a", Type: "device_test", Status: "running"},
			},
			want: false,
		},
		{
			name:   "an idle cluster does not",
			status: "active",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &simplyblockv1alpha2.StorageCluster{
				Status: simplyblockv1alpha2.StorageClusterStatus{
					Status: tc.status,
					Tasks:  tc.tasks,
				},
			}

			if got := ClusterRebalancing(cluster); got != tc.want {
				t.Errorf("ClusterRebalancing() = %v, want %v", got, tc.want)
			}
		})
	}
}
