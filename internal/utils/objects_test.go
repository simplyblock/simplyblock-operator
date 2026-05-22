package utils

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
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
	cr := &simplyblockv1alpha1.StorageNode{
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			WorkerNodes: []string{"n1", "n2", "n3"},
		},
	}
	if !ShouldActivateCluster(2, 3, cr) { // required=mod+1 => 3
		t.Fatalf("should activate when all workers are online and requirement met")
	}
}

func TestClusterStatusHelpers(t *testing.T) {
	active := &simplyblockv1alpha1.StorageCluster{
		Status: simplyblockv1alpha1.StorageClusterStatus{Status: "active"},
	}
	if !ClusterAlreadyActive(active) {
		t.Fatalf("ClusterAlreadyActive should be true")
	}

	expanding := &simplyblockv1alpha1.StorageCluster{
		Status: simplyblockv1alpha1.StorageClusterStatus{Status: "in_expansion"},
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

	clusterA := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "ns1"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: "uuid-a"},
	}
	clusterNoUUID := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-b", Namespace: "ns1"},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{},
	}

	poolA := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "gold", Namespace: "ns1"},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-a",
		},
		Status: simplyblockv1alpha1.PoolStatus{UUID: "pool-uuid-a"},
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

	exists, uuid, name, _, err := ExistingClusterUUID(ctx, c)
	if err != nil {
		t.Fatalf("ExistingClusterUUID unexpected error: %v", err)
	}
	if !exists || uuid != "uuid-a" || name != "cluster-a" {
		t.Fatalf("ExistingClusterUUID unexpected result: exists=%v uuid=%q name=%q", exists, uuid, name)
	}
}
