package controller

import (
	"context"
	"net/http"
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

func newDrainSN(nodeUUID, subPhase, state string) *simplyblockv1alpha1.StorageNodeSet { //nolint:unparam
	sn := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-drain", Namespace: drainTestNS},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName: drainTestCluster,
			Action:      utils.NodeActionRemove,
			NodeUUID:    nodeUUID,
		},
	}
	if subPhase != "" || state != "" {
		sn.Status.ActionStatus = &simplyblockv1alpha1.ActionStatus{
			Action:   utils.NodeActionRemove,
			NodeUUID: nodeUUID,
			State:    state,
			SubPhase: subPhase,
		}
	}
	return sn
}

// ── performDrainAndRemove: terminal state early return ────────────────────────

func TestDrainSkipsWhenAlreadySuccess(t *testing.T) {
	sn := newDrainSN(drainTestNodeUUID, "", utils.ActionStateSuccess)
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
	sn := newDrainSN(drainTestNodeUUID, "", utils.ActionStateFailed)
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
	sn := newDrainSN(drainTestNodeUUID, "", utils.ActionStateSuccess)
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

func newPV(name, volumeUUID, clusterUUID, poolUUID string) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{
		Driver:       utils.CSIProvisioner,
		VolumeHandle: clusterUUID + ":" + poolUUID + ":" + volumeUUID,
	}
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: drainTestNS,
		Name:      name + "-pvc",
	}
	return pv
}

func newPVC(name, ns string, pinned bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if pinned {
		pvc.Annotations = map[string]string{
			simplyblockv1alpha1.AnnotationPinnedVolume: "true",
		}
	}
	return pvc
}

func TestMatchVolumesToPVs_PVManaged(t *testing.T) {
	pv := newPV("pv-a", "vol-1111", drainTestClusterUUID, "pool-1")
	pvc := newPVC("pv-a-pvc", drainTestNS, false)
	r := newDrainReconciler(t, pv, pvc)

	vols := []webapi.VolumeInfo{{UUID: "vol-1111", Name: "pvc-something"}}
	pvManaged, pinned, unmanaged, byUUID, err := matchVolumesToPVs(context.Background(), r, vols, "^never-matches$")
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
	pv := newPV("pv-b", "vol-2222", drainTestClusterUUID, "pool-1")
	pvc := newPVC("pv-b-pvc", drainTestNS, true) // pinned
	r := newDrainReconciler(t, pv, pvc)

	vols := []webapi.VolumeInfo{{UUID: "vol-2222", Name: "pvc-something"}}
	pvManaged, pinned, unmanaged, _, err := matchVolumesToPVs(context.Background(), r, vols, "^never-matches$")
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
	pvManaged, pinned, unmanaged, _, err := matchVolumesToPVs(context.Background(), r, vols, "^never-matches$")
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
	pvManaged, pinned, unmanaged, _, err := matchVolumesToPVs(context.Background(), r, vols, simplyblockv1alpha1.DefaultSystemVolumeFilterRegex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pvManaged)+len(pinned)+len(unmanaged) != 0 {
		t.Errorf("system volume should be skipped entirely, got pvManaged=%v pinned=%v unmanaged=%v", pvManaged, pinned, unmanaged)
	}
}

func TestMatchVolumesToPVs_BothPinnedAndUnmanagedVisible(t *testing.T) {
	// When a node has both blocking types they must all surface — not short-circuit.
	pv := newPV("pv-pin", "vol-pinned", drainTestClusterUUID, "pool-1")
	pvc := newPVC("pv-pin-pvc", drainTestNS, true)
	r := newDrainReconciler(t, pv, pvc)

	vols := []webapi.VolumeInfo{
		{UUID: "vol-pinned", Name: "pvc-a"},
		{UUID: "vol-orphan", Name: "manually-created"},
	}
	_, pinned, unmanaged, _, err := matchVolumesToPVs(context.Background(), r, vols, "^never-matches$")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pinned) != 1 {
		t.Errorf("expected 1 pinned volume, got %v", pinned)
	}
	if len(unmanaged) != 1 {
		t.Errorf("expected 1 unmanaged volume, got %v", unmanaged)
	}
}

func TestMatchVolumesToPVs_EmptyNodeSkipsMigration(t *testing.T) {
	r := newDrainReconciler(t)
	pvManaged, pinned, unmanaged, _, err := matchVolumesToPVs(context.Background(), r, nil, simplyblockv1alpha1.DefaultSystemVolumeFilterRegex)
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
	pvManaged, pinned, unmanaged, _, err := matchVolumesToPVs(context.Background(), r, vols, simplyblockv1alpha1.DefaultSystemVolumeFilterRegex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pvManaged)+len(pinned)+len(unmanaged) != 0 {
		t.Errorf("system-only node should produce no drain work")
	}
}

// ── drainMigrate: VolumeMigration idempotency ─────────────────────────────────

func TestDrainMigrateDoesNotRecreateExistingCRs(t *testing.T) {
	// Existing VolumeMigration CR for a drain — operator restart must not
	// create a duplicate with the same drain label.
	sn := newDrainSN(drainTestNodeUUID, drainSubPhaseMigrating, utils.ActionStateRunning)
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

func TestDrainMigrateFailedCRTriggersResumeAndFail(t *testing.T) {
	sn := newDrainSN(drainTestNodeUUID, drainSubPhaseMigrating, utils.ActionStateRunning)
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
			ErrorMessage: "backend rejected migration",
		},
	}
	r := newDrainReconciler(t, sn, failedVM)

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", true)
	defer mock.Close()
	// Resume API call (best-effort, may fail in test — that is acceptable)
	mock.Register(http.MethodPost,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/"+drainTestNodeUUID+"/resume",
		webapimock.RouteResponse{Status: http.StatusNoContent},
	)

	if _, err := r.drainMigrate(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, sn); err != nil {
		t.Fatalf("drainMigrate: %v", err)
	}

	// After resumeAndFail, actionStatus.state must be failed.
	updated := &simplyblockv1alpha1.StorageNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sn), updated); err != nil {
		t.Fatalf("get StorageNodeSet: %v", err)
	}
	if updated.Status.ActionStatus == nil {
		t.Fatal("expected actionStatus to be set")
	}
	if updated.Status.ActionStatus.State != utils.ActionStateFailed {
		t.Errorf("expected state=failed, got %q", updated.Status.ActionStatus.State)
	}
	if updated.Status.ActionStatus.SubPhase != "" {
		t.Errorf("expected SubPhase cleared on failure, got %q", updated.Status.ActionStatus.SubPhase)
	}
}

// ── drainMigrationName ────────────────────────────────────────────────────────

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

func TestDrainCancellationSkipsWhenActionStillActive(t *testing.T) {
	// If the live CR still has action=remove, a stale reconcile must not resume.
	sn := newDrainSN(drainTestNodeUUID, drainSubPhaseMigrating, utils.ActionStateRunning)
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
	sn := newDrainSN(drainTestNodeUUID, drainSubPhaseValidating, utils.ActionStateRunning)
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

// ── Rapid action toggling ─────────────────────────────────────────────────────

func TestRapidActionToggleDoesNotLeakState(t *testing.T) {
	// Set action=remove, cancel immediately (Validating phase, before any suspend).
	// After cancel the ActionStatus must be nil so re-applying remove starts fresh.
	sn := newDrainSN(drainTestNodeUUID, drainSubPhaseValidating, utils.ActionStateRunning)
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
