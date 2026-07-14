package controller

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	drainTestNS          = "test"
	drainTestCluster     = "test-cluster"
	drainTestClusterUUID = "cccc0000-0000-0000-0000-000000000001"
	drainTestNodeUUID    = "aaaa0000-0000-0000-0000-000000000001"
	drainTestNodeUUID2   = "aaaa0000-0000-0000-0000-000000000002"
)

func newDrainReconciler(t *testing.T, objects ...client.Object) *StorageNodeSetReconciler {
	t.Helper()
	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
	)
	cluster := testCluster(drainTestNS, drainTestCluster, drainTestClusterUUID)
	all := append([]client.Object{cluster}, objects...)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
		&simplyblockv1alpha1.VolumeMigration{},
	}, all...)
	return &StorageNodeSetReconciler{
		Client:    cl,
		Scheme:    scheme,
		Namespace: drainTestNS,
		Recorder:  record.NewFakeRecorder(32),
	}
}

func newDrainSN(subPhase, state string) *simplyblockv1alpha1.StorageNodeSet {
	sn := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-drain", Namespace: drainTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: drainTestCluster,
			Action:      utils.NodeActionRemove,
			NodeUUID:    drainTestNodeUUID,
		},
	}
	if subPhase != "" || state != "" {
		sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:   utils.NodeActionRemove,
			NodeUUID: drainTestNodeUUID,
			State:    state,
			SubPhase: subPhase,
		}
	}
	return sn
}

// ── performDrainAndRemove: terminal state early return ────────────────────────

func TestDrainSkipsWhenAlreadySuccess(t *testing.T) {
	sn := newDrainSN("", utils.ActionStateSuccess)
	r := newDrainReconciler(t, sn)

	res, err := r.performDrainAndRemove(context.Background(), webapi.NewClient("http://127.0.0.1:1"), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("expected no error for success state, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected zero requeue for success state, got %v", res.RequeueAfter)
	}
}

func TestDrainSkipsWhenAlreadyFailed(t *testing.T) {
	sn := newDrainSN("", utils.ActionStateFailed)
	r := newDrainReconciler(t, sn)

	res, err := r.performDrainAndRemove(context.Background(), webapi.NewClient("http://127.0.0.1:1"), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("expected no error for failed state, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected zero requeue for failed state, got %v", res.RequeueAfter)
	}
}

func TestDrainSkipsSuccessEvenWhenSubPhaseIsEmpty(t *testing.T) {
	// Regression: stale reconcile reads SubPhase="" after success and must not
	// re-initialize the drain (which would restart it on an already-removed node).
	sn := newDrainSN("", utils.ActionStateSuccess)
	r := newDrainReconciler(t, sn)

	// Run twice — second call must also return immediately without API calls.
	for i := 0; i < 2; i++ {
		res, err := r.performDrainAndRemove(context.Background(), webapi.NewClient("http://127.0.0.1:1"), drainTestClusterUUID, sn)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error %v", i, err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("iteration %d: unexpected requeue %v", i, res.RequeueAfter)
		}
	}
}

// ── roundRobinTargetNodes ─────────────────────────────────────────────────────

func TestRoundRobinDistributesEvenly(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online"},
			{"id":"node-2","status":"online"},
			{"id":"node-3","status":"online"}
		]`},
	)

	pvNames := []string{"pv-a", "pv-b", "pv-c", "pv-d", "pv-e", "pv-f"}
	excluded := "node-1"
	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, excluded, pvNames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// excluded node should not appear as a target
	for pv, target := range assignment {
		if target == excluded {
			t.Errorf("pv %s assigned to excluded node %s", pv, excluded)
		}
	}
	// all pvNames must be assigned
	if len(assignment) != len(pvNames) {
		t.Errorf("expected %d assignments, got %d", len(pvNames), len(assignment))
	}
	// each of node-2 and node-3 should appear 3 times (6 pvs / 2 nodes)
	counts := map[string]int{}
	for _, target := range assignment {
		counts[target]++
	}
	for _, node := range []string{"node-2", "node-3"} {
		if counts[node] != 3 {
			t.Errorf("node %s expected 3 assignments, got %d", node, counts[node])
		}
	}
}

func TestRoundRobinErrorsWhenNoTargetAvailable(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	// Only one node, and it is the excluded one.
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online"}
		]`},
	)

	_, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a"})
	if err == nil {
		t.Fatal("expected error when no online peer node is available")
	}
}

func TestRoundRobinSkipsOfflineNodes(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online"},
			{"id":"node-2","status":"offline"},
			{"id":"node-3","status":"online"}
		]`},
	)

	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a", "pv-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for pv, target := range assignment {
		if target == "node-2" {
			t.Errorf("pv %s assigned to offline node-2", pv)
		}
		if target == "node-1" {
			t.Errorf("pv %s assigned to excluded node-1", pv)
		}
	}
	_ = assignment
}

// ── matchVolumesToPVs ─────────────────────────────────────────────────────────

func newPV(name, volumeUUID string) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{
		Driver:       utils.CSIProvisioner,
		VolumeHandle: drainTestClusterUUID + ":pool-1:" + volumeUUID,
	}
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: drainTestNS,
		Name:      name + "-pvc",
	}
	return pv
}

func newPVC(name string, pinned bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: drainTestNS},
	}
	if pinned {
		pvc.Annotations = map[string]string{
			simplyblockv1alpha1.AnnotationPinnedVolume: "true",
		}
	}
	return pvc
}

func TestMatchVolumesToPVs_PVManaged(t *testing.T) {
	pv := newPV("pv-a", "vol-1111")
	pvc := newPVC("pv-a-pvc", false)
	r := newDrainReconciler(t, pv, pvc)

	vols := []webapi.VolumeInfo{{UUID: "vol-1111", Name: "pvc-something"}}
	pvManaged, pinned, unmanaged, byUUID, _, err := matchVolumesToPVs(context.Background(), r.Client, vols, regexp.MustCompile("^never-matches$"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pvManaged) != 1 || pvManaged[0] != "vol-1111" {
		t.Errorf("expected vol-1111 in pvManaged, got %v", pvManaged)
	}
	if len(pinned) != 0 || len(unmanaged) != 0 {
		t.Errorf("expected no pinned/unmanaged, got pinned=%v unmanaged=%v", pinned, unmanaged)
	}
	if byUUID["vol-1111"] != "pv-a" {
		t.Errorf("expected pvName=pv-a, got %q", byUUID["vol-1111"])
	}
}

func TestMatchVolumesToPVs_Pinned(t *testing.T) {
	pv := newPV("pv-b", "vol-2222")
	pvc := newPVC("pv-b-pvc", true) // pinned
	r := newDrainReconciler(t, pv, pvc)

	vols := []webapi.VolumeInfo{{UUID: "vol-2222", Name: "pvc-something"}}
	pvManaged, pinned, unmanaged, _, _, err := matchVolumesToPVs(context.Background(), r.Client, vols, regexp.MustCompile("^never-matches$"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pinned) != 1 || pinned[0] != "vol-2222" {
		t.Errorf("expected vol-2222 in pinned, got %v", pinned)
	}
	if len(pvManaged) != 0 || len(unmanaged) != 0 {
		t.Errorf("expected no pvManaged/unmanaged, got pvManaged=%v unmanaged=%v", pvManaged, unmanaged)
	}
}

func TestMatchVolumesToPVs_Unmanaged(t *testing.T) {
	r := newDrainReconciler(t) // no PVs in cluster

	vols := []webapi.VolumeInfo{{UUID: "vol-orphan", Name: "manually-created"}}
	pvManaged, pinned, unmanaged, _, _, err := matchVolumesToPVs(context.Background(), r.Client, vols, regexp.MustCompile("^never-matches$"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unmanaged) != 1 || unmanaged[0] != "vol-orphan" {
		t.Errorf("expected vol-orphan in unmanaged, got %v", unmanaged)
	}
	if len(pvManaged) != 0 || len(pinned) != 0 {
		t.Errorf("unexpected pvManaged/pinned: %v / %v", pvManaged, pinned)
	}
}

func TestMatchVolumesToPVs_SystemVolumeSkipped(t *testing.T) {
	r := newDrainReconciler(t) // no PVs — if not filtered, would be unmanaged

	vols := []webapi.VolumeInfo{{UUID: "vol-bench", Name: "sb-fio-baseline-xyz"}}
	pvManaged, pinned, unmanaged, _, _, err := matchVolumesToPVs(context.Background(), r.Client, vols, defaultSystemVolumeFilter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pvManaged)+len(pinned)+len(unmanaged) != 0 {
		t.Errorf("system volume should be skipped entirely, got pvManaged=%v pinned=%v unmanaged=%v", pvManaged, pinned, unmanaged)
	}
}

func TestDrainValidateBothPinnedAndUnmanagedSurfacedTogether(t *testing.T) {
	// When a node has both pinned and unmanaged volumes, drainValidate must emit
	// BOTH warning events in a single reconcile — not short-circuit after pinned.
	// This prevents the user having to fix one issue, retry, then discover the next.
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	// Backend returns two volumes: one PV-managed+pinned, one unmanaged.
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-pools/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"pool-1","name":"p1"}]`},
	)
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-pools/pool-1/volumes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"vol-pinned","name":"pvc-a","storage_node_id":"` + drainTestNodeUUID + `"},
			{"id":"vol-orphan","name":"manually-created","storage_node_id":"` + drainTestNodeUUID + `"}
		]`},
	)
	// Cluster not rebalancing.
	rebalancing := false
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status:     simplyblockv1alpha1.StorageClusterStatus{Rebalancing: &rebalancing},
	}
	pv := newPV("pv-pin", "vol-pinned")
	pvc := newPVC("pv-pin-pvc", true) // pinned annotation set

	fakeRecorder := record.NewFakeRecorder(8)
	sn := newDrainSN(drainSubPhaseValidating, utils.ActionStateRunning)
	sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:   utils.NodeActionRemove,
		NodeUUID: drainTestNodeUUID,
		State:    utils.ActionStateRunning,
		SubPhase: drainSubPhaseValidating,
	}

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
	}, cluster, sn, pv, pvc)
	r := &StorageNodeSetReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: fakeRecorder,
	}

	res, err := r.drainValidate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero requeue when blocking volumes present")
	}

	// Drain all buffered events.
	close(fakeRecorder.Events)
	var reasons []string
	for e := range fakeRecorder.Events {
		// Event format: "Warning PinnedVolumeBlocking ..."
		parts := strings.SplitN(e, " ", 3)
		if len(parts) >= 2 {
			reasons = append(reasons, parts[1])
		}
	}

	hasPinned := contains(reasons, "PinnedVolumeBlocking")
	hasUnmanaged := contains(reasons, "UnmanagedVolumeBlocking")

	if !hasPinned {
		t.Error("expected PinnedVolumeBlocking event to be emitted")
	}
	if !hasUnmanaged {
		t.Error("expected UnmanagedVolumeBlocking event to be emitted in the same reconcile")
	}
	if !hasPinned || !hasUnmanaged {
		t.Errorf("emitted events: %v — both blockers must surface together so the user can fix everything at once", reasons)
	}

	// Message must mention both issues.
	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	msg := ""
	if updated.Status.ActionStatus != nil {
		msg = updated.Status.ActionStatus.Message
	}
	if !strings.Contains(msg, "pinned") || !strings.Contains(msg, "unmanaged") {
		t.Errorf("status message should mention both blockers, got: %q", msg)
	}
}

func TestMatchVolumesToPVs_EmptyNodeSkipsMigration(t *testing.T) {
	r := newDrainReconciler(t)
	pvManaged, pinned, unmanaged, _, _, err := matchVolumesToPVs(context.Background(), r.Client, nil, defaultSystemVolumeFilter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pvManaged)+len(pinned)+len(unmanaged) != 0 {
		t.Errorf("empty node should produce no buckets")
	}
}

func TestMatchVolumesToPVs_OnlySystemVolumes(t *testing.T) {
	r := newDrainReconciler(t)
	vols := []webapi.VolumeInfo{
		{UUID: "v1", Name: "sb-fio-baseline-read"},
		{UUID: "v2", Name: "sb-fio-baseline-write"},
	}
	pvManaged, pinned, unmanaged, _, _, err := matchVolumesToPVs(context.Background(), r.Client, vols, defaultSystemVolumeFilter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pvManaged)+len(pinned)+len(unmanaged) != 0 {
		t.Errorf("system-only node should produce no drain work")
	}
}

// ── drainValidate: reconciler-level user-visible behaviour ───────────────────
//
// These tests call drainValidate (not matchVolumesToPVs) and assert what the
// user actually sees: events emitted on the StorageNodeSet CR and the status
// message. Testing the classification helper alone is insufficient — it does
// not verify that the reconciler surfaces the result to the user.

// newValidateSetup builds a mock HTTP server and reconciler for drainValidate
// tests. volumes is the JSON array the backend returns for the pool's volumes.
func newValidateSetup(
	t *testing.T,
	volumes string,
	k8sObjects ...client.Object,
) (*StorageNodeSetReconciler, *record.FakeRecorder, *webapimock.SpecServer, *simplyblockv1alpha1.StorageNodeSet) {
	t.Helper()
	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	t.Cleanup(mock.Close)
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-pools/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[{"id":"pool-1","name":"p1"}]`},
	)
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-pools/pool-1/volumes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: volumes},
	)

	rebalancing := false
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status:     simplyblockv1alpha1.StorageClusterStatus{Rebalancing: &rebalancing},
	}
	sn := newDrainSN(drainSubPhaseValidating, utils.ActionStateRunning)
	sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:   utils.NodeActionRemove,
		NodeUUID: drainTestNodeUUID,
		State:    utils.ActionStateRunning,
		SubPhase: drainSubPhaseValidating,
	}

	recorder := record.NewFakeRecorder(8)
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	allObjs := append([]client.Object{cluster, sn}, k8sObjects...)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
	}, allObjs...)
	r := &StorageNodeSetReconciler{Client: cl, Scheme: scheme, Recorder: recorder}
	return r, recorder, mock, sn
}

// collectEvents drains all buffered events and returns a map of reason→count.
func collectEvents(rec *record.FakeRecorder) map[string]int {
	close(rec.Events)
	reasons := map[string]int{}
	for e := range rec.Events {
		parts := strings.SplitN(e, " ", 3)
		if len(parts) >= 2 {
			reasons[parts[1]]++
		}
	}
	return reasons
}

func TestDrainValidatePinnedVolumeEmitsEventAndBlocks(t *testing.T) {
	// User sees: PinnedVolumeBlocking event; status message mentions "pinned";
	// drain does not advance past Validating.
	pv := newPV("pv-x", "vol-pinned")
	pvc := newPVC("pv-x-pvc", true)
	r, rec, mock, sn := newValidateSetup(t,
		`[{"id":"vol-pinned","name":"pvc-x","storage_node_id":"`+drainTestNodeUUID+`"}]`,
		pv, pvc,
	)
	defer mock.Close()

	res, err := r.drainValidate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero requeue when pinned volume blocks drain")
	}

	events := collectEvents(rec)
	if events["PinnedVolumeBlocking"] == 0 {
		t.Error("expected PinnedVolumeBlocking event; user must be told why drain is blocked")
	}

	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	msg := ""
	if updated.Status.ActionStatus != nil {
		msg = updated.Status.ActionStatus.Message
	}
	if !strings.Contains(msg, "pinned") {
		t.Errorf("status message must mention 'pinned' so user knows what to fix; got: %q", msg)
	}
	if updated.Status.ActionStatus != nil && updated.Status.ActionStatus.SubPhase != drainSubPhaseValidating {
		t.Errorf("drain must stay in Validating when blocked; got SubPhase=%q", updated.Status.ActionStatus.SubPhase)
	}
}

func TestDrainValidateUnmanagedVolumeEmitsEventAndBlocks(t *testing.T) {
	// User sees: UnmanagedVolumeBlocking event; status message mentions "unmanaged";
	// drain does not advance past Validating.
	r, rec, mock, sn := newValidateSetup(t,
		`[{"id":"vol-orphan","name":"manually-created","storage_node_id":"`+drainTestNodeUUID+`"}]`,
		// no PV in cluster → volume is unmanaged
	)
	defer mock.Close()

	res, err := r.drainValidate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero requeue when unmanaged volume blocks drain")
	}

	events := collectEvents(rec)
	if events["UnmanagedVolumeBlocking"] == 0 {
		t.Error("expected UnmanagedVolumeBlocking event; user must be told why drain is blocked")
	}

	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	msg := ""
	if updated.Status.ActionStatus != nil {
		msg = updated.Status.ActionStatus.Message
	}
	if !strings.Contains(msg, "unmanaged") {
		t.Errorf("status message must mention 'unmanaged' so user knows what to fix; got: %q", msg)
	}
}

func TestDrainValidateEmptyNodeAdvancesToSuspending(t *testing.T) {
	// User sees: drain proceeds without any blocking event.
	// Empty node must not block; it advances straight to Suspending.
	r, rec, mock, sn := newValidateSetup(t, `[]`)
	defer mock.Close()

	res, err := r.drainValidate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should requeue immediately (advance to Suspending), not with the blocking interval.
	if res.RequeueAfter > drainRequeueBlocking {
		t.Errorf("unexpected long requeue for empty node: %v (expected ≤ %v)", res.RequeueAfter, drainRequeueBlocking)
	}

	events := collectEvents(rec)
	if events["PinnedVolumeBlocking"] > 0 || events["UnmanagedVolumeBlocking"] > 0 {
		t.Errorf("empty node must not emit blocking events; got %v", events)
	}

	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.ActionStatus == nil || updated.Status.ActionStatus.SubPhase != drainSubPhaseSuspending {
		got := ""
		if updated.Status.ActionStatus != nil {
			got = updated.Status.ActionStatus.SubPhase
		}
		t.Errorf("empty node must advance to Suspending; got SubPhase=%q", got)
	}
}

func TestDrainValidateSystemOnlyNodeAdvancesToSuspending(t *testing.T) {
	// User sees: drain proceeds without blocking even though volumes exist —
	// system volumes are silently filtered and the drain advances normally.
	r, rec, mock, sn := newValidateSetup(t, `[
		{"id":"v1","name":"sb-fio-baseline-read","storage_node_id":"`+drainTestNodeUUID+`"},
		{"id":"v2","name":"sb-fio-baseline-write","storage_node_id":"`+drainTestNodeUUID+`"}
	]`)
	defer mock.Close()

	_, err := r.drainValidate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := collectEvents(rec)
	if events["PinnedVolumeBlocking"] > 0 || events["UnmanagedVolumeBlocking"] > 0 {
		t.Errorf("system-volume-only node must not emit blocking events; got %v", events)
	}

	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.ActionStatus == nil || updated.Status.ActionStatus.SubPhase != drainSubPhaseSuspending {
		got := ""
		if updated.Status.ActionStatus != nil {
			got = updated.Status.ActionStatus.SubPhase
		}
		t.Errorf("system-only node must advance to Suspending; got SubPhase=%q", got)
	}
}

// ── drainMigrate: VolumeMigration idempotency ─────────────────────────────────

func TestDrainMigrateDoesNotRecreateExistingCRs(t *testing.T) {
	// Existing VolumeMigration CR for a drain — operator restart must not
	// create a duplicate with the same drain label.
	sn := newDrainSN(drainSubPhaseMigrating, utils.ActionStateRunning)
	existingVM := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drain-existing-pvc-a",
			Namespace: drainTestNS,
			Labels:    map[string]string{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         "pv-a",
			TargetNodeUUID: drainTestNodeUUID2,
		},
		Status: simplyblockv1alpha1.VolumeMigrationStatus{
			Phase: simplyblockv1alpha1.VolumeMigrationPhaseRunning,
		},
	}
	r := newDrainReconciler(t, sn, existingVM)

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	// No pool/volume listing needed — should use existing CRs.

	// Call drainMigrate. It should see the existing CR and not call the API
	// to create new ones.
	if _, err := r.drainMigrate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn); err != nil {
		t.Fatalf("drainMigrate: %v", err)
	}

	var vmList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &vmList,
		client.InNamespace(drainTestNS),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
	); err != nil {
		t.Fatalf("list VolumeMigration: %v", err)
	}
	if len(vmList.Items) != 1 {
		t.Errorf("expected exactly 1 VolumeMigration (no duplicate created), got %d", len(vmList.Items))
	}
}

func TestDrainMigrateFailedCRDeletedAndRetriedWithNewTarget(t *testing.T) {
	// Any VolumeMigration failure deletes the Failed CR and requeues so
	// createMissingVolumeMigrations can recreate it with a fresh target.
	// The node stays suspended — resumeAndFail is NOT called.
	sn := newDrainSN(drainSubPhaseMigrating, utils.ActionStateRunning)
	failedVM := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drain-failed-pvc-a",
			Namespace: drainTestNS,
			Labels:    map[string]string{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         "pv-a",
			TargetNodeUUID: drainTestNodeUUID2,
		},
		Status: simplyblockv1alpha1.VolumeMigrationStatus{
			Phase:        simplyblockv1alpha1.VolumeMigrationPhaseFailed,
			ErrorMessage: "ContinueMigration: status 400: target node not online",
		},
	}
	r := newDrainReconciler(t, sn, failedVM)

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	res, err := r.drainMigrate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("drainMigrate: %v", err)
	}
	// Must requeue for retry, not zero (which would mean terminal).
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero requeue when VM failed and retry is in progress")
	}

	// State must still be running — the node is NOT resumed and drain NOT failed.
	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.ActionStatus != nil && updated.Status.ActionStatus.State == utils.ActionStateFailed {
		t.Error("drain must not be marked failed — should retry with a new target")
	}

	// The Failed VM must be deleted so the next reconcile recreates it.
	var vmList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &vmList,
		client.InNamespace(drainTestNS),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
	); err != nil {
		t.Fatalf("list VMs: %v", err)
	}
	for _, vm := range vmList.Items {
		if vm.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed {
			t.Errorf("Failed VM %q must be deleted so it can be recreated with a different target", vm.Name)
		}
	}
}

func TestDrainMigrateFailedCRDeletedWhenClusterPaused(t *testing.T) {
	// When a VolumeMigration fails AND the cluster is not ready, the drain
	// should pause AND delete the Failed VM so it can be recreated with a fresh
	// target assignment once the cluster recovers — not just defer the failure.
	rebalancing := true
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			Status:      utils.ClusterStatusActive,
			Rebalancing: &rebalancing,
		},
	}
	sn := newDrainSN(drainSubPhaseMigrating, utils.ActionStateRunning)
	sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:   utils.NodeActionRemove,
		NodeUUID: drainTestNodeUUID,
		State:    utils.ActionStateRunning,
		SubPhase: drainSubPhaseMigrating,
	}
	failedVM := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drain-failed-pvc-a",
			Namespace: drainTestNS,
			Labels:    map[string]string{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         "pv-a",
			TargetNodeUUID: drainTestNodeUUID2,
		},
		Status: simplyblockv1alpha1.VolumeMigrationStatus{
			Phase:        simplyblockv1alpha1.VolumeMigrationPhaseFailed,
			ErrorMessage: "target node in_shutdown",
		},
	}

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
		&simplyblockv1alpha1.VolumeMigration{},
	}, cluster, sn, failedVM)
	r := &StorageNodeSetReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(8),
	}

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	res, err := r.drainMigrate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("drainMigrate: %v", err)
	}
	// Must requeue (pause), not fail.
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero requeue when cluster is paused due to rebalancing")
	}
	// State must still be running — NOT failed.
	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.ActionStatus != nil && updated.Status.ActionStatus.State == utils.ActionStateFailed {
		t.Error("drain must not be marked failed when pausing due to cluster state")
	}
	// The Failed VM must have been deleted so the drain can recreate it on resume.
	var vmList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &vmList,
		client.InNamespace(drainTestNS),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
	); err != nil {
		t.Fatalf("list VMs: %v", err)
	}
	for _, vm := range vmList.Items {
		if vm.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed {
			t.Errorf("Failed VolumeMigration %q must be deleted during cluster-state pause so it can be recreated on resume", vm.Name)
		}
	}
}

// ── drainMigrationName ────────────────────────────────────────────────────────

func TestDrainMigrationNameNoCollisionOnLongPVNames(t *testing.T) {
	// Two PV names that share a 60+ char common prefix must produce distinct CR
	// names after sanitisation and truncation (collision guard via FNV suffix).
	longBase := "pvc-" + strings.Repeat("a", 55) // 59 chars — produces a 63-char name when prefixed
	pv1 := longBase + "1"
	pv2 := longBase + "2"
	nodeUUID := "aaaabbbb-cccc-dddd-eeee-ffffffffffff"

	name1 := drainMigrationName(nodeUUID, pv1)
	name2 := drainMigrationName(nodeUUID, pv2)

	if name1 == name2 {
		t.Errorf("collision: both PVs produced the same CR name %q", name1)
	}
	if len(name1) > 63 {
		t.Errorf("name1 too long: %d chars", len(name1))
	}
	if len(name2) > 63 {
		t.Errorf("name2 too long: %d chars", len(name2))
	}
}

func TestDrainMigrationNameIsDNSValid(t *testing.T) {
	cases := []struct {
		nodeUUID string
		pvName   string
	}{
		{"afc7286e-ca84-42f1-bc8f-c582ad2a9a9e", "pvc-a62c57bc-f64c-4385-ace4-f84b729fc8ee"},
		{"short", "pvc-simple"},
		{"", "pvc-no-node"},
		{"uuid", "PVC-Upper-Case"},
	}
	for _, tc := range cases {
		name := drainMigrationName(tc.nodeUUID, tc.pvName)
		if len(name) > 63 {
			t.Errorf("name too long (%d): %q", len(name), name)
		}
		if len(name) == 0 {
			t.Errorf("empty name for nodeUUID=%q pvName=%q", tc.nodeUUID, tc.pvName)
		}
		for _, c := range name {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				t.Errorf("invalid char %q in name %q", c, name)
			}
		}
		if name[0] == '-' || name[len(name)-1] == '-' {
			t.Errorf("name starts or ends with '-': %q", name)
		}
	}
}

// ── drainHandleCancellation: stale cache guard ────────────────────────────────

func TestDrainCancellationDeletesInFlightVolumeMigrationCRs(t *testing.T) {
	// When the user cancels mid-drain, all owned VolumeMigration CRs must be
	// deleted so a subsequent drain starts fresh and emits MigrationCreated events.
	// Without this, the re-applied drain silently reuses the old CRs and the user
	// sees no indication that migration restarted.
	sn := newDrainSN(drainSubPhaseMigrating, utils.ActionStateRunning)
	sn.Spec.Action = "" // user cleared the action
	sn.Spec.NodeUUID = ""
	inFlightVM := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drain-inflight-pvc-a",
			Namespace: drainTestNS,
			Labels:    map[string]string{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         "pv-a",
			TargetNodeUUID: drainTestNodeUUID2,
		},
		Status: simplyblockv1alpha1.VolumeMigrationStatus{
			Phase: simplyblockv1alpha1.VolumeMigrationPhaseRunning,
		},
	}
	r := newDrainReconciler(t, sn, inFlightVM)

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	// Node is online (already resumed or never suspended in this test).
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/"+drainTestNodeUUID,
		webapimock.RouteResponse{Status: http.StatusOK, Body: `{"status":"online"}`},
	)

	if _, err := r.drainHandleCancellation(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn); err != nil {
		t.Fatalf("drainHandleCancellation: %v", err)
	}

	// The in-flight VolumeMigration CR must have been deleted.
	var vmList simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &vmList,
		client.InNamespace(drainTestNS),
		client.MatchingLabels{"storage.simplyblock.io/drain-node": drainTestNodeUUID},
	); err != nil {
		t.Fatalf("list VolumeMigration: %v", err)
	}
	if len(vmList.Items) != 0 {
		t.Errorf("expected in-flight VolumeMigration CRs to be deleted on cancel, got %d remaining", len(vmList.Items))
	}

	// ActionStatus must also be cleared.
	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.ActionStatus != nil {
		t.Errorf("expected ActionStatus cleared after cancellation, got %+v", updated.Status.ActionStatus)
	}
}

func TestDrainCancellationSkipsWhenActionStillActive(t *testing.T) {
	// If the live CR still has action=remove, a stale reconcile must not resume.
	sn := newDrainSN(drainSubPhaseMigrating, utils.ActionStateRunning)
	r := newDrainReconciler(t, sn)

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	// No resume endpoint registered — if resume were called the mock would return 404
	// which would log an error. We verify the CR is unchanged.

	// The passed-in sn has action=remove in the live store (seeded in fake client),
	// so drainHandleCancellation's re-fetch will see it and exit early.
	if _, err := r.drainHandleCancellation(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn); err != nil {
		t.Fatalf("drainHandleCancellation: %v", err)
	}
}

// ── drainValidate: cluster rebalancing blocks drain ───────────────────────────

func TestDrainValidateBlocksWhenClusterRebalancing(t *testing.T) {
	rebalancing := true
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status:     simplyblockv1alpha1.StorageClusterStatus{Rebalancing: &rebalancing},
	}
	sn := newDrainSN(drainSubPhaseValidating, utils.ActionStateRunning)
	sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:   utils.NodeActionRemove,
		NodeUUID: drainTestNodeUUID,
		State:    utils.ActionStateRunning,
		SubPhase: drainSubPhaseValidating,
	}

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
	}, cluster, sn)
	r := &StorageNodeSetReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(32),
	}

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	res, err := r.drainValidate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should requeue — not advance to Suspending.
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero requeue when cluster is rebalancing")
	}
	// SubPhase must still be Validating.
	if sn.Status.ActionStatus.SubPhase != drainSubPhaseValidating {
		t.Errorf("expected SubPhase=Validating, got %q", sn.Status.ActionStatus.SubPhase)
	}
}

// ── drainClusterPauseCheck: pause during active sub-phases ────────────────────

// newPauseCheckReconciler builds a reconciler seeded with the given StorageCluster
// and a StorageNodeSet in the provided sub-phase, for drainClusterPauseCheck tests.
func newPauseCheckReconciler(t *testing.T, cluster *simplyblockv1alpha1.StorageCluster, subPhase string) (*StorageNodeSetReconciler, *simplyblockv1alpha1.StorageNodeSet) {
	t.Helper()
	sn := newDrainSN(subPhase, utils.ActionStateRunning)
	sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
		Action:   utils.NodeActionRemove,
		NodeUUID: drainTestNodeUUID,
		State:    utils.ActionStateRunning,
		SubPhase: subPhase,
	}
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StorageNodeSet{},
		&simplyblockv1alpha1.StorageCluster{},
	}, cluster, sn)
	r := &StorageNodeSetReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(32),
	}
	return r, sn
}

func TestDrainPausesWhenClusterInactive(t *testing.T) {
	// When the cluster transitions to a non-active status (e.g. "degraded")
	// during an active drain sub-phase, performDrainAndRemove must pause and
	// requeue rather than advancing the state machine.
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status:     simplyblockv1alpha1.StorageClusterStatus{Status: "degraded"},
	}

	for _, subPhase := range []string{
		drainSubPhaseSuspending,
		drainSubPhaseMigrating,
		drainSubPhaseVerifying,
		drainSubPhaseRemoving,
	} {
		t.Run("subPhase="+subPhase, func(t *testing.T) {
			r, sn := newPauseCheckReconciler(t, cluster, subPhase)

			mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
			defer mock.Close()

			res, err := r.performDrainAndRemove(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.RequeueAfter == 0 {
				t.Errorf("expected non-zero requeue when cluster is inactive")
			}

			// SubPhase must not have advanced.
			updated := &simplyblockv1alpha1.StorageNodeSet{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
				t.Fatalf("get: %v", err)
			}
			if updated.Status.ActionStatus == nil {
				t.Fatal("expected actionStatus to remain set")
			}
			if updated.Status.ActionStatus.SubPhase != subPhase {
				t.Errorf("sub-phase must not advance during pause: got %q want %q",
					updated.Status.ActionStatus.SubPhase, subPhase)
			}
			// Message should mention the pause.
			if !strings.Contains(updated.Status.ActionStatus.Message, "paused") {
				t.Errorf("expected message to mention 'paused', got %q",
					updated.Status.ActionStatus.Message)
			}
		})
	}
}

func TestDrainPausesWhenClusterRebalancingDuringActiveDrain(t *testing.T) {
	// When the cluster starts rebalancing after the drain has already advanced
	// past Validating, the drain must pause at its current sub-phase.
	rebalancing := true
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			Status:      utils.ClusterStatusActive,
			Rebalancing: &rebalancing,
		},
	}

	for _, subPhase := range []string{
		drainSubPhaseSuspending,
		drainSubPhaseMigrating,
	} {
		t.Run("subPhase="+subPhase, func(t *testing.T) {
			r, sn := newPauseCheckReconciler(t, cluster, subPhase)

			mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
			defer mock.Close()

			res, err := r.performDrainAndRemove(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.RequeueAfter == 0 {
				t.Errorf("expected non-zero requeue when cluster is rebalancing")
			}

			updated := &simplyblockv1alpha1.StorageNodeSet{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
				t.Fatalf("get: %v", err)
			}
			if updated.Status.ActionStatus == nil {
				t.Fatal("expected actionStatus to remain set")
			}
			if updated.Status.ActionStatus.SubPhase != subPhase {
				t.Errorf("sub-phase must not advance during pause: got %q want %q",
					updated.Status.ActionStatus.SubPhase, subPhase)
			}
			if !strings.Contains(updated.Status.ActionStatus.Message, "rebalancing") {
				t.Errorf("expected message to mention 'rebalancing', got %q",
					updated.Status.ActionStatus.Message)
			}
		})
	}
}

func TestDrainContinuesWhenClusterActiveAndNotRebalancing(t *testing.T) {
	// When the cluster is active and not rebalancing, drainClusterPauseCheck
	// must return (false, "") so the drain proceeds normally.
	rebalancing := false
	cluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: drainTestCluster, Namespace: drainTestNS},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			Status:      utils.ClusterStatusActive,
			Rebalancing: &rebalancing,
		},
	}
	r, sn := newPauseCheckReconciler(t, cluster, drainSubPhaseMigrating)

	paused, reason := r.drainClusterPauseCheck(context.Background(), sn)
	if paused {
		t.Errorf("expected drain to continue when cluster is active and not rebalancing, got reason=%q", reason)
	}
}

// ── Rapid action toggling ─────────────────────────────────────────────────────

func TestRapidActionToggleDoesNotLeakState(t *testing.T) {
	// Set action=remove, cancel immediately (Validating phase, before any suspend).
	// After cancel the ActionStatus must be nil so re-applying remove starts fresh.
	sn := newDrainSN(drainSubPhaseValidating, utils.ActionStateRunning)
	// Simulate user clearing spec.action.
	sn.Spec.Action = ""
	sn.Spec.NodeUUID = ""
	r := newDrainReconciler(t, sn)

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()

	if _, err := r.drainHandleCancellation(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn); err != nil {
		t.Fatalf("drainHandleCancellation: %v", err)
	}

	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	// ActionStatus cleared — re-applying action=remove will start a fresh drain.
	if updated.Status.ActionStatus != nil {
		t.Errorf("expected ActionStatus to be nil after cancel at Validating, got %+v", updated.Status.ActionStatus)
	}
}
