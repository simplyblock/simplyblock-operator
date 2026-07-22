package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	snsTestNS      = "test"
	snsTestCluster = "cluster-a"
)

func newSNSReconciler(t *testing.T, objects ...client.Object) *StorageNodeSetReconciler {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.StorageNode{},
			&simplyblockv1alpha1.StorageNodeSet{},
		).
		WithObjects(objects...).
		WithIndex(&simplyblockv1alpha1.StorageNode{}, "spec.storageNodeSetRef", func(obj client.Object) []string {
			sn := obj.(*simplyblockv1alpha1.StorageNode)
			return []string{sn.Spec.StorageNodeSetRef}
		}).
		Build()
	return &StorageNodeSetReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(16),
	}
}

// ── TestStorageNodeCRName ──────────────────────────────────────────────────────

func TestStorageNodeCRName_SimpleCase(t *testing.T) {
	name := storageNodeCRName("my-sns", "worker-a.example.com", "0", 0)
	if name == "" {
		t.Fatal("expected non-empty name")
	}
	if len(name) > 63 {
		t.Errorf("name exceeds 63 chars: %q (%d)", name, len(name))
	}
	// Must be lowercase
	if name != strings.ToLower(name) {
		t.Errorf("name is not lowercase: %q", name)
	}
}

func TestStorageNodeCRName_TruncatesLongNames(t *testing.T) {
	longWorker := "vm" + strings.Repeat("a", 60) + ".simplyblock3.localdomain"
	name := storageNodeCRName("simplyblock-node", longWorker, "0", 0)
	if len(name) > 63 {
		t.Errorf("truncated name still exceeds 63 chars: len=%d", len(name))
	}
}

func TestStorageNodeCRName_CollisionGuard(t *testing.T) {
	// Two workers sharing a long prefix must produce distinct names.
	base := "vm" + strings.Repeat("x", 55) + ".example.com"
	name1 := storageNodeCRName("sns", base+"1", "0", 0)
	name2 := storageNodeCRName("sns", base+"2", "0", 0)
	if name1 == name2 {
		t.Errorf("collision: both workers mapped to %q", name1)
	}
}

func TestStorageNodeCRName_IsDNSLabelSafe(t *testing.T) {
	name := storageNodeCRName("my-sns", "vm01.simplyblock3.localdomain", "0", 0)
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			t.Errorf("invalid character %q in name %q", c, name)
		}
	}
}

// ── TestSanitiseDNSLabel ───────────────────────────────────────────────────────

func TestSanitiseDNSLabel_ReplacesInvalidChars(t *testing.T) {
	got := sanitiseDNSLabel("vm_01.EXAMPLE.com")
	if strings.ContainsAny(got, "_ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("unsanitised result: %q", got)
	}
}

func TestSanitiseDNSLabel_StripsLeadingTrailingHyphens(t *testing.T) {
	got := sanitiseDNSLabel("-bad-label-")
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("result has leading/trailing hyphen: %q", got)
	}
}

// ── TestBuildPerNodeEnvFile ───────────────────────────────────────────────────

func TestBuildPerNodeEnvFile_UsesFleetDefaults(t *testing.T) {
	maxLvol := int32(20)
	corePercent := int32(50)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName:           snsTestCluster,
			MaxLogicalVolumeCount: &maxLvol,
			CorePercentage:        &corePercent,
			SpdkSystemMemory:      "4G",
		},
	}
	env := buildPerNodeEnvFile(sns, "worker-a.example.com")
	if !strings.Contains(env, "MAX_LVOL=20") {
		t.Errorf("missing MAX_LVOL=20 in env:\n%s", env)
	}
	if !strings.Contains(env, "CORES_PERCENTAGE=50") {
		t.Errorf("missing CORES_PERCENTAGE=50 in env:\n%s", env)
	}
}

func TestBuildPerNodeEnvFile_OverrideWinsOverFleet(t *testing.T) {
	fleetMax := int32(20)
	overrideMax := int32(99)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName:           snsTestCluster,
			MaxLogicalVolumeCount: &fleetMax,
			NodeConfigs: map[string]simplyblockv1alpha1.StorageNodeOverrides{
				"worker-b": {MaxLogicalVolumeCount: &overrideMax},
			},
		},
	}
	env := buildPerNodeEnvFile(sns, "worker-b")
	if !strings.Contains(env, "MAX_LVOL=99") {
		t.Errorf("expected MAX_LVOL=99 (override), got:\n%s", env)
	}
}

func TestBuildPerNodeEnvFile_WorkerNotInNodeConfigs_UsesFleet(t *testing.T) {
	maxLvol := int32(15)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName:           snsTestCluster,
			MaxLogicalVolumeCount: &maxLvol,
		},
	}
	env := buildPerNodeEnvFile(sns, "worker-not-configured")
	if !strings.Contains(env, "MAX_LVOL=15") {
		t.Errorf("expected fleet MAX_LVOL=15:\n%s", env)
	}
}

func TestBuildPerNodeEnvFile_ContainsAllRequiredKeys(t *testing.T) {
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: snsTestCluster},
	}
	env := buildPerNodeEnvFile(sns, "any-worker")
	required := []string{"MAX_LVOL=", "MAX_SIZE=", "CORES_PERCENTAGE=",
		"RESERVED_SYSTEM_CPUS=", "CPU_TOPOLOGY_ENABLED=",
		"PCI_ALLOWED=", "PCI_BLOCKED=", "NVME_DEVICES=",
		"DEVICE_MODEL=", "SIZE_RANGE=", "JM_PERCENT=", "HA_JM_COUNT="}
	for _, key := range required {
		if !strings.Contains(env, key) {
			t.Errorf("missing key %q in env:\n%s", key, env)
		}
	}
}

// ── TestCountInFlightNodes ────────────────────────────────────────────────────

func TestCountInFlightNodes_ZeroWhenNonePosted(t *testing.T) {
	sn1 := newStorageNode("sn-1", snsTestNS, "sns", "worker-1.example.com")
	sn2 := newStorageNode("sn-2", snsTestNS, "sns", "worker-2.example.com")
	r := newSNReconciler(t, sn1, sn2)

	count, err := r.countInFlightNodes(context.Background(), snsTestNS, "sns", "sn-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 in-flight, got %d", count)
	}
}

func TestCountInFlightNodes_CountsSiblingsWithPostedAtAndNoUUID(t *testing.T) {
	now := metav1.Now()
	sn1 := newStorageNode("sn-1", snsTestNS, "sns", "worker-1.example.com")
	sn2 := newStorageNode("sn-2", snsTestNS, "sns", "worker-2.example.com")
	sn2.Status.PostedAt = &now // sn-2 is in-flight
	sn3 := newStorageNode("sn-3", snsTestNS, "sns", "worker-3.example.com")
	sn3.Status.PostedAt = &now
	sn3.Status.UUID = "already-online-uuid" // sn-3 is done
	r := newSNReconciler(t, sn1, sn2, sn3)

	count, err := r.countInFlightNodes(context.Background(), snsTestNS, "sns", "sn-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 in-flight (sn-2), got %d", count)
	}
}

func TestCountInFlightNodes_ExcludesSelf(t *testing.T) {
	now := metav1.Now()
	sn1 := newStorageNode("sn-1", snsTestNS, "sns", "worker-1.example.com")
	sn1.Status.PostedAt = &now // self is in-flight
	r := newSNReconciler(t, sn1)

	count, err := r.countInFlightNodes(context.Background(), snsTestNS, "sns", "sn-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("self should not be counted, got %d", count)
	}
}

// ── TestSyncUUIDFromNodeSet ───────────────────────────────────────────────────

func TestSyncUUIDFromNodeSet_CopiesUUIDWhenFound(t *testing.T) {
	sn := newStorageNode("sn-1", snsTestNS, "sns", snTestWorker)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: snTestWorker, UUID: "backend-uuid-123", Status: utils.NodeStatusOnline, Health: true},
			},
		},
	}
	r := newSNReconciler(t, sn, sns)

	if err := r.syncUUIDFromNodeSet(context.Background(), sn, sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: snsTestNS}, &updated)
	if updated.Status.UUID != "backend-uuid-123" {
		t.Errorf("UUID not synced: got %q", updated.Status.UUID)
	}
	if updated.Status.Status != utils.NodeStatusOnline {
		t.Errorf("status not synced: got %q", updated.Status.Status)
	}
}

func TestSyncUUIDFromNodeSet_NoopWhenWorkerNotInNodes(t *testing.T) {
	sn := newStorageNode("sn-1", snsTestNS, "sns", snTestWorker)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "other-worker.example.com", UUID: "other-uuid"},
			},
		},
	}
	r := newSNReconciler(t, sn, sns)

	if err := r.syncUUIDFromNodeSet(context.Background(), sn, sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: snsTestNS}, &updated)
	if updated.Status.UUID != "" {
		t.Errorf("UUID should remain empty, got %q", updated.Status.UUID)
	}
}

func TestSyncUUIDFromNodeSet_SkipsEmptyUUID(t *testing.T) {
	sn := newStorageNode("sn-1", snsTestNS, "sns", snTestWorker)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: snTestWorker, UUID: ""}, // placeholder, not yet online
			},
		},
	}
	r := newSNReconciler(t, sn, sns)

	if err := r.syncUUIDFromNodeSet(context.Background(), sn, sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: snsTestNS}, &updated)
	if updated.Status.UUID != "" {
		t.Errorf("UUID should remain empty when node entry has empty UUID, got %q", updated.Status.UUID)
	}
}

// ── TestSyncManualStorageNodeStatus ───────────────────────────────────────────

func TestSyncManualStorageNodeStatus_AddsManualNodeToSNSStatus(t *testing.T) {
	// A StorageNode without OwnerReference (manual) that has a UUID
	sn := newStorageNode("manual-sn", snsTestNS, "sns", "manual-worker.example.com")
	sn.Status.UUID = "manual-uuid-456"
	sn.Status.Status = utils.NodeStatusOnline
	sn.Status.Health = true

	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: snsTestCluster},
		// WorkerNodes does NOT contain manual-worker
	}
	r := newSNSReconciler(t, sn, sns)

	if err := r.syncManualStorageNodeStatus(context.Background(), sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeSet
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sns", Namespace: snsTestNS}, &updated)

	found := false
	for _, n := range updated.Status.Nodes {
		if n.UUID == "manual-uuid-456" {
			found = true
			if n.Status != utils.NodeStatusOnline {
				t.Errorf("status not synced: got %q", n.Status)
			}
		}
	}
	if !found {
		t.Error("manual StorageNode UUID not added to StorageNodeSet.status.nodes[]")
	}
}

func TestSyncManualStorageNodeStatus_SkipsUnprovisionedNodes(t *testing.T) {
	sn := newStorageNode("manual-sn", snsTestNS, "sns", "manual-worker.example.com")
	// UUID is empty — not yet provisioned

	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: snsTestCluster},
	}
	r := newSNSReconciler(t, sn, sns)

	if err := r.syncManualStorageNodeStatus(context.Background(), sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeSet
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sns", Namespace: snsTestNS}, &updated)
	if len(updated.Status.Nodes) != 0 {
		t.Errorf("expected empty status.nodes[], got %d entries", len(updated.Status.Nodes))
	}
}

func TestSyncManualStorageNodeStatus_SkipsWorkerInSpecWorkerNodes(t *testing.T) {
	// Worker is in spec.workerNodes — it's operator-managed, not manual
	sn := newStorageNode("managed-sn", snsTestNS, "sns", snTestWorker)
	sn.Status.UUID = "managed-uuid"

	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: snsTestCluster,
			WorkerNodes: []string{snTestWorker}, // operator-managed
		},
	}
	r := newSNSReconciler(t, sn, sns)

	if err := r.syncManualStorageNodeStatus(context.Background(), sns); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeSet
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sns", Namespace: snsTestNS}, &updated)
	if len(updated.Status.Nodes) != 0 {
		t.Errorf("operator-managed node should not be added by syncManualStorageNodeStatus")
	}
}

func TestSyncManualStorageNodeStatus_IdempotentOnSecondCall(t *testing.T) {
	sn := newStorageNode("manual-sn", snsTestNS, "sns", "manual-worker.example.com")
	sn.Status.UUID = "manual-uuid-789"
	sn.Status.Status = utils.NodeStatusOnline

	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: snsTestCluster},
	}
	r := newSNSReconciler(t, sn, sns)

	// First call
	_ = r.syncManualStorageNodeStatus(context.Background(), sns)

	// Re-fetch and call again
	var sns2 simplyblockv1alpha1.StorageNodeSet
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sns", Namespace: snsTestNS}, &sns2)
	_ = r.syncManualStorageNodeStatus(context.Background(), &sns2)

	var updated simplyblockv1alpha1.StorageNodeSet
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sns", Namespace: snsTestNS}, &updated)
	count := 0
	for _, n := range updated.Status.Nodes {
		if n.UUID == "manual-uuid-789" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry for manual node, got %d (not idempotent)", count)
	}
}
