package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	snsTestNS      = "test"
	snsTestCluster = "cluster-a"
)

func newSNSReconciler(t *testing.T, objects ...client.Object) *StorageNodeSetReconciler {
	t.Helper()
	// corev1: the reconciler reads/writes the per-node ConfigMap and lists Nodes.
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
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
	name := storageNodeCRName("my-sns")
	if name == "" {
		t.Fatal("expected non-empty name")
	}
	if len(name) > 63 {
		t.Errorf("name exceeds 63 chars: %q (%d)", name, len(name))
	}
	if name != strings.ToLower(name) {
		t.Errorf("name is not lowercase: %q", name)
	}
	if !strings.HasPrefix(name, "my-sns-") {
		t.Errorf("name %q does not carry the sns prefix", name)
	}
}

func TestStorageNodeCRName_TruncatesLongNames(t *testing.T) {
	longSNS := "simplyblock-node-" + strings.Repeat("a", 80)
	name := storageNodeCRName(longSNS)
	if len(name) > 63 {
		t.Errorf("name exceeds 63 chars: len=%d", len(name))
	}
}

func TestStorageNodeCRName_IsRandomPerCall(t *testing.T) {
	// The id suffix is random, so repeated calls must (with overwhelming
	// probability) produce distinct names — this is what the create-retry loop
	// relies on to resolve collisions.
	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		seen[storageNodeCRName("sns")] = struct{}{}
	}
	if len(seen) < 50 {
		t.Errorf("expected 50 distinct random names, got %d", len(seen))
	}
}

func TestStorageNodeCRName_IsDNSLabelSafe(t *testing.T) {
	name := storageNodeCRName("my-sns")
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

// newSizingStorageCluster returns a StorageCluster carrying the cluster-scoped
// node sizing values that buildPerNodeEnvFile reads.
func newSizingStorageCluster(maxSubsys, vcpuCount *int32, maxHugePages string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: snsTestCluster, Namespace: snsTestNS},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			MaxSubsystemCount: maxSubsys,
			VCPUCount:         vcpuCount,
			MaxHugePagesSize:  maxHugePages,
		},
	}
}

func TestBuildPerNodeEnvFile_UsesClusterSizingValues(t *testing.T) {
	maxSubsys := int32(20)
	vcpuCount := int32(8)
	cluster := newSizingStorageCluster(&maxSubsys, &vcpuCount, "100G")
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName:      snsTestCluster,
			SpdkSystemMemory: "4G",
		},
	}
	env := buildPerNodeEnvFile(cluster, sns, "worker-a.example.com")
	for _, want := range []string{"MAX_SUBSYS_COUNT=20", "VCPU_COUNT=8", "MAX_HUGE_PAGES_SIZE='100G'"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %q in env:\n%s", want, env)
		}
	}
}

// Cluster sizing values are not overridable per node: nodeConfigs may narrow
// device selection but never the huge-page or core layout.
func TestBuildPerNodeEnvFile_ClusterSizingIdenticalAcrossWorkers(t *testing.T) {
	maxSubsys := int32(20)
	vcpuCount := int32(8)
	cluster := newSizingStorageCluster(&maxSubsys, &vcpuCount, "")
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: snsTestCluster,
			NodeConfigs: map[string]simplyblockv1alpha1.StorageNodeOverrides{
				"worker-b": {DriveSizeRange: "50G-1T"},
			},
		},
	}
	plain := buildPerNodeEnvFile(cluster, sns, "worker-a")
	overridden := buildPerNodeEnvFile(cluster, sns, "worker-b")

	for _, want := range []string{"MAX_SUBSYS_COUNT=20", "VCPU_COUNT=8"} {
		if !strings.Contains(plain, want) || !strings.Contains(overridden, want) {
			t.Errorf("expected %q in both entries:\n%s\n---\n%s", want, plain, overridden)
		}
	}
	if !strings.Contains(overridden, "SIZE_RANGE='50G-1T'") {
		t.Errorf("expected per-node driveSizeRange override to apply:\n%s", overridden)
	}
}

// maxSubsystemCount and vcpuCount are required by the CRD schema; a cluster
// missing them (admitted before that requirement) must not yield a ConfigMap the
// node cannot boot from, since an empty MAX_SUBSYS_COUNT only fails later inside
// node_configure.py.
func TestReconcilePerNodeConfigMap_RejectsClusterMissingRequiredSizing(t *testing.T) {
	vcpuCount := int32(8)
	cases := map[string]*simplyblockv1alpha1.StorageCluster{
		"both unset":        newSizingStorageCluster(nil, nil, ""),
		"maxSubsystemCount": newSizingStorageCluster(nil, &vcpuCount, ""),
		"vcpuCount":         newSizingStorageCluster(ptr.To(int32(20)), nil, ""),
	}
	for name, cluster := range cases {
		t.Run(name, func(t *testing.T) {
			sns := &simplyblockv1alpha1.StorageNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
				Spec: simplyblockv1alpha1.StorageNodeSetSpec{
					ClusterName: snsTestCluster,
					WorkerNodes: []string{"worker-a"},
				},
			}
			r := newSNSReconciler(t, cluster, sns)
			err := r.reconcilePerNodeConfigMap(context.Background(), sns)
			if err == nil {
				t.Fatal("expected an error for a cluster missing required node sizing")
			}
			if !strings.Contains(err.Error(), "maxSubsystemCount") {
				t.Errorf("error should name the fields to set, got: %v", err)
			}
		})
	}
}

func TestReconcilePerNodeConfigMap_WritesClusterSizingForEveryWorker(t *testing.T) {
	cluster := newSizingStorageCluster(ptr.To(int32(20)), ptr.To(int32(8)), "")
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: snsTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: snsTestCluster,
			WorkerNodes: []string{"worker-a", "worker-b"},
		},
	}
	r := newSNSReconciler(t, cluster, sns)
	if err := r.reconcilePerNodeConfigMap(context.Background(), sns); err != nil {
		t.Fatalf("reconcilePerNodeConfigMap: %v", err)
	}

	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), types.NamespacedName{
		Name:      PerNodeConfigMapName(sns.Name),
		Namespace: snsTestNS,
	}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	for _, worker := range []string{"worker-a", "worker-b"} {
		entry, ok := cm.Data[worker]
		if !ok {
			t.Fatalf("no entry for %s", worker)
		}
		for _, want := range []string{"MAX_SUBSYS_COUNT=20", "VCPU_COUNT=8"} {
			if !strings.Contains(entry, want) {
				t.Errorf("%s: missing %q in:\n%s", worker, want, entry)
			}
		}
	}
}

func TestBuildPerNodeEnvFile_ContainsAllRequiredKeys(t *testing.T) {
	cluster := newSizingStorageCluster(ptr.To(int32(20)), ptr.To(int32(8)), "")
	sns := &simplyblockv1alpha1.StorageNodeSet{
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: snsTestCluster},
	}
	env := buildPerNodeEnvFile(cluster, sns, "any-worker")
	required := []string{"MAX_SUBSYS_COUNT=", "MAX_HUGE_PAGES_SIZE=", "VCPU_COUNT=",
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

	count, err := r.countInFlightNodes(context.Background(), snsTestNS, "sns", "worker-1.example.com")
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

	count, err := r.countInFlightNodes(context.Background(), snsTestNS, "sns", "worker-1.example.com")
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

	count, err := r.countInFlightNodes(context.Background(), snsTestNS, "sns", "worker-1.example.com")
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
