// A cutover the slot did not see complete (bf24 2026-11-14): with
// --delete-source the source volume is deleted right after the ANA flip, and a
// slot that polls after that reads 404 where cutover_done was. Whether the
// volume moved is the control plane's relationship record; these tests pin
// that the slot, the cluster-scope selection and the operation's outcome all
// follow it.

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	mcClusterA = "aaaaaaaa-0000-0000-0000-000000000001"
	mcClusterB = "bbbbbbbb-0000-0000-0000-000000000001"
	mcPoolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
	mcPoolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
	mcVolA     = "f100ae0c-554b-47be-a072-4051d0cb5e8e" // the source, on A
	mcVolB     = "716869d3-119a-449c-bfa6-47852a5b3b8c" // where the cutover put it, on B
)

var mcHandleA = mcClusterA + ":" + mcPoolA + ":" + mcVolA

// movedBackend is a fake control plane for a volume that may have moved from
// A to B. The source's replication read and volume read answer as set; the
// relationship records say whether it moved.
type movedBackend struct {
	// moved: A's relationship names B as active. relErr: the relationship
	// read fails.
	moved, relErr bool
	// sourceGone: the source volume (and so its replication read) is 404.
	sourceGone bool
	// sourceState is the source's replication state when it exists ("" = 404).
	sourceState string
	puts        int
}

func (b *movedBackend) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && path == "/api/v2/clusters/"+mcClusterA+"/replication/relationships/"+mcVolA:
		if b.relErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		active := mcVolA
		if b.moved {
			active = mcVolB
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"is_source": true, "target_cluster_id": mcClusterB, "target_pool_id": mcPoolB,
			"target_lvol_id": mcVolB, "active_lvol_id": active,
		})
	case req.Method == http.MethodGet && strings.Contains(path, "/replication/relationships/"):
		w.WriteHeader(http.StatusNotFound)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/volumes/"+mcVolA+"/replication"):
		if b.sourceGone || b.sourceState == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: b.sourceState, IsSource: true})
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication/status"):
		_ = json.NewEncoder(w).Encode(map[string]any{"role": "source", "state": "in_sync", "resyncing": false})
	case req.Method == http.MethodGet && strings.HasSuffix(strings.TrimSuffix(path, "/"), "/volumes/"+mcVolA):
		if b.sourceGone {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": mcVolA, "do_replicate": true})
	case req.Method == http.MethodPut:
		b.puts++
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func mcSlot(state string, annotations map[string]string) *simplyblockv1alpha1.ReplicationSlot {
	slot := newTestSlot(state)
	slot.Spec.VolumeID = mcHandleA
	slot.Status.Direction = string(simplyblockv1alpha1.ReplicationSlotDirectionSource)
	slot.Annotations = annotations
	return slot
}

func reconcileSlotOnce(t *testing.T, r *ReplicationSlotReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), slotRequest("slot1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func assertCutOverToB(t *testing.T, got *simplyblockv1alpha1.ReplicationSlot) {
	t.Helper()
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone) {
		t.Fatalf("State = %q (%s), want cutover_done", got.Status.State, got.Status.Message)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionTarget) {
		t.Errorf("Direction = %q, want target", got.Status.Direction)
	}
	if got.Status.TargetLvolID != mcVolB {
		t.Errorf("TargetLvolID = %q, want %s", got.Status.TargetLvolID, mcVolB)
	}
	if got.Spec.VolumeID != mcHandleA {
		t.Errorf("spec.volumeID = %q, want the source kept, as on every finished slot", got.Spec.VolumeID)
	}
}

// bf24 vol-3/vol-6: proceed signalled, the backend cut over and deleted the
// source before the next poll. The slot is cut over, not released.
func TestSlot_CutoverPending_SourceDeletedAfterCompletedCutover_CutsOver(t *testing.T) {
	b := &movedBackend{moved: true, sourceGone: true}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, mcSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending), map[string]string{
		annotCutoverObserved:        annotCutoverObservedValue,
		annotCutoverProceedSignaled: annotCutoverProceedSignaledValue,
	}), readyReplicationPolicy())

	reconcileSlotOnce(t, r)
	assertCutOverToB(t, getSlot(t, cl))

	// And it stays there: the next pass does not revert it.
	reconcileSlotOnce(t, r)
	assertCutOverToB(t, getSlot(t, cl))
	if b.puts != 0 {
		t.Errorf("PUT calls = %d, want no re-attach of the deleted source", b.puts)
	}
}

// Without --delete-source the source survives and may already report a fresh
// replicating record; the relationship still says the data moved.
func TestSlot_CutoverPending_SourceKeptAndReplicatingAfterCompletedCutover_CutsOver(t *testing.T) {
	b := &movedBackend{moved: true, sourceState: utils.ReplicationBackendStateReplicating}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, mcSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending), map[string]string{
		annotCutoverObserved:        annotCutoverObservedValue,
		annotCutoverProceedSignaled: annotCutoverProceedSignaledValue,
	}), readyReplicationPolicy())

	reconcileSlotOnce(t, r)
	assertCutOverToB(t, getSlot(t, cl))
}

// A relationship that did not move is a cutover that really was abandoned:
// the #856 release still applies.
func TestSlot_CutoverPending_NotMoved_StillReleases(t *testing.T) {
	b := &movedBackend{sourceState: utils.ReplicationBackendStateReplicating}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, mcSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending), map[string]string{
		annotCutoverObserved: annotCutoverObservedValue,
	}), readyReplicationPolicy())

	reconcileSlotOnce(t, r)
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q, want replicating for an abandoned cutover", got.Status.State)
	}
}

// When the relationship cannot be read, nothing is decided: releasing a
// completed cutover is the bf24 failure.
func TestSlot_CutoverPending_RelationshipUnreadable_Waits(t *testing.T) {
	b := &movedBackend{relErr: true, sourceGone: true}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, mcSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending), map[string]string{
		annotCutoverObserved: annotCutoverObservedValue,
	}), readyReplicationPolicy())

	reconcileSlotOnce(t, r)
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Errorf("State = %q, want cutover_pending until the relationship can be read", got.Status.State)
	}
}

// The slots bf24 left behind: released to replicating by the older operator,
// still addressing the deleted source. A replicating poll repairs them.
func TestSlot_Replicating_SourceGoneAndMoved_Repairs(t *testing.T) {
	b := &movedBackend{moved: true, sourceGone: true}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	slot := mcSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating), nil)
	slot.Status.Message = "Cutover did not complete; the backend has no active replication relationship"
	r, cl := newSlotReconciler(t, slot, readyReplicationPolicy())

	reconcileSlotOnce(t, r)
	assertCutOverToB(t, getSlot(t, cl))
}

// A replicating slot whose volume is gone without having moved is left as it
// was: there is no location to adopt.
func TestSlot_Replicating_SourceGoneNotMoved_Unchanged(t *testing.T) {
	b := &movedBackend{sourceGone: true}
	srv := newAPIServer(t, b.serve)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	r, cl := newSlotReconciler(t, mcSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating), nil), readyReplicationPolicy())

	reconcileSlotOnce(t, r)
	if got := getSlot(t, cl); got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q, want replicating", got.Status.State)
	}
}

// ---------- cluster-scope selection ----------

// mcClusterScope builds a cluster migration from source (site-a / site-b)
// under policyName, with one claim whose slot sits replicating on the deleted
// A source under slotPolicy.
func mcClusterScope(t *testing.T, source, policyName, slotPolicy string) (*ReplicationOpsReconciler, client.Client, *planServer) {
	t.Helper()
	siteA := testCluster("default", "site-a", mcClusterA)
	siteB := testCluster("default", "site-b", mcClusterB)
	pair := readyPairForOps()
	pair.Name = "pair-" + source
	pair.Spec.SourceCluster = source
	pol := migrationPolicy()
	pol.Name = policyName
	pol.Spec.PairRef = pair.Name
	ops := migrationOps(utils.ReplicationOpsScopeCluster, source)
	ops.Spec.PolicyRef = policyName
	ops.Spec.DeleteSource = true
	stale := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: slotPolicy + "-vol-3", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: slotPolicy, PVCRef: "vol-3", VolumeID: mcHandleA},
		Status: simplyblockv1alpha1.ReplicationSlotStatus{
			State:     string(simplyblockv1alpha1.ReplicationSlotStateReplicating),
			Direction: string(simplyblockv1alpha1.ReplicationSlotDirectionSource),
			Message:   "Cutover did not complete; the backend has no active replication relationship",
		},
	}
	r, cl := newOpsReplReconciler(t, siteA, siteB, pair, pol, ops, stale,
		migBoundPVC("vol-3", "pv-3", map[string]string{utils.AnnotationReplicationPolicy: slotPolicy}),
		csiPV("pv-3", mcHandleA),
	)
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusBadRequest, `{"detail":"volumes not on cluster"}`
	}}
	b := &movedBackend{moved: true, sourceGone: true}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/migration-plans/") {
			ps.handler(w, req)
			return
		}
		b.serve(w, req)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	return r, cl, ps
}

// bf24 round 5 (A -> B again): the stale slot made vol-3 look like it was
// still on A, and the plan was refused with 400. It moved to B: skipped as
// moved, no plan asked for it.
func TestMigration_ClusterScope_StaleSlotOfAMovedVolumeDoesNotPlaceItOnTheSource(t *testing.T) {
	r, cl, ps := mcClusterScope(t, "site-a", "a2b", "a2b")

	reconcileOps(t, r)

	if len(ps.posts) != 0 {
		t.Fatalf("asked for a plan with %v; the volume is no longer on the source", ps.posts)
	}
	got := getOps(t, cl)
	var moved bool
	for _, res := range got.Status.Results {
		if res.PVCRef == "vol-3" && res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) &&
			strings.Contains(res.Detail, mcClusterB) {
			moved = true
		}
	}
	if !moved {
		t.Errorf("results = %+v, want vol-3 skipped as moved to %s", got.Status.Results, mcClusterB)
	}
	if got.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = Failed: %s", got.Status.Message)
	}
}

// bf24 round 4 (B -> A): vol-3 lives on B, but its stale slot under the A -> B
// policy looked live, so it was not migrated. The slot is superseded like a
// finished one and the claim is enrolled under the new policy.
func TestMigration_ClusterScope_StaleSlotOfAnotherPolicyIsSuperseded(t *testing.T) {
	r, cl, _ := mcClusterScope(t, "site-b", "b2a", "a2b")

	reconcileOps(t, r)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "vol-3"}, pvc); err != nil {
		t.Fatal(err)
	}
	if got := pvc.Annotations[utils.AnnotationReplicationPolicy]; got != "default/b2a" && got != "b2a" {
		t.Errorf("vol-3 policy = %q, want b2a: its slot names a volume that has moved", got)
	}
	for _, res := range getOps(t, cl).Status.Results {
		if res.PVCRef == "vol-3" && res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
			t.Errorf("vol-3 skipped: %s", res.Detail)
		}
	}
}

// Under the same policy (A -> B -> A on one pair) the stale slot holds the
// name the new slot needs: it is retired.
func TestMigration_ClusterScope_StaleSlotOfTheSamePolicyIsRetired(t *testing.T) {
	r, cl, _ := mcClusterScope(t, "site-b", "b2a", "b2a")

	reconcileOps(t, r)

	err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "b2a-vol-3"},
		&simplyblockv1alpha1.ReplicationSlot{})
	if err == nil {
		t.Error("the stale slot is still there; the watcher cannot create one on the moved volume")
	}
}

// ---------- the operation's outcome ----------

// A plan reports a volume done while its slot still names the moved-away
// source: the operation does not succeed until the slot has caught up.
func TestMigration_PlanDone_WaitsForASlotStillOnTheMovedSource(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON(mcVolA, "replicating", 600))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanDone, planVolumeJSON(mcVolA, "done", 0))
	b := &movedBackend{moved: true, sourceGone: true}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/migration-plans/") {
			ps.handler(w, req)
			return
		}
		b.serve(w, req)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	slot := slotForPolicy()
	slot.Spec.VolumeID = mcHandleA
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slot,
		migrationOps(utils.ReplicationOpsScopePolicy, "pol"))

	reconcileOps(t, r) // creates the plan
	reconcileOps(t, r) // plan done, slot lagging

	got := getOps(t, cl)
	if got.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Fatalf("Succeeded while the slot still names the deleted source")
	}
	if got.Status.Subphase != replOpsSubphaseTrackingPlan || !strings.Contains(got.Status.Message, "waiting for 1 volume") {
		t.Errorf("subphase/message = %q / %q", got.Status.Subphase, got.Status.Message)
	}

	// The slot reconciler cuts it over; the operation then succeeds.
	s := migGetSlot(t, cl, testPolicySlotPVC1)
	patch := client.MergeFrom(s.DeepCopy())
	s.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone)
	if err := cl.Status().Patch(context.Background(), s, patch); err != nil {
		t.Fatal(err)
	}
	reconcileOps(t, r)
	if got := getOps(t, cl); got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q (%s), want Succeeded once the slot has cut over", got.Status.Phase, got.Status.Message)
	}
}

// A slot whose volume the control plane does not report as moved is not
// waited for: nothing would ever change it.
func TestMigration_PlanDone_DoesNotWaitOnASlotWhoseVolumeDidNotMove(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON(mcVolA, "replicating", 600))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanDone, planVolumeJSON(mcVolA, "done", 0))
	b := &movedBackend{}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/migration-plans/") {
			ps.handler(w, req)
			return
		}
		b.serve(w, req)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
	slot := slotForPolicy()
	slot.Spec.VolumeID = mcHandleA
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slot,
		migrationOps(utils.ReplicationOpsScopePolicy, "pol"))

	reconcileOps(t, r)
	reconcileOps(t, r)
	if got := getOps(t, cl); got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q (%s), want Succeeded", got.Status.Phase, got.Status.Message)
	}
}
