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

	// Short names for the drain-escalation tests. goconst flags these as
	// repeated literals, and naming them also says which is a volume and which
	// is a node -- "pv-a" and "node-2" read alike at a glance.
	drainTestPVA   = "pv-a"
	drainTestPVB   = "pv-b"
	drainTestNode1 = "node-1"
	drainTestNode2 = "node-2"
	drainTestNode3 = "node-3"
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

	tried := func(pv string) []string { return []string{drainTestNode2} }
	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()),
		drainTestClusterUUID, drainTestNode1, []string{drainTestPVA}, tried)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if assignment[drainTestPVA] != drainTestNode3 {
		t.Errorf("pv-a went to %q; node-2 already failed for it, so the only "+
			"remaining candidate is node-3", assignment[drainTestPVA])
	}
}

func TestRoundRobinErrorsWhenEveryTargetHasFailedForTheVolume(t *testing.T) {
	// The escalation has to end somewhere, and saying so beats handing the
	// volume back to a node that has already failed it.
	mock := threeOnlineNodes(t)
	defer mock.Close()

	tried := func(pv string) []string { return []string{drainTestNode2, drainTestNode3} }
	_, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()),
		drainTestClusterUUID, drainTestNode1, []string{drainTestPVA}, tried)
	if err == nil {
		t.Fatal("a target was assigned although every peer had already failed for it")
	}
	if !strings.Contains(err.Error(), drainTestPVA) {
		t.Errorf("error should name the volume that ran out of targets, got: %v", err)
	}
}

func TestRoundRobinEscalationIsPerVolume(t *testing.T) {
	// One volume exhausting a target says nothing about another volume, so the
	// exhausted list is keyed by PV rather than shared across the drain.
	mock := threeOnlineNodes(t)
	defer mock.Close()

	tried := func(pv string) []string {
		if pv == drainTestPVA {
			return []string{drainTestNode2}
		}
		return nil
	}
	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()),
		drainTestClusterUUID, drainTestNode1, []string{drainTestPVA, drainTestPVB}, tried)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if assignment[drainTestPVA] == drainTestNode2 {
		t.Error("pv-a was sent back to node-2, which had already failed for it")
	}
	if assignment[drainTestPVB] != drainTestNode3 {
		t.Errorf("pv-b went to %q; nothing has failed for it, so it keeps its "+
			"round-robin slot", assignment[drainTestPVB])
	}
}

func TestRecordDrainTargetTried(t *testing.T) {
	ops := &simplyblockv1alpha1.StorageNodeOps{}

	if !recordDrainTargetTried(ops, drainTestPVA, drainTestNode2) {
		t.Fatal("recording a new target reported no change")
	}
	if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 1 || got[0] != drainTestNode2 {
		t.Fatalf("tried targets for pv-a = %v, want [node-2]", got)
	}
	// Re-observing the same failed CR must not keep patching the status.
	if recordDrainTargetTried(ops, drainTestPVA, drainTestNode2) {
		t.Error("recording the same target again reported a change")
	}
	if !recordDrainTargetTried(ops, drainTestPVA, drainTestNode3) {
		t.Error("recording a second target reported no change")
	}
	if got := drainTargetsTriedFor(ops, drainTestPVB); got != nil {
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

	pvNames := []string{drainTestPVA, drainTestPVB, "pv-c", "pv-d", "pv-e", "pv-f"}
	excluded := drainTestNode1
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
	for _, node := range []string{drainTestNode2, drainTestNode3} {
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

	_, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, drainTestNode1, []string{drainTestPVA}, nil)
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

	assignment, err := roundRobinTargetNodes(context.Background(), webapi.NewClient(mock.URL()), drainTestClusterUUID, drainTestNode1, []string{drainTestPVA, drainTestPVB}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for pv, target := range assignment {
		if target == drainTestNode2 {
			t.Errorf("pv %s assigned to offline node-2", pv)
		}
		if target == drainTestNode1 {
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
	pv := newPV(drainTestPVA, "vol-1111")
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
	if byUUID["vol-1111"] != drainTestPVA {
		t.Errorf("expected pvName=pv-a, got %q", byUUID["vol-1111"])
	}
}

func TestMatchVolumesToPVs_Pinned(t *testing.T) {
	pv := newPV(drainTestPVB, "vol-2222")
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

func TestTargetWasEngaged(t *testing.T) {
	// The distinction that decides whether a failure burns a target. A
	// migration that never reached its target failed for a reason that would
	// have failed against any node, so marking that node exhausted destroys a
	// good candidate -- which is how a drain lost all six on 2026-09-25 after
	// the volume's consumer pod died.
	engaged := &simplyblockv1alpha1.VolumeMigration{}
	engaged.Status.SourceNodeUUID = "node-src"
	if !targetWasEngaged(engaged) {
		t.Error("a migration that reached Running was treated as never having " +
			"touched its target, so a genuinely bad target is never exhausted")
	}

	// Validation-time failures (consumer pod down, PV unresolvable, cluster
	// busy) land here: Failed, but with no source recorded because
	// reconcileRunning never ran.
	notEngaged := &simplyblockv1alpha1.VolumeMigration{}
	if targetWasEngaged(notEngaged) {
		t.Error("a migration that failed before running was blamed on its target")
	}
}

func TestRecordExhaustedTargetsIgnoresFailuresBeforeTheTargetWasReached(t *testing.T) {
	// The wiring, not just the predicate: a drain that burns targets for
	// environment failures exhausts its whole candidate list on something no
	// target could have prevented, and then stalls for good.
	vm := func(pv, target, sourceNode string) simplyblockv1alpha1.VolumeMigration {
		m := simplyblockv1alpha1.VolumeMigration{}
		m.Spec.PVName = pv
		m.Spec.TargetNodeUUID = target
		m.Status.SourceNodeUUID = sourceNode // only set once the migration ran
		return m
	}

	ops := &simplyblockv1alpha1.StorageNodeOps{}
	failed := []simplyblockv1alpha1.VolumeMigration{
		vm(drainTestPVA, drainTestNode2, "node-src"), // ran, then failed: node-2 is implicated
		vm(drainTestPVA, drainTestNode3, ""),         // failed in validation: node-3 is not
	}

	if !recordExhaustedTargets(ops, failed) {
		t.Fatal("the engaged target was not recorded")
	}
	got := drainTargetsTriedFor(ops, drainTestPVA)
	if len(got) != 1 || got[0] != drainTestNode2 {
		t.Errorf("exhausted targets = %v, want [node-2] only -- node-3 failed "+
			"before the migration reached it and must stay available", got)
	}
}

func TestUnattributedFailuresAreBoundedNotIgnored(t *testing.T) {
	// The bound that stops the escalation deadlocking in the other direction.
	// Not burning a target for a failure it did not cause is right -- one dead
	// consumer pod must not exhaust six good nodes -- but with nothing counted
	// there is also nothing to escalate to, and the drain recreates the same
	// migration against the same node for ever (2026-09-26).
	vm := func(pv, target, sourceNode string) simplyblockv1alpha1.VolumeMigration {
		m := simplyblockv1alpha1.VolumeMigration{}
		m.Spec.PVName = pv
		m.Spec.TargetNodeUUID = target
		m.Status.SourceNodeUUID = sourceNode // only set once the migration ran
		return m
	}

	ops := &simplyblockv1alpha1.StorageNodeOps{}
	failed := []simplyblockv1alpha1.VolumeMigration{vm(drainTestPVA, drainTestNode2, "")}

	// Below the bound the target stays available: the failure says nothing
	// about it, and another candidate is not obviously better.
	for i := 1; i < MaxUnattributedFailures; i++ {
		recordExhaustedTargets(ops, failed)
		if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 0 {
			t.Fatalf("after %d unattributed failures the target was already "+
				"burned (%v); a failure it did not cause must not exhaust it", i, got)
		}
	}

	// At the bound it is abandoned anyway: something here is not working, even
	// if it cannot be pinned on the node.
	recordExhaustedTargets(ops, failed)
	got := drainTargetsTriedFor(ops, drainTestPVA)
	if len(got) != 1 || got[0] != drainTestNode2 {
		t.Fatalf("exhausted targets = %v, want [node-2] once the bound is "+
			"reached -- otherwise the drain retries one target indefinitely", got)
	}

	// And the count resets, so the next candidate is judged on its own attempts.
	for i := range ops.Status.DrainTargetsTried {
		if ops.Status.DrainTargetsTried[i].PVName == drainTestPVA &&
			ops.Status.DrainTargetsTried[i].Failures != 0 {
			t.Errorf("failure count = %d after burning the target, want 0",
				ops.Status.DrainTargetsTried[i].Failures)
		}
	}
}

func TestAnAttributableFailureStillBurnsImmediately(t *testing.T) {
	// Attribution decides how fast a target is abandoned, not whether it is.
	m := simplyblockv1alpha1.VolumeMigration{}
	m.Spec.PVName = drainTestPVA
	m.Spec.TargetNodeUUID = drainTestNode2
	m.Status.SourceNodeUUID = "node-src" // the migration ran against it

	ops := &simplyblockv1alpha1.StorageNodeOps{}
	recordExhaustedTargets(ops, []simplyblockv1alpha1.VolumeMigration{m})

	if got := drainTargetsTriedFor(ops, drainTestPVA); len(got) != 1 {
		t.Errorf("a failure the target caused did not burn it on the first "+
			"attempt: %v", got)
	}
}
