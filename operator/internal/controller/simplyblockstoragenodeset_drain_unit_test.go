package controller

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/kube"

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
		Recorder:  events.NewFakeRecorder(32),
	}
}

// threeOnlineNodes serves node-1 (the drained node) plus two online peers.
func threeOnlineNodes(t *testing.T) *webapimock.SpecServer {
	t.Helper()
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online"},
			{"id":"node-2","status":"online"},
			{"id":"node-3","status":"online"}
		]`},
	)
	return mock
}

func TestRoundRobinEscalatesPastATargetThatAlreadyFailed(t *testing.T) {
	// Without this the retry is not a retry. Round-robin is a pure function of
	// position, so a single volume is assigned the same first candidate on every
	// pass and goes straight back to the node it just failed on -- which is how
	// a drain spent 75 minutes re-picking one target on 2026-09-25.
	mock := threeOnlineNodes(t)
	defer mock.Close()

	tried := func(pv string) []string { return []string{"node-2"} }
	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()),
		drainTestClusterUUID, "node-1", []string{"pv-a"}, tried)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if assignment["pv-a"] != "node-3" {
		t.Errorf("pv-a went to %q; node-2 already failed for it, so the only "+
			"remaining candidate is node-3", assignment["pv-a"])
	}
}

func TestRoundRobinErrorsWhenEveryTargetHasFailedForTheVolume(t *testing.T) {
	// The escalation has to end somewhere, and saying so beats handing the
	// volume back to a node that has already failed it.
	mock := threeOnlineNodes(t)
	defer mock.Close()

	tried := func(pv string) []string { return []string{"node-2", "node-3"} }
	_, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()),
		drainTestClusterUUID, "node-1", []string{"pv-a"}, tried)
	if err == nil {
		t.Fatal("a target was assigned although every peer had already failed for it")
	}
	if !strings.Contains(err.Error(), "pv-a") {
		t.Errorf("error should name the volume that ran out of targets, got: %v", err)
	}
}

func TestRoundRobinEscalationIsPerVolume(t *testing.T) {
	// One volume exhausting a target says nothing about another volume, so the
	// exhausted list is keyed by PV rather than shared across the drain.
	mock := threeOnlineNodes(t)
	defer mock.Close()

	tried := func(pv string) []string {
		if pv == "pv-a" {
			return []string{"node-2"}
		}
		return nil
	}
	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()),
		drainTestClusterUUID, "node-1", []string{"pv-a", "pv-b"}, tried)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if assignment["pv-a"] == "node-2" {
		t.Error("pv-a was sent back to node-2, which had already failed for it")
	}
	if assignment["pv-b"] != "node-3" {
		t.Errorf("pv-b went to %q; nothing has failed for it, so it keeps its "+
			"round-robin slot", assignment["pv-b"])
	}
}

func TestRecordDrainTargetTried(t *testing.T) {
	ops := &simplyblockv1alpha1.StorageNodeOps{}

	if !recordDrainTargetTried(ops, "pv-a", "node-2") {
		t.Fatal("recording a new target reported no change")
	}
	if got := drainTargetsTriedFor(ops, "pv-a"); len(got) != 1 || got[0] != "node-2" {
		t.Fatalf("tried targets for pv-a = %v, want [node-2]", got)
	}
	// Re-observing the same failed CR must not keep patching the status.
	if recordDrainTargetTried(ops, "pv-a", "node-2") {
		t.Error("recording the same target again reported a change")
	}
	if !recordDrainTargetTried(ops, "pv-a", "node-3") {
		t.Error("recording a second target reported no change")
	}
	if got := drainTargetsTriedFor(ops, "pv-b"); got != nil {
		t.Errorf("pv-b inherited pv-a's exhausted targets: %v", got)
	}
}

func TestRoundRobinDistributesEvenly(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
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
	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, excluded, pvNames, nil)
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
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
	defer mock.Close()
	// Only one node, and it is the excluded one.
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online"}
		]`},
	)

	_, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a"}, nil)
	if err == nil {
		t.Fatal("expected error when no online peer node is available")
	}
}

func TestRoundRobinSkipsOfflineNodes(t *testing.T) {
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
	defer mock.Close()
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online"},
			{"id":"node-2","status":"offline"},
			{"id":"node-3","status":"online"}
		]`},
	)

	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a", "pv-b"}, nil)
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
			kube.AnnoSelectedStorageNode: "true",
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
