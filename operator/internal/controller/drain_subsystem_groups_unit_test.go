// One VolumeMigration per NVMe subsystem, not per PV.
//
// The control plane migrates a subsystem whole; volumes that share one
// (max_namespace_per_subsys > 1) move together. The drain used to create a CR
// per PV, each with its own target, and the CRs of one subsystem cancelled each
// other's migrations for ever (2026-09-28: "1 of 6 volumes migrated" for 79
// minutes). These tests pin the grouping, the CR creation and the volume-based
// progress count.

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const sharedNQN = "nqn.2023-02.io.simplyblock:cluster:lvol:shared"

func TestDrainSubsystemGroups_SharedSubsystemHasOneCanonicalPV(t *testing.T) {
	vols := []webapi.VolumeInfo{
		{UUID: "v1", NQN: sharedNQN},
		{UUID: "v2", NQN: sharedNQN},
		{UUID: "v3", NQN: sharedNQN},
		{UUID: "v4", NQN: "nqn.other:lvol:alone"},
	}
	byVol := map[string]string{"v1": "pvc-c", "v2": "pvc-a", "v3": "pvc-b", "v4": "pvc-z"}
	canonicalByPV, members := drainSubsystemGroups(vols, []string{"v1", "v2", "v3", "v4"}, byVol)

	for _, pv := range []string{"pvc-a", "pvc-b", "pvc-c"} {
		if canonicalByPV[pv] != "pvc-a" {
			t.Errorf("canonical of %s = %q, want pvc-a (smallest name of the group)", pv, canonicalByPV[pv])
		}
	}
	if canonicalByPV["pvc-z"] != "pvc-z" {
		t.Errorf("a volume alone in its subsystem carries its own CR, got %q", canonicalByPV["pvc-z"])
	}
	if got := strings.Join(members["pvc-a"], ","); got != "pvc-a,pvc-b,pvc-c" {
		t.Errorf("members of the shared subsystem = %q, want pvc-a,pvc-b,pvc-c", got)
	}
	if len(members) != 2 {
		t.Errorf("expected 2 groups, got %d: %v", len(members), members)
	}
}

func TestDrainSubsystemGroups_NoNQNMeansOwnGroup(t *testing.T) {
	vols := []webapi.VolumeInfo{{UUID: "v1"}, {UUID: "v2"}}
	byVol := map[string]string{"v1": "pvc-1", "v2": "pvc-2"}
	canonicalByPV, members := drainSubsystemGroups(vols, []string{"v1", "v2"}, byVol)
	if len(members) != 2 || canonicalByPV["pvc-1"] != "pvc-1" || canonicalByPV["pvc-2"] != "pvc-2" {
		t.Errorf("volumes without an NQN must not be merged: %v / %v", canonicalByPV, members)
	}
}

func TestDrainMigrationVolumes_CountsMembersThenReportedThenOne(t *testing.T) {
	withMembers := &simplyblockv1alpha1.VolumeMigration{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{annoSubsystemMembers: "a,b,c"}}}
	reported := &simplyblockv1alpha1.VolumeMigration{}
	reported.Status.MemberCount = 4
	plain := &simplyblockv1alpha1.VolumeMigration{}
	if n := drainMigrationVolumes(withMembers); n != 3 {
		t.Errorf("members annotation: got %d, want 3", n)
	}
	if n := drainMigrationVolumes(reported); n != 4 {
		t.Errorf("reported member count: got %d, want 4", n)
	}
	if n := drainMigrationVolumes(plain); n != 1 {
		t.Errorf("plain CR: got %d, want 1", n)
	}
}

// drainVolumesServer answers the pool, volume and node listings the drain walks
// when it creates VolumeMigrations. Three volumes on the drained node: two share
// a subsystem, one is alone.
func drainVolumesServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
			_, _ = w.Write([]byte(`[{"id":"pool-1","name":"pool"}]`))
		case strings.HasSuffix(r.URL.Path, "/volumes/"):
			_, _ = w.Write([]byte(`[
				{"id":"vol-1","name":"pvc-1","nqn":"` + sharedNQN + `","storage_node_id":"` + opsTestNodeUUID + `","status":"online"},
				{"id":"vol-2","name":"pvc-2","nqn":"` + sharedNQN + `","storage_node_id":"` + opsTestNodeUUID + `","status":"online"},
				{"id":"vol-3","name":"pvc-3","nqn":"nqn.other:lvol:alone","storage_node_id":"` + opsTestNodeUUID + `","status":"online"}
			]`))
		case strings.HasSuffix(r.URL.Path, "/storage-nodes/"):
			_, _ = w.Write([]byte(`[
				{"id":"` + opsTestNodeUUID + `","status":"online"},
				{"id":"node-b","status":"online"},
				{"id":"node-c","status":"online"}
			]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func drainPV(name, volumeUUID string) client.Object {
	pv := newPV(name, volumeUUID)
	pv.Spec.CSI.VolumeHandle = "cluster-1:pool-1:" + volumeUUID
	pv.Spec.ClaimRef = nil // no PVC to fetch: PV-managed as is
	return pv
}

func TestCreateMissingVolumeMigrations_OneCRPerSubsystem(t *testing.T) {
	srv := drainVolumesServer(t)
	defer srv.Close()

	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	ops.Status.SubPhase = simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating
	r := newOpsReconciler(t, sn, ops,
		drainPV("pvc-1", "vol-1"), drainPV("pvc-2", "vol-2"), drainPV("pvc-3", "vol-3"))

	_, err := r.createMissingVolumeMigrationsOps(context.Background(), webapi.NewClient(srv.URL),
		"cluster-1", ops, sn, nil, map[string]struct{}{})
	if err != nil {
		t.Fatalf("createMissingVolumeMigrationsOps: %v", err)
	}

	var list simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &list, client.InNamespace(opsTestNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected one CR for the shared subsystem plus one for the lone volume, got %d", len(list.Items))
	}
	byPV := map[string]*simplyblockv1alpha1.VolumeMigration{}
	for i := range list.Items {
		byPV[list.Items[i].Spec.PVName] = &list.Items[i]
	}
	shared, ok := byPV["pvc-1"]
	if !ok {
		t.Fatalf("the shared subsystem's CR must be carried by its smallest PV name, got %v", byPV)
	}
	if got := shared.Annotations[annoSubsystemMembers]; got != "pvc-1,pvc-2" {
		t.Errorf("members = %q, want pvc-1,pvc-2", got)
	}
	if _, dup := byPV["pvc-2"]; dup {
		t.Errorf("pvc-2 shares pvc-1's subsystem and must not get its own CR")
	}
	if lone, ok := byPV["pvc-3"]; !ok {
		t.Errorf("the lone volume needs its own CR")
	} else if lone.Annotations[annoSubsystemMembers] != "" {
		t.Errorf("a lone volume carries no members annotation, got %q", lone.Annotations[annoSubsystemMembers])
	}

	updated := reloadOps(t, r)
	if updated.Status.VolumesPending != 3 || !strings.Contains(updated.Status.Message, "0 of 3 volumes") {
		t.Errorf("progress must count volumes, not CRs: pending=%d message=%q",
			updated.Status.VolumesPending, updated.Status.Message)
	}
}

func TestHasMissingVolumeMigrations_SiblingIsCoveredByItsSubsystemCR(t *testing.T) {
	srv := drainVolumesServer(t)
	defer srv.Close()

	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	r := newOpsReconciler(t, sn, ops,
		drainPV("pvc-1", "vol-1"), drainPV("pvc-2", "vol-2"), drainPV("pvc-3", "vol-3"))
	existing := map[string]struct{}{
		drainMigrationName(opsTestNodeUUID, "pvc-1"): {},
		drainMigrationName(opsTestNodeUUID, "pvc-3"): {},
	}
	if r.hasMissingVolumeMigrationsOps(context.Background(), webapi.NewClient(srv.URL), "cluster-1", opsTestNodeUUID, ops, existing, nil) {
		t.Errorf("pvc-2 is covered by pvc-1's subsystem CR; nothing is missing")
	}
	delete(existing, drainMigrationName(opsTestNodeUUID, "pvc-3"))
	if !r.hasMissingVolumeMigrationsOps(context.Background(), webapi.NewClient(srv.URL), "cluster-1", opsTestNodeUUID, ops, existing, nil) {
		t.Errorf("the lone volume's CR is gone; it is missing")
	}
}

// drainVolumesMidCutoverServer lists the drained node's volumes during a
// batch cutover: pvc-1, the group's canonical PV, has already moved to the
// target; pvc-2, its sibling in the same subsystem, still reads as on the
// drained node.
func drainVolumesMidCutoverServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
			_, _ = w.Write([]byte(`[{"id":"pool-1","name":"pool"}]`))
		case strings.HasSuffix(r.URL.Path, "/volumes/"):
			_, _ = w.Write([]byte(`[
				{"id":"vol-1","name":"pvc-1","nqn":"` + sharedNQN + `","storage_node_id":"node-target","status":"online"},
				{"id":"vol-2","name":"pvc-2","nqn":"` + sharedNQN + `","storage_node_id":"` + opsTestNodeUUID + `","status":"online"}
			]`))
		case strings.HasSuffix(r.URL.Path, "/storage-nodes/"):
			_, _ = w.Write([]byte(`[{"id":"` + opsTestNodeUUID + `","status":"online"},{"id":"node-target","status":"online"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// A sibling still on the drained node mid-cutover is covered by the CR that
// lists it as a member; it must not get a second VolumeMigration.
func TestDrain_ASiblingMidCutoverIsNotScheduledTwice(t *testing.T) {
	srv := drainVolumesMidCutoverServer(t)
	defer srv.Close()

	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	existingCR := simplyblockv1alpha1.VolumeMigration{}
	existingCR.Name = drainMigrationName(opsTestNodeUUID, "pvc-1")
	existingCR.Namespace = opsTestNS
	existingCR.Annotations = map[string]string{annoSubsystemMembers: "pvc-1,pvc-2"}
	existingCR.Spec.PVName = "pvc-1"
	existingCR.Spec.TargetNodeUUID = "node-target"
	existingCR.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseRunning
	r := newOpsReconciler(t, sn, ops, drainPV("pvc-1", "vol-1"), drainPV("pvc-2", "vol-2"))

	items := []simplyblockv1alpha1.VolumeMigration{existingCR}
	names := map[string]struct{}{existingCR.Name: {}}
	api := webapi.NewClient(srv.URL)
	if r.hasMissingVolumeMigrationsOps(context.Background(), api, "cluster-1", opsTestNodeUUID, ops, names, items) {
		t.Error("pvc-2 is listed as a member of the running CR; nothing is missing")
	}
	if _, err := r.createMissingVolumeMigrationsOps(context.Background(), api, "cluster-1", ops, sn, items, names); err != nil {
		t.Fatalf("createMissingVolumeMigrationsOps: %v", err)
	}
	var list simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(context.Background(), &list, client.InNamespace(opsTestNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("no new VolumeMigration may be created for a covered sibling, got %d", len(list.Items))
	}
}

func TestDrainCoveredPVsIncludesMembers(t *testing.T) {
	a := simplyblockv1alpha1.VolumeMigration{}
	a.Spec.PVName = "pvc-a"
	b := simplyblockv1alpha1.VolumeMigration{}
	b.Spec.PVName = "pvc-b"
	b.Annotations = map[string]string{annoSubsystemMembers: "pvc-b,pvc-c"}
	got := drainCoveredPVs([]simplyblockv1alpha1.VolumeMigration{a, b})
	for _, pv := range []string{"pvc-a", "pvc-b", "pvc-c"} {
		if _, ok := got[pv]; !ok {
			t.Errorf("%s must be covered", pv)
		}
	}
	if len(got) != 3 {
		t.Errorf("covered = %v, want exactly pvc-a, pvc-b, pvc-c", got)
	}
}
