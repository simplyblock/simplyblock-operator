package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	snTestNS      = "test"
	snTestCluster = "cluster-a"
	snTestWorker  = "worker-1.example.com"
)

func newSNReconciler(t *testing.T, objects ...client.Object) *StorageNodeReconciler {
	t.Helper()
	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
	)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.StorageNode{},
			&simplyblockv1alpha1.StorageNodeOps{},
			&simplyblockv1alpha1.StorageCluster{},
			&simplyblockv1alpha1.StorageNodeSet{},
		).
		WithObjects(objects...).
		WithIndex(&simplyblockv1alpha1.StorageNode{}, "spec.storageNodeSetRef", func(obj client.Object) []string {
			sn := obj.(*simplyblockv1alpha1.StorageNode)
			return []string{sn.Spec.StorageNodeSetRef}
		}).
		WithIndex(&simplyblockv1alpha1.StorageNode{}, "spec.workerNode", func(obj client.Object) []string {
			sn := obj.(*simplyblockv1alpha1.StorageNode)
			return []string{sn.Spec.WorkerNode}
		}).
		Build()
	return &StorageNodeReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(16),
	}
}

//nolint:unparam
func newStorageNodeSet(name, ns, cluster string, nodeConfigs map[string]simplyblockv1alpha1.StorageNodeOverrides) *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: cluster,
			WorkerNodes: []string{snTestWorker},
			NodeConfigs: nodeConfigs,
		},
	}
}

//nolint:unparam
func newStorageNode(name, ns, snsRef, worker string) *simplyblockv1alpha1.StorageNode {
	return &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: snsRef,
			WorkerNode:        worker,
		},
	}
}

// ── TestSyncOverrides ─────────────────────────────────────────────────────────

func TestSyncOverrides_PropagatesNodeConfigs(t *testing.T) {
	maxLvol := int32(99)
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, map[string]simplyblockv1alpha1.StorageNodeOverrides{
		snTestWorker: {MaxSubsystemCount: &maxLvol, SpdkSystemMemory: "8G"},
	})
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	r := newSNReconciler(t, sns, sn)

	if err := r.syncOverrides(context.Background(), sn, sns); err != nil {
		t.Fatalf("syncOverrides returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	if err := r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: snTestNS}, &updated); err != nil {
		t.Fatalf("failed to fetch updated StorageNode: %v", err)
	}
	if updated.Spec.Overrides == nil {
		t.Fatal("expected Overrides to be set")
	}
	if updated.Spec.Overrides.SpdkSystemMemory != "8G" {
		t.Errorf("SpdkSystemMemory: got %q want %q", updated.Spec.Overrides.SpdkSystemMemory, "8G")
	}
	if *updated.Spec.Overrides.MaxSubsystemCount != 99 {
		t.Errorf("MaxSubsystemCount: got %d want 99", *updated.Spec.Overrides.MaxSubsystemCount)
	}
}

func TestSyncOverrides_NoopWhenWorkerNotInNodeConfigs(t *testing.T) {
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	r := newSNReconciler(t, sns, sn)

	if err := r.syncOverrides(context.Background(), sn, sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: snTestNS}, &updated)
	if updated.Spec.Overrides != nil {
		t.Error("expected Overrides to remain nil when worker not in nodeConfigs")
	}
}

// ── TestEffectiveNodeConfig ───────────────────────────────────────────────────

func TestEffectiveNodeConfig_OverridesTakePrecedence(t *testing.T) {
	fleetMem := "4G"
	overrideMem := "16G"
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{SpdkSystemMemory: fleetMem},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			Overrides: &simplyblockv1alpha1.StorageNodeOverrides{SpdkSystemMemory: overrideMem},
		},
	}
	eff := effectiveNodeConfig(sn, sns)
	if eff.SpdkSystemMemory != overrideMem {
		t.Errorf("expected override %q, got %q", overrideMem, eff.SpdkSystemMemory)
	}
}

func TestEffectiveNodeConfig_FallsBackToFleetDefault(t *testing.T) {
	fleetMem := "4G"
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{SpdkSystemMemory: fleetMem},
	}
	sn := &simplyblockv1alpha1.StorageNode{} // no overrides
	eff := effectiveNodeConfig(sn, sns)
	if eff.SpdkSystemMemory != fleetMem {
		t.Errorf("expected fleet default %q, got %q", fleetMem, eff.SpdkSystemMemory)
	}
}

// ── TestEffectiveFailureDomain ────────────────────────────────────────────────

func TestEffectiveFailureDomain_OverrideTakesPrecedenceOverMap(t *testing.T) {
	fd := int32(3)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			NodeFailureDomains: map[string]int32{snTestWorker: 1},
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			WorkerNode: snTestWorker,
			Overrides:  &simplyblockv1alpha1.StorageNodeOverrides{FailureDomain: &fd},
		},
	}
	if got := effectiveFailureDomain(sn, sns); got != 3 {
		t.Errorf("expected 3 from override, got %d", got)
	}
}

func TestEffectiveFailureDomain_FallsBackToMap(t *testing.T) {
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			NodeFailureDomains: map[string]int32{snTestWorker: 2},
		},
	}
	sn := &simplyblockv1alpha1.StorageNode{
		Spec: simplyblockv1alpha1.StorageNodeSpec{WorkerNode: snTestWorker},
	}
	if got := effectiveFailureDomain(sn, sns); got != 2 {
		t.Errorf("expected 2 from map, got %d", got)
	}
}

func TestEffectiveFailureDomain_ZeroWhenNotSet(t *testing.T) {
	sns := &simplyblockv1alpha1.StorageNodeSet{}
	sn := &simplyblockv1alpha1.StorageNode{
		Spec: simplyblockv1alpha1.StorageNodeSpec{WorkerNode: snTestWorker},
	}
	if got := effectiveFailureDomain(sn, sns); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

// ── TestCheckFailureDomain ────────────────────────────────────────────────────

func TestCheckFailureDomain_BlocksWhenEnabledAndNotSet(t *testing.T) {
	enabled := true
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: snTestCluster, Namespace: snTestNS},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{EnableFailureDomains: &enabled},
	}
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	r := newSNReconciler(t, cluster, sns, sn)

	err := r.checkFailureDomain(context.Background(), sn, sns)
	if err == nil {
		t.Fatal("expected error when failureDomain not set and enableFailureDomains=true")
	}
}

func TestCheckFailureDomain_AllowsWhenFailureDomainSet(t *testing.T) {
	enabled := true
	fd := int32(1)
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: snTestCluster, Namespace: snTestNS},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{EnableFailureDomains: &enabled},
	}
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	sn.Spec.Overrides = &simplyblockv1alpha1.StorageNodeOverrides{FailureDomain: &fd}
	r := newSNReconciler(t, cluster, sns, sn)

	if err := r.checkFailureDomain(context.Background(), sn, sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckFailureDomain_SkipsWhenFeatureDisabled(t *testing.T) {
	disabled := false
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: snTestCluster, Namespace: snTestNS},
		Spec:       simplyblockv1alpha1.StorageClusterSpec{EnableFailureDomains: &disabled},
	}
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker) // no failureDomain
	r := newSNReconciler(t, cluster, sns, sn)

	if err := r.checkFailureDomain(context.Background(), sn, sns); err != nil {
		t.Fatalf("expected no error when feature disabled, got: %v", err)
	}
}

// ── TestEnsureRemoveOps ───────────────────────────────────────────────────────

func TestEnsureRemoveOps_CreatesOpsWhenMissing(t *testing.T) {
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	sn.Status.UUID = "uuid-1"
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	r := newSNReconciler(t, sn, sns)

	if err := r.ensureRemoveOps(context.Background(), sn); err != nil {
		t.Fatalf("ensureRemoveOps returned error: %v", err)
	}

	var ops simplyblockv1alpha1.StorageNodeOps
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "sn-1-remove", Namespace: snTestNS,
	}, &ops); err != nil {
		t.Fatalf("expected StorageNodeOps to be created: %v", err)
	}
	if ops.Spec.Action != "remove" {
		t.Errorf("expected action=remove, got %q", ops.Spec.Action)
	}
	if ops.Spec.StorageNodeRef != "sn-1" {
		t.Errorf("expected storageNodeRef=sn-1, got %q", ops.Spec.StorageNodeRef)
	}
}

func TestEnsureRemoveOps_IdempotentWhenAlreadyExists(t *testing.T) {
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	sn.Status.UUID = "uuid-1"
	existingOps := &simplyblockv1alpha1.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-1-remove", Namespace: snTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeOpsSpec{StorageNodeRef: "sn-1", Action: "remove"},
	}
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	r := newSNReconciler(t, sn, sns, existingOps)

	// Should not return an error on second call.
	if err := r.ensureRemoveOps(context.Background(), sn); err != nil {
		t.Fatalf("ensureRemoveOps should be idempotent, got: %v", err)
	}
}

// ── TestHandleDeletion ────────────────────────────────────────────────────────

func TestHandleDeletion_RemovesFinalizerWhenNeverProvisioned(t *testing.T) {
	sn := newStorageNode("sn-1", snTestNS, "sns", snTestWorker)
	sn.Finalizers = []string{storageNodeFinalizer}
	// status.UUID is empty — node was never provisioned
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, nil)
	r := newSNReconciler(t, sn, sns)

	_, err := r.handleDeletion(context.Background(), sn, sns)
	if err != nil {
		t.Fatalf("handleDeletion returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: snTestNS}, &updated)
	for _, f := range updated.Finalizers {
		if f == storageNodeFinalizer {
			t.Error("finalizer should have been removed for unprovisioned node")
		}
	}
}

// TestCountInFlightNodes_DeduplicatesByWorker verifies that countInFlightNodes
// counts distinct physical hosts (WorkerNode), not individual StorageNode CRs.
// With nodesPerSocket=2 each host has two CRs; both get PostedAt stamped
// (primary posts, secondary copies via workerAlreadyPosted fast-path). Without
// deduplication each host would consume 2 slots instead of 1, causing
// maxParallelNodeAdds=6 to allow only ~3 hosts concurrently.
func TestCountInFlightNodes_DeduplicatesByWorker(t *testing.T) {
	const (
		ns     = snTestNS
		snsRef = "sns-dedup"
	)

	now := metav1.Now()
	postedAt := &now

	makeNode := func(name, worker, uuid, status string, posted *metav1.Time) *simplyblockv1alpha1.StorageNode {
		sn := newStorageNode(name, ns, snsRef, worker)
		sn.Status.PostedAt = posted
		sn.Status.UUID = uuid
		sn.Status.Status = status
		return sn
	}

	// Two sockets on worker-A: both in-flight (PostedAt set, no UUID yet).
	snA1 := makeNode("sn-a1", "worker-a", "", "", postedAt)
	snA2 := makeNode("sn-a2", "worker-a", "", "", postedAt)

	// Two sockets on worker-B: both in-flight.
	snB1 := makeNode("sn-b1", "worker-b", "", "", postedAt)
	snB2 := makeNode("sn-b2", "worker-b", "", "", postedAt)

	// worker-C: one socket done (UUID assigned), one still in_creation.
	snC1 := makeNode("sn-c1", "worker-c", "uuid-c", utils.NodeStatusInCreation, postedAt)
	snC2 := makeNode("sn-c2", "worker-c", "uuid-c", utils.NodeStatusInCreation, postedAt)

	// worker-D: timed out — should NOT count.
	snD1 := makeNode("sn-d1", "worker-d", "", utils.NodeStatusTimeout, postedAt)

	// worker-E: no PostedAt — not yet started, should NOT count.
	snE1 := makeNode("sn-e1", "worker-e", "", "", nil)

	r := newSNReconciler(t, snA1, snA2, snB1, snB2, snC1, snC2, snD1, snE1)

	t.Run("counts distinct in-flight workers, not CRs", func(t *testing.T) {
		// Exclude worker-a (the calling node's worker). Expect worker-b and worker-c = 2.
		count, err := r.countInFlightNodes(context.Background(), ns, snsRef, "worker-a")
		if err != nil {
			t.Fatalf("countInFlightNodes returned error: %v", err)
		}
		if count != 2 {
			t.Fatalf("expected 2 distinct in-flight workers (b, c), got %d", count)
		}
	})

	t.Run("excludes the calling node's own worker", func(t *testing.T) {
		// Exclude worker-b. Expect worker-a and worker-c = 2.
		count, err := r.countInFlightNodes(context.Background(), ns, snsRef, "worker-b")
		if err != nil {
			t.Fatalf("countInFlightNodes returned error: %v", err)
		}
		if count != 2 {
			t.Fatalf("expected 2 distinct in-flight workers (a, c), got %d", count)
		}
	})
}
