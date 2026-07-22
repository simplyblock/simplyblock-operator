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
