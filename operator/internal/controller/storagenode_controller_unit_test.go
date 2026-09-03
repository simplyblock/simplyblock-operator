package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
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
	sns := newStorageNodeSet("sns", snTestNS, snTestCluster, map[string]simplyblockv1alpha1.StorageNodeOverrides{
		snTestWorker: {DriveSizeRange: "50G-1T", SpdkSystemMemory: "8G"},
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
	if updated.Spec.Overrides.DriveSizeRange != "50G-1T" {
		t.Errorf("DriveSizeRange: got %q want %q", updated.Spec.Overrides.DriveSizeRange, "50G-1T")
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

	_, err := r.handleDeletion(context.Background(), sn, snTestCluster)
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

// The reconciler serves a node's status from the stream's cache, so a change
// the control plane pushed costs no request to read back. The server here
// fails every request and counts them: a reconciler that polled would both
// call it and fail to write a status.
func TestSyncStatusReadsThePushedNodeInsteadOfAskingForIt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	const nodeUUID = "fd687dfd-9b5d-4eca-8cb1-23bcf550ad21"
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "simplyblock-node-asxeub"},
		Status:     simplyblockv1alpha1.StorageNodeStatus{UUID: nodeUUID},
	}

	// The stream reported the node online, as its snapshot would.
	sub := subscriptions.NewNodeSubscription()
	sub.RegisterNode(nodeUUID, client.ObjectKeyFromObject(sn))
	err := sub.Ingest(context.Background(), cpinformer.Event{
		Kind:  cpinformer.EventSnapshot,
		Scope: cpinformer.Scope{"22222222-2222-2222-2222-222222222222"},
		Data: []byte(`[{"id":"` + nodeUUID + `","status":"online","mgmt_ip":"192.168.10.112",
			"health_check":true,"hostname":"vm02_4420","cpu_spdk_count":6,"lvols":3,
			"rpc_port":4420,"lvol_subsys_port":4426,"nvmf_port":4421,"failure_domain":-1}]`),
	})
	if err != nil {
		t.Fatalf("ingest snapshot: %v", err)
	}

	r := newSNReconciler(t, sn)
	r.Nodes = sub

	res, err := r.syncStatus(context.Background(), sn, "22222222-2222-2222-2222-222222222222",
		webapi.NewClient(srv.URL))
	if err != nil {
		t.Fatalf("syncStatus: %v", err)
	}

	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("made %d control-plane request(s), want none for a node the stream already delivered", n)
	}

	var got simplyblockv1alpha1.StorageNode
	key := client.ObjectKeyFromObject(sn)
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status.Status != "online" || !got.Status.Health {
		t.Errorf("status/health = %q/%v, want online/true", got.Status.Status, got.Status.Health)
	}
	if got.Status.Hostname != "vm02_4420" {
		t.Errorf("hostname = %q", got.Status.Hostname)
	}
	if got.Status.Ports == nil || got.Status.Ports.Management != "192.168.10.112" {
		t.Errorf("ports = %+v", got.Status.Ports)
	}
	if got.Status.Resources == nil || *got.Status.Resources.Volumes != 3 {
		t.Errorf("resources = %+v", got.Status.Resources)
	}

	// The poll survives as the correctness floor, relaxed because it is no
	// longer how a change is noticed.
	if res.RequeueAfter < time.Minute {
		t.Errorf("RequeueAfter = %s, want the relaxed backstop rather than a poll", res.RequeueAfter)
	}
}

// A node the stream has not delivered still has to be readable, or a cold cache
// would leave the CR's status frozen until the first snapshot lands.
func TestSyncStatusFallsBackToTheControlPlaneForAnUncachedNode(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"aaaa","status":"in_creation","health_check":false,
			"hostname":"vm09_4420","cpu_spdk_count":4,"lvols":0,"mgmt_ip":"10.0.0.9",
			"rpc_port":4420,"lvol_subsys_port":4426,"nvmf_port":4421,"failure_domain":-1}`))
	}))
	defer srv.Close()

	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "simplyblock-node-new"},
		Status:     simplyblockv1alpha1.StorageNodeStatus{UUID: "aaaa"},
	}

	r := newSNReconciler(t, sn)
	r.Nodes = subscriptions.NewNodeSubscription() // connected, nothing delivered yet

	if _, err := r.syncStatus(context.Background(), sn, "cluster-1", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("syncStatus: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("made %d request(s), want exactly 1 for a node the stream has not delivered", n)
	}

	var got simplyblockv1alpha1.StorageNode
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status.Status != nodeStatusInCreation || got.Status.Hostname != "vm09_4420" {
		t.Errorf("status/hostname = %q/%q", got.Status.Status, got.Status.Hostname)
	}
}

// fakeNodeCapacity stands in for the metrics endpoint.
type fakeNodeCapacity struct {
	samples map[string]prometheus.Capacity
	err     error
}

func (f *fakeNodeCapacity) NodeCapacity(
	_ context.Context, _ string,
) (map[string]prometheus.Capacity, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.samples, nil
}

const (
	snCapNode  = "fd687dfd-9b5d-4eca-8cb1-23bcf550ad21"
	snCapTotal = int64(112303538176)
	snCapUsed  = int64(422576128)
)

func nodeWithUUID() *simplyblockv1alpha1.StorageNode {
	return &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "simplyblock-node-cap"},
		Status:     simplyblockv1alpha1.StorageNodeStatus{UUID: snCapNode},
	}
}

func nodeRecording(used, total int64) *simplyblockv1alpha1.StorageNode {
	sn := nodeWithUUID()
	sn.Status.Resources = &simplyblockv1alpha1.StorageNodeResources{
		Capacity: &simplyblockv1alpha1.StorageNodeCapacity{
			TotalBytes: ptr.To(total),
			UsedBytes:  ptr.To(used),
		},
	}
	return sn
}

func applyWithCapacity(
	t *testing.T, sn *simplyblockv1alpha1.StorageNode, src NodeCapacitySource,
) simplyblockv1alpha1.StorageNode {
	t.Helper()
	r := newSNReconciler(t, sn)
	r.Capacity = src
	if _, err := r.applyNodeStatus(context.Background(), sn, "cluster-1",
		SNODEAPIResponse{Status: nodeStatusOnline, Hostname: "vm02_4420"}); err != nil {
		t.Fatalf("applyNodeStatus: %v", err)
	}
	var got simplyblockv1alpha1.StorageNode
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	return got
}

// A node's occupancy is in neither the control plane's API nor its stream, only
// in the metrics it exports, so a first reading is published from there.
func TestNodeStatusPublishesTheCapacitySample(t *testing.T) {
	sampledAt := time.Unix(1788423117, 0).UTC()
	got := applyWithCapacity(t, nodeWithUUID(), &fakeNodeCapacity{
		samples: map[string]prometheus.Capacity{
			snCapNode: {Total: snCapTotal, Used: snCapUsed, SampledAt: sampledAt},
		},
	})

	c := got.Status.Resources.Capacity
	if c == nil {
		t.Fatal("capacity was not published")
	}
	if *c.TotalBytes != snCapTotal || *c.UsedBytes != snCapUsed {
		t.Errorf("total/used = %d/%d", *c.TotalBytes, *c.UsedBytes)
	}
	if c.SampledAt == nil || !c.SampledAt.Time.Equal(sampledAt) {
		t.Errorf("sampledAt = %v, want %s", c.SampledAt, sampledAt)
	}
}

// The reconciler watches its own objects, so a status write schedules another
// reconcile. Recording every sample would make a node reconcile itself for as
// long as any I/O was happening, so a reading that has barely moved is left
// alone and the patch stays empty.
func TestASampleThatBarelyMovedIsNotWritten(t *testing.T) {
	// One mebibyte more on a 104 GiB node: far under one percent.
	got := applyWithCapacity(t, nodeRecording(snCapUsed, snCapTotal), &fakeNodeCapacity{
		samples: map[string]prometheus.Capacity{
			snCapNode: {
				Total: snCapTotal, Used: snCapUsed + 1048576,
				SampledAt: time.Unix(1788423200, 0).UTC(),
			},
		},
	})

	if *got.Status.Resources.Capacity.UsedBytes != snCapUsed {
		t.Errorf("used was rewritten to %d for a sub-threshold move",
			*got.Status.Resources.Capacity.UsedBytes)
	}
}

// A move worth knowing about is recorded. Without this the suppression above
// would be indistinguishable from never updating at all.
func TestASampleThatMovedMateriallyIsWritten(t *testing.T) {
	want := snCapUsed + 10*1024*1024*1024 // ten gibibytes: over one percent
	got := applyWithCapacity(t, nodeRecording(snCapUsed, snCapTotal), &fakeNodeCapacity{
		samples: map[string]prometheus.Capacity{
			snCapNode: {Total: snCapTotal, Used: want, SampledAt: time.Unix(1788423200, 0).UTC()},
		},
	})

	if *got.Status.Resources.Capacity.UsedBytes != want {
		t.Errorf("used = %d, want %d", *got.Status.Resources.Capacity.UsedBytes, want)
	}
}

// A device joining or leaving changes the total, which is worth recording
// however little the used size moved.
func TestAChangedTotalIsAlwaysWritten(t *testing.T) {
	const grown = int64(168455307264)
	got := applyWithCapacity(t, nodeRecording(snCapUsed, snCapTotal), &fakeNodeCapacity{
		samples: map[string]prometheus.Capacity{
			snCapNode: {Total: grown, Used: snCapUsed, SampledAt: time.Unix(1788423200, 0).UTC()},
		},
	})

	if *got.Status.Resources.Capacity.TotalBytes != grown {
		t.Errorf("total = %d, want the new one", *got.Status.Resources.Capacity.TotalBytes)
	}
}

// Prometheus being unreachable leaves the rest of the status correct. A node
// whose occupancy is momentarily unknown is better published than not.
func TestAFailingNodeCapacitySourceStillPublishesTheNode(t *testing.T) {
	got := applyWithCapacity(t, nodeWithUUID(),
		&fakeNodeCapacity{err: errors.New("connection refused")})

	if got.Status.Status != nodeStatusOnline || got.Status.Hostname != "vm02_4420" {
		t.Errorf("status/hostname = %q/%q", got.Status.Status, got.Status.Hostname)
	}
	if got.Status.Resources.Capacity != nil {
		t.Errorf("capacity = %+v, want absent rather than zero", got.Status.Resources.Capacity)
	}
}

// A node the exporter has never measured has no capacity to publish, and zeros
// would read as an empty node rather than an unmeasured one.
func TestAnUnsampledNodePublishesNoCapacity(t *testing.T) {
	got := applyWithCapacity(t, nodeWithUUID(), &fakeNodeCapacity{
		samples: map[string]prometheus.Capacity{
			snCapNode: {Total: 0, Used: 0}, // no SampledAt: never measured
		},
	})

	if got.Status.Resources.Capacity != nil {
		t.Errorf("capacity = %+v, want absent", got.Status.Resources.Capacity)
	}
}
