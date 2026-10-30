// Unit tests for the ReplicationOps migration action: one migration plan per
// operation, its progress and forecast mirrored into status, the terminal
// outcomes, the dry run, cluster-scope enrollment, and the per-volume commit
// path used when the control plane has no migration plans.

package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// Names the migration tests share: the plan the fake control plane creates,
// the migration policy an operation follows, and a second pair and policy for
// tests that move volumes onward.
const (
	testPlanID         = "plan-1"
	testMigPolicyName  = "mig"
	testPairBName      = "pair-b"
	testMigPolicyBName = "mig-b"
)

// planServer is a fake control plane serving the migration-plans API. It
// records every request body and serves the plan the test sets.
type planServer struct {
	mu        sync.Mutex
	posts     []map[string]interface{}
	commits   []string
	gets      int
	postReply func() (int, string)
	planBody  string
	listBody  string
}

func (p *planServer) handler(w http.ResponseWriter, req *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/migration-plans/"):
		raw, _ := io.ReadAll(req.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		p.posts = append(p.posts, body)
		code, out := p.postReply()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/migration-plans/"):
		if p.listBody == "" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte(p.listBody))
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/migration-plans/"):
		p.gets++
		_, _ = w.Write([]byte(p.planBody))
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/replication/commit"):
		p.commits = append(p.commits, req.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func planJSON(id, status string, volumes ...string) string {
	idField := "null"
	if id != "" {
		idField = `"` + id + `"`
	}
	return `{"id":` + idField + `,"status":"` + status + `","volumes":[` + strings.Join(volumes, ",") +
		`],"forecast":{"bytes_remaining":1073741824,"eta_seconds":600.2,"completes_at":"2026-10-22T12:00:00Z",` +
		`"throughput_bytes_per_sec":209715200,"throughput_measured":true}}`
}

func planVolumeJSON(lvolID, state string, eta float64) string {
	b, _ := json.Marshal(map[string]interface{}{
		"lvol_id": lvolID, "name": "lv", "state": state, "target_node_id": "tnode-1",
		"error": "", "bytes_total": 2048, "bytes_remaining": 1024, "eta_seconds": eta,
	})
	return string(b)
}

func migrationOps(scope, ref string) *simplyblockv1alpha1.ReplicationOps {
	return &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: testReplOpsName, Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{Action: "migration", Scope: scope, Ref: ref},
	}
}

func migGetSlot(t *testing.T, cl client.Client, name string) *simplyblockv1alpha1.ReplicationSlot {
	t.Helper()
	s := &simplyblockv1alpha1.ReplicationSlot{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, s); err != nil {
		t.Fatalf("get slot %s: %v", name, err)
	}
	return s
}

func reconcileOps(t *testing.T, r *ReplicationOpsReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), opsRequest(testReplOpsName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// ---------- plan flow ----------

func TestMigration_CreatesOnePlanAndKeepsRunning(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 600))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "converging", 30))
	ops := migrationOps(utils.ReplicationOpsScopePolicy, "pol")
	ops.Spec.DeleteSource = true
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(), ops)
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)
	reconcileOps(t, r)

	if len(ps.posts) != 1 {
		t.Fatalf("POST migration-plans %d times, want exactly 1", len(ps.posts))
	}
	body := ps.posts[0]
	if body["delete_source"] != true {
		t.Errorf("plan body delete_source = %v, want true", body["delete_source"])
	}
	if ids, _ := body["lvol_ids"].([]interface{}); len(ids) != 1 || ids[0] != "vol-u" {
		t.Errorf("plan body lvol_ids = %v, want [vol-u]", body["lvol_ids"])
	}
	if ps.gets != 1 {
		t.Errorf("GET plan %d times, want 1 (second pass tracks)", ps.gets)
	}

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseRunning) {
		t.Fatalf("phase = %q, want Running while the plan runs", got.Status.Phase)
	}
	if got.Status.PlanID != testPlanID || got.Status.Subphase != replOpsSubphaseTrackingPlan {
		t.Errorf("planID/subphase = %q/%q, want plan-1/%s", got.Status.PlanID, got.Status.Subphase, replOpsSubphaseTrackingPlan)
	}
	if len(got.Status.Results) != 1 {
		t.Fatalf("results = %+v, want 1", got.Status.Results)
	}
	res := got.Status.Results[0]
	if res.SlotRef != testPolicySlotPVC1 || res.PVCRef != "pvc1" || res.State != "converging" ||
		res.Status != string(simplyblockv1alpha1.ReplicationOpsResultPending) || res.ETASeconds != 30 ||
		res.BytesRemaining != 1024 || res.TargetNodeID != "tnode-1" {
		t.Errorf("result = %+v, want the plan's progress for pol-pvc1", res)
	}
	f := got.Status.Forecast
	if f == nil || f.ETASeconds != 601 || f.BytesRemaining != 1<<30 || !f.ThroughputMeasured ||
		f.ThroughputBytesPerSec != 209715200 || f.CompletesAt == nil {
		t.Errorf("forecast = %+v, want the plan's forecast", f)
	}

	// The slot state machine stays the slot reconciler's: no pre-marking.
	if s := migGetSlot(t, cl, testPolicySlotPVC1); s.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("slot state = %q, want it left at replicating", s.Status.State)
	}
}

func TestMigration_PlanDone_Succeeds(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 600))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanDone, planVolumeJSON("vol-u", "done", 0))
	pol := readyPolicyForOps("pol")
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slotForPolicy(), migrationOps(utils.ReplicationOpsScopePolicy, "pol"))
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)
	reconcileOps(t, r)

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Fatalf("phase = %q, want Succeeded once the plan is done", got.Status.Phase)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) {
		t.Errorf("results = %+v, want 1 succeeded", got.Status.Results)
	}
	p := &simplyblockv1alpha1.ReplicationPolicy{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, p)
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("policy lock = %q, want released", p.Status.ActiveOpsRef)
	}
}

func TestMigration_PlanPartial_FailsWithResults(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 600))
	}}
	ps.planBody = planJSON(testPlanID, migrationPlanPartial, planVolumeJSON("vol-u", "failed", 0))
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(),
		migrationOps(utils.ReplicationOpsScopePolicy, "pol"))
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)
	reconcileOps(t, r)

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Fatalf("phase = %q, want Failed for a partial plan", got.Status.Phase)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultFailed) {
		t.Errorf("results = %+v, want the failed volume kept", got.Status.Results)
	}
	if !strings.Contains(got.Status.Message, "1 of 1") {
		t.Errorf("message = %q, want the count of volumes that did not migrate", got.Status.Message)
	}
}

func TestMigration_WaitsForSlotsToReplicateBeforeThePlan(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) { return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning) }}
	slot := slotForPolicy()
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateAttaching)
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slot, migrationOps(utils.ReplicationOpsScopePolicy, "pol"))
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	res, err := r.Reconcile(context.Background(), opsRequest(testReplOpsName))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ps.posts) != 0 {
		t.Fatalf("created a plan while a slot was still attaching")
	}
	if res.RequeueAfter != replOpsWaitPoll {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, replOpsWaitPoll)
	}
	got := getOps(t, cl)
	if got.Status.Subphase != replOpsSubphaseAwaitingReplication {
		t.Errorf("subphase = %q, want %s", got.Status.Subphase, replOpsSubphaseAwaitingReplication)
	}
}

func TestMigration_AdoptsAPlanCreatedByADeadPass(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) { return http.StatusCreated, planJSON("plan-2", migrationPlanRunning) }}
	ops := migrationOps(utils.ReplicationOpsScopePolicy, "pol")
	ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseRunning)
	ops.Status.Subphase = replOpsSubphaseCreatingPlan
	ps.listBody = `[{"id":"` + testPlanID + `","name":"` + migrationPlanName(ops) + `","status":"running","volumes":[]}]`
	pol := readyPolicyForOps("pol")
	pol.Status.ActiveOpsRef = testReplOpsName
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slotForPolicy(), ops)
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	if len(ps.posts) != 0 {
		t.Fatalf("created a second plan instead of adopting plan-1")
	}
	if got := getOps(t, cl); got.Status.PlanID != testPlanID {
		t.Errorf("planID = %q, want the adopted plan-1", got.Status.PlanID)
	}
}

func TestMigration_DryRun_PublishesForecastWithoutSideEffects(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusOK, planJSON("", migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 600))
	}}
	ops := migrationOps(utils.ReplicationOpsScopePolicy, "pol")
	ops.Spec.DryRun = true
	slot := slotForPolicy()
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateAttaching) // a dry run does not wait
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slot, ops)
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	if len(ps.posts) != 1 || ps.posts[0]["dry_run"] != true {
		t.Fatalf("posts = %v, want one dry_run=true request", ps.posts)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Fatalf("phase = %q, want Succeeded after a dry run", got.Status.Phase)
	}
	if got.Status.PlanID != "" {
		t.Errorf("planID = %q, want none for a dry run", got.Status.PlanID)
	}
	if got.Status.Forecast == nil || got.Status.Forecast.ETASeconds != 601 {
		t.Errorf("forecast = %+v, want the dry run's forecast", got.Status.Forecast)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].TargetNodeID != "tnode-1" {
		t.Errorf("results = %+v, want the planned target node", got.Status.Results)
	}
	if len(ps.commits) != 0 {
		t.Errorf("a dry run committed %v", ps.commits)
	}
}

func TestMigration_BackendWithoutPlans_CommitsPerVolumeAndTracksSlots(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) { return http.StatusNotFound, "" }}
	ops := migrationOps(utils.ReplicationOpsScopePolicy, "pol")
	ops.Spec.DeleteSource = true
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(), ops)
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	if len(ps.commits) != 1 {
		t.Fatalf("commits = %v, want one per-volume commit", ps.commits)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseRunning) ||
		got.Status.Subphase != replOpsSubphaseAwaitingCutover {
		t.Fatalf("phase/subphase = %q/%q, want Running/%s until the slot cuts over",
			got.Status.Phase, got.Status.Subphase, replOpsSubphaseAwaitingCutover)
	}
	slot := migGetSlot(t, cl, testPolicySlotPVC1)
	if slot.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Fatalf("slot state = %q, want cutover_pending on the commit path", slot.Status.State)
	}

	// The slot reconciler finishes the cutover.
	patch := client.MergeFrom(slot.DeepCopy())
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone)
	if err := cl.Status().Patch(context.Background(), slot, patch); err != nil {
		t.Fatalf("patch slot: %v", err)
	}
	reconcileOps(t, r)

	if len(ps.commits) != 1 {
		t.Errorf("commits = %v, want no second commit", ps.commits)
	}
	got = getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded once every slot cut over", got.Status.Phase)
	}
}

func TestMigration_TargetScopeUsesAPlan(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning, planVolumeJSON("vol-u", "replicating", 60))
	}}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), slotForPolicy(),
		migrationOps(utils.ReplicationOpsScopeTarget, "pair1"))
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	if len(ps.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(ps.posts))
	}
	if got := getOps(t, cl); got.Status.PlanID != testPlanID {
		t.Errorf("planID = %q, want plan-1 (status %+v)", got.Status.PlanID, got.Status)
	}
}

// ---------- cluster scope ----------

func migBoundPVC(name, pvName string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: annotations},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func csiPV(name, handle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.simplyblock.io", VolumeHandle: handle},
		}},
	}
}

func migrationPolicy() *simplyblockv1alpha1.ReplicationPolicy {
	p := readyPolicyForOps(testMigPolicyName)
	p.Spec.Mode = utils.ReplicationPolicyModeMigration
	return p
}

func TestMigration_ClusterScope_EnrollsSkipsAndThenPlans(t *testing.T) {
	ps := &planServer{postReply: func() (int, string) {
		return http.StatusCreated, planJSON(testPlanID, migrationPlanRunning,
			planVolumeJSON("vol-a", "replicating", 100), planVolumeJSON("vol-d", "queued", 200))
	}}
	ops := migrationOps(utils.ReplicationOpsScopeCluster, testClusterName)
	ops.Spec.PolicyRef = testMigPolicyName
	src := testClusterUUID + ":pool:"
	slotD := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-pvc-d", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: testMigPolicyName, PVCRef: "pvc-d", VolumeID: src + "vol-d"},
		Status:     simplyblockv1alpha1.ReplicationSlotStatus{State: string(simplyblockv1alpha1.ReplicationSlotStateReplicating)},
	}
	slotE := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-pvc-e", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: "dr", PVCRef: "pvc-e", VolumeID: src + "vol-e"},
		Status:     simplyblockv1alpha1.ReplicationSlotStatus{State: string(simplyblockv1alpha1.ReplicationSlotStateReplicating)},
	}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), migrationPolicy(), ops,
		migBoundPVC("pvc-a", "pv-a", nil), csiPV("pv-a", src+"vol-a"), // to enroll
		migBoundPVC("pvc-b", "pv-b", nil), csiPV("pv-b", "other-cluster:pool:vol-b"), // another cluster
		migBoundPVC("pvc-c", "pv-c", map[string]string{utils.AnnotationReplicationPolicy: "dr"}), csiPV("pv-c", src+"vol-c"),
		migBoundPVC("pvc-d", "pv-d", map[string]string{utils.AnnotationReplicationPolicy: testMigPolicyName}), csiPV("pv-d", src+"vol-d"), slotD,
		migBoundPVC("pvc-e", "pv-e", nil), csiPV("pv-e", src+"vol-e"), slotE,
	)
	srv := newAPIServer(t, ps.handler)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	if len(ps.posts) != 0 {
		t.Fatalf("created a plan before the enrolled claim had a slot")
	}
	pvcA := &corev1.PersistentVolumeClaim{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pvc-a"}, pvcA)
	if pvcA.Annotations[utils.AnnotationReplicationPolicy] != testMigPolicyName {
		t.Errorf("pvc-a annotations = %v, want enrolled under mig", pvcA.Annotations)
	}
	pvcB := &corev1.PersistentVolumeClaim{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pvc-b"}, pvcB)
	if _, ok := pvcB.Annotations[utils.AnnotationReplicationPolicy]; ok {
		t.Errorf("annotated pvc-b, which lives on another cluster")
	}
	pvcC := &corev1.PersistentVolumeClaim{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pvc-c"}, pvcC)
	if pvcC.Annotations[utils.AnnotationReplicationPolicy] != "dr" {
		t.Errorf("overwrote pvc-c's policy: %v", pvcC.Annotations)
	}
	got := getOps(t, cl)
	if got.Status.Subphase != replOpsSubphaseEnrolling {
		t.Errorf("subphase = %q, want %s", got.Status.Subphase, replOpsSubphaseEnrolling)
	}
	skipped := map[string]bool{}
	for _, res := range got.Status.Results {
		if res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
			skipped[res.PVCRef] = true
		}
	}
	if !skipped["pvc-c"] || !skipped["pvc-e"] || skipped["pvc-b"] || len(skipped) != 2 {
		t.Errorf("skipped = %v, want pvc-c and pvc-e only", skipped)
	}

	// The PVC watcher creates pvc-a's slot and the slot reconciler attaches it.
	slotA := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-pvc-a", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: testMigPolicyName, PVCRef: "pvc-a", VolumeID: src + "vol-a"},
	}
	if err := cl.Create(context.Background(), slotA); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	patch := client.MergeFrom(slotA.DeepCopy())
	slotA.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateReplicating)
	if err := cl.Status().Patch(context.Background(), slotA, patch); err != nil {
		t.Fatalf("patch slot: %v", err)
	}

	reconcileOps(t, r)

	if len(ps.posts) != 1 {
		t.Fatalf("posts = %d, want 1 plan once every claim replicates", len(ps.posts))
	}
	ids, _ := ps.posts[0]["lvol_ids"].([]interface{})
	gotIDs := map[interface{}]bool{}
	for _, id := range ids {
		gotIDs[id] = true
	}
	if len(ids) != 2 || !gotIDs["vol-a"] || !gotIDs["vol-d"] {
		t.Errorf("plan lvol_ids = %v, want vol-a and vol-d", ids)
	}
	got = getOps(t, cl)
	if got.Status.PlanID != testPlanID {
		t.Fatalf("planID = %q, want plan-1", got.Status.PlanID)
	}
	// The skips from enrollment stay next to the plan's volumes.
	if len(got.Status.Results) != 4 {
		t.Errorf("results = %+v, want 2 skipped + 2 planned", got.Status.Results)
	}
}

func TestMigration_ClusterScope_RefusesAFailoverPolicy(t *testing.T) {
	ops := migrationOps(utils.ReplicationOpsScopeCluster, testClusterName)
	ops.Spec.PolicyRef = "pol" // mode defaults to failover
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	reconcileOps(t, r)

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed for a failover-mode policy", got.Status.Phase)
	}
}

// A PV keeps the handle it was provisioned with, so a cluster-scoped migration
// that chose volumes by that handle could never move a volume again once it
// had migrated: the handle still names the cluster it left. Selection goes by
// where each volume lives now, and a completed migration's slot gives way to
// the new policy instead of pinning the claim to the old one.
func TestMigration_ClusterScope_SelectsVolumesByWhereTheyLiveNow(t *testing.T) {
	const (
		clusterA = "aaaaaaaa-0000-0000-0000-000000000001"
		clusterB = "bbbbbbbb-0000-0000-0000-000000000001"
		clusterC = "cccccccc-0000-0000-0000-000000000001"
		poolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
		poolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
		poolC    = "cccccccc-0000-0000-0000-00000000000c"
		volA     = "11111111-1111-1111-1111-111111111111" // pvc-m's original, on A
		volB     = "22222222-2222-2222-2222-222222222222" // pvc-m migrated to B
		volN     = "33333333-3333-3333-3333-333333333333" // pvc-n's original, on B
		volC     = "44444444-4444-4444-4444-444444444444" // pvc-n migrated to C
		volO     = "55555555-5555-5555-5555-555555555555" // pvc-o, never moved, on B
	)
	siteB := testCluster("default", "site-b", clusterB)
	pair := readyPairForOps()
	pair.Name = testPairBName
	pair.Spec.SourceCluster = "site-b"
	pol := migrationPolicy()
	pol.Name = testMigPolicyBName
	pol.Spec.PairRef = testPairBName
	ops := migrationOps(utils.ReplicationOpsScopeCluster, "site-b")
	ops.Spec.PolicyRef = testMigPolicyBName

	finished := func(name, policy, pvc, handle string) *simplyblockv1alpha1.ReplicationSlot {
		return &simplyblockv1alpha1.ReplicationSlot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: policy, PVCRef: pvc, VolumeID: handle},
			Status: simplyblockv1alpha1.ReplicationSlotStatus{
				State:     string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone),
				Direction: string(simplyblockv1alpha1.ReplicationSlotDirectionTarget),
			},
		}
	}
	r, cl := newOpsReplReconciler(t, siteB, pair, pol, ops,
		migBoundPVC("pvc-m", "pv-m", map[string]string{utils.AnnotationReplicationPolicy: "old"}),
		csiPV("pv-m", clusterA+":"+poolA+":"+volA),
		finished("old-pvc-m", "old", "pvc-m", clusterA+":"+poolA+":"+volA),
		migBoundPVC("pvc-n", "pv-n", map[string]string{utils.AnnotationReplicationPolicy: "away"}),
		csiPV("pv-n", clusterB+":"+poolB+":"+volN),
		finished("away-pvc-n", "away", "pvc-n", clusterB+":"+poolB+":"+volN),
		migBoundPVC("pvc-o", "pv-o", nil), csiPV("pv-o", clusterB+":"+poolB+":"+volO),
	)

	rel := func(w http.ResponseWriter, isSource bool, tc, tp, tv, active string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"is_source": isSource, "target_cluster_id": tc, "target_pool_id": tp,
			"target_lvol_id": tv, "active_lvol_id": active,
		})
	}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch req.URL.Path {
		case "/api/v2/clusters/" + clusterA + "/replication/relationships/" + volA:
			rel(w, true, clusterB, poolB, volB, volB)
		case "/api/v2/clusters/" + clusterB + "/replication/relationships/" + volB:
			rel(w, false, clusterB, poolB, volB, volB)
		case "/api/v2/clusters/" + clusterB + "/replication/relationships/" + volN:
			rel(w, true, clusterC, poolC, volC, volC)
		case "/api/v2/clusters/" + clusterC + "/replication/relationships/" + volC:
			rel(w, false, clusterC, poolC, volC, volC)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	annotation := func(name string) string {
		pvc := &corev1.PersistentVolumeClaim{}
		_ = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, pvc)
		return pvc.Annotations[utils.AnnotationReplicationPolicy]
	}
	if got := annotation("pvc-m"); got != testMigPolicyBName {
		t.Errorf("pvc-m policy = %q, want mig-b: it migrated to B, and its finished slot must give way", got)
	}
	if got := annotation("pvc-o"); got != testMigPolicyBName {
		t.Errorf("pvc-o policy = %q, want mig-b", got)
	}
	if got := annotation("pvc-n"); got != "away" {
		t.Errorf("pvc-n policy = %q, want it left alone: it moved on to C", got)
	}

	got := getOps(t, cl)
	if got.Status.Subphase != replOpsSubphaseEnrolling {
		t.Errorf("subphase = %q, want %s", got.Status.Subphase, replOpsSubphaseEnrolling)
	}
	var skippedN bool
	for _, res := range got.Status.Results {
		if res.PVCRef == "pvc-m" && res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
			t.Errorf("pvc-m skipped: %s", res.Detail)
		}
		if res.PVCRef == "pvc-n" && res.Status == string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
			skippedN = true
			if !strings.Contains(res.Detail, clusterC) {
				t.Errorf("pvc-n detail = %q, want it to name the cluster it moved to", res.Detail)
			}
		}
	}
	if !skippedN {
		t.Errorf("results = %+v, want pvc-n reported as moved away", got.Status.Results)
	}
}

// A volume back on the cluster it once left under the same policy (A -> B ->
// A on one pair): the finished slot holds the slot name the policy needs, so
// it is retired and the watcher creates a fresh one.
func TestMigration_ClusterScope_RetiresAFinishedSlotOfTheSamePolicy(t *testing.T) {
	const (
		clusterB = "bbbbbbbb-0000-0000-0000-000000000001"
		clusterC = "cccccccc-0000-0000-0000-000000000001"
		poolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
		poolC    = "cccccccc-0000-0000-0000-00000000000c"
		volC     = "44444444-4444-4444-4444-444444444444"
		volB     = "66666666-6666-6666-6666-666666666666"
	)
	siteB := testCluster("default", "site-b", clusterB)
	pair := readyPairForOps()
	pair.Name = testPairBName
	pair.Spec.SourceCluster = "site-b"
	pol := migrationPolicy()
	pol.Name = testMigPolicyBName
	pol.Spec.PairRef = testPairBName
	ops := migrationOps(utils.ReplicationOpsScopeCluster, "site-b")
	ops.Spec.PolicyRef = testMigPolicyBName
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-b-pvc-r", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: testMigPolicyBName, PVCRef: "pvc-r",
			VolumeID: clusterC + ":" + poolC + ":" + volC},
		Status: simplyblockv1alpha1.ReplicationSlotStatus{
			State: string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone),
		},
	}
	r, cl := newOpsReplReconciler(t, siteB, pair, pol, ops, slot,
		migBoundPVC("pvc-r", "pv-r", map[string]string{utils.AnnotationReplicationPolicy: testMigPolicyBName}),
		csiPV("pv-r", clusterC+":"+poolC+":"+volC),
	)
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v2/clusters/" + clusterC + "/replication/relationships/" + volC:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"is_source": true, "target_cluster_id": clusterB, "target_pool_id": poolB,
				"target_lvol_id": volB, "active_lvol_id": volB,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	reconcileOps(t, r)

	err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mig-b-pvc-r"},
		&simplyblockv1alpha1.ReplicationSlot{})
	if err == nil {
		t.Error("the finished slot is still there; the watcher cannot create the policy's new one")
	}
	if got := getOps(t, cl); got.Status.Subphase != replOpsSubphaseEnrolling {
		t.Errorf("subphase = %q, want %s", got.Status.Subphase, replOpsSubphaseEnrolling)
	}
}
