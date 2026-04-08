package utils

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
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
	cr := &simplyblockv1alpha1.SimplyBlockStorageNode{
		Spec: simplyblockv1alpha1.SimplyBlockStorageNodeSpec{
			WorkerNodes: []string{"n1", "n2", "n3"},
		},
	}
	if !ShouldActivateCluster(2, 3, cr) { // required=mod+1 => 3
		t.Fatalf("should activate when all workers are online and requirement met")
	}

	coreIsolation := true
	cr.Spec.CoreIsolation = &coreIsolation
	if ShouldActivateCluster(2, 3, cr) {
		t.Fatalf("should not activate when core isolation is enabled")
	}
}

func TestClusterStatusHelpers(t *testing.T) {
	active := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{Status: "active"},
	}
	if !ClusterAlreadyActive(active) {
		t.Fatalf("ClusterAlreadyActive should be true")
	}

	expanding := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		Status: simplyblockv1alpha1.SimplyBlockStorageClusterStatus{Status: "in_expansion"},
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

	clusterA := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: "cluster-a"},
		Status:     simplyblockv1alpha1.SimplyBlockStorageClusterStatus{UUID: "uuid-a"},
	}
	clusterNoUUID := &simplyblockv1alpha1.SimplyBlockStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns1"},
		Spec:       simplyblockv1alpha1.SimplyBlockStorageClusterSpec{ClusterName: "cluster-b"},
	}

	poolA := &simplyblockv1alpha1.SimplyBlockPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns1"},
		Spec: simplyblockv1alpha1.SimplyBlockPoolSpec{
			ClusterName: "cluster-a",
			Name:        "gold",
		},
		Status: simplyblockv1alpha1.SimplyBlockPoolStatus{UUID: "pool-uuid-a"},
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

	exists, uuid, name, err := ExistingClusterUUID(ctx, c, "ns1")
	if err != nil {
		t.Fatalf("ExistingClusterUUID unexpected error: %v", err)
	}
	if !exists || uuid != "uuid-a" || name != "cluster-a" {
		t.Fatalf("ExistingClusterUUID unexpected result: exists=%v uuid=%q name=%q", exists, uuid, name)
	}
}
