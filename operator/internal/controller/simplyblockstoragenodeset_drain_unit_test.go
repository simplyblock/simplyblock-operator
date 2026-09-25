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

// drainNodesMock serves a storage-nodes list where node-1 is the node being
// drained and its secondary is whatever secondary names.
func drainNodesMock(t *testing.T, secondary string, nodes string) *webapimock.SpecServer {
	t.Helper()
	mock := webapimock.NewSpecServerFromFile(t, "../../../shared/openapi.json", true)
	mock.Register(http.MethodGet,
		"/api/v2/clusters/"+drainTestClusterUUID+"/storage-nodes/",
		webapimock.RouteResponse{Status: http.StatusOK, Body: `[
			{"id":"node-1","status":"online","secondary_node_id":"` + secondary + `"},` + nodes + `
		]`},
	)
	return mock
}

func TestDrainSendsEveryVolumeToTheSecondary(t *testing.T) {
	// The secondary already holds the drained node's lvstore, so this is the one
	// target that turns the migration into a role handover instead of a copy.
	// Round-robin spread volumes over all online peers and reached a replica
	// holder only by chance.
	mock := drainNodesMock(t, "node-2", `
			{"id":"node-2","status":"online"},
			{"id":"node-3","status":"online"}`)
	defer mock.Close()

	pvNames := []string{"pv-a", "pv-b", "pv-c", "pv-d", "pv-e", "pv-f"}
	assignment, err := drainTargetNodes(
		context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", pvNames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignment) != len(pvNames) {
		t.Fatalf("expected %d assignments, got %d", len(pvNames), len(assignment))
	}
	for pv, target := range assignment {
		if target != "node-2" {
			t.Errorf("pv %s went to %s, want the secondary node-2 "+
				"(node-3 holds no replica, so that is a full copy)", pv, target)
		}
	}
}

func TestDrainStallsWhenTheSecondaryIsNotOnline(t *testing.T) {
	// Falling back to any other online peer is what silently turns a handover
	// into a cluster-wide copy, so an offline secondary must stop the drain and
	// say so rather than pick someone else.
	mock := drainNodesMock(t, "node-2", `
			{"id":"node-2","status":"offline"},
			{"id":"node-3","status":"online"}`)
	defer mock.Close()

	_, err := drainTargetNodes(
		context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a"})
	if err == nil {
		t.Fatal("an offline secondary was accepted, or fell back to another node")
	}
	if !strings.Contains(err.Error(), "node-2") || !strings.Contains(err.Error(), "offline") {
		t.Errorf("error should name the secondary and its status, got: %v", err)
	}
}

func TestDrainStallsWhenThereIsNoSecondary(t *testing.T) {
	mock := drainNodesMock(t, "", `
			{"id":"node-2","status":"online"}`)
	defer mock.Close()

	_, err := drainTargetNodes(
		context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a"})
	if err == nil {
		t.Fatal("a node with no secondary produced a target anyway")
	}
}

func TestDrainRejectsANodeThatIsItsOwnSecondary(t *testing.T) {
	// Would otherwise migrate every volume onto the node being removed.
	mock := drainNodesMock(t, "node-1", `
			{"id":"node-2","status":"online"}`)
	defer mock.Close()

	_, err := drainTargetNodes(
		context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a"})
	if err == nil {
		t.Fatal("the drained node was accepted as its own migration target")
	}
}

func TestDrainStallsWhenTheSecondaryIsNotInTheCluster(t *testing.T) {
	mock := drainNodesMock(t, "node-9", `
			{"id":"node-2","status":"online"}`)
	defer mock.Close()

	_, err := drainTargetNodes(
		context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, "node-1", []string{"pv-a"})
	if err == nil {
		t.Fatal("a secondary that is not a cluster member produced a target anyway")
	}
}

func TestDrainIgnoresUnrelatedOfflineNodes(t *testing.T) {
	// Only the secondary's health decides the target. Another peer being down
	// is not this drain's problem, and must not divert the volumes or stall it.
	mock := drainNodesMock(t, "node-3", `
			{"id":"node-2","status":"offline"},
			{"id":"node-3","status":"online"}`)
	defer mock.Close()

	assignment, err := drainTargetNodes(
		context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID,
		"node-1", []string{"pv-a", "pv-b"})
	if err != nil {
		t.Fatalf("an unrelated offline node stalled the drain: %v", err)
	}
	for pv, target := range assignment {
		if target != "node-3" {
			t.Errorf("pv %s went to %s, want the secondary node-3", pv, target)
		}
	}
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
