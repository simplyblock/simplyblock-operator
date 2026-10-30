package controller

// The trigger and snapshot actions of ReplicationOps: replicate each volume now,
// and take one consistency-group generation of a policy now.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// testOpsName is the ReplicationOps every test here reconciles.
const testOpsName = "ops1"

func opsFor(action, scope, ref string) *simplyblockv1alpha1.ReplicationOps {
	return &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: testOpsName, Namespace: "default", UID: "ops1-uid",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{Action: action, Scope: scope, Ref: ref},
	}
}

// slotNamed is a slot of policy "pol" named name, for volumeID.
func slotNamed(name, volumeID string) *simplyblockv1alpha1.ReplicationSlot {
	s := slotForPolicy()
	s.Name, s.Spec.VolumeID = name, volumeID
	return s
}

// callLog records the requests a fake control plane received.
type callLog struct {
	mu    sync.Mutex
	calls []string
	body  map[string]any
}

func (l *callLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, r.Method+" "+r.URL.Path)
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &l.body)
	}
}

func (l *callLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func triggerPath(volume string) string {
	return "POST /api/v2/clusters/" + testClusterUUID + "/storage-pools/pool-u/volumes/" + volume + "/replication/trigger"
}

func wantPhase(t *testing.T, cl client.Client, phase simplyblockv1alpha1.ReplicationOpsPhase) *simplyblockv1alpha1.ReplicationOps {
	t.Helper()
	got := getOps(t, cl)
	if got.Status.Phase != string(phase) {
		t.Fatalf("phase = %q (%s), want %s", got.Status.Phase, got.Status.Message, phase)
	}
	return got
}

// ---------- trigger ----------

// A trigger replicates every volume of its scope now: one trigger call per
// volume, on the pair's source cluster, and a succeeded result for each.
func TestOps_Trigger_ReplicatesEveryVolumeOfTheScope(t *testing.T) {
	for _, tc := range []struct {
		scope, ref string
		volumes    []string
	}{
		{utils.ReplicationOpsScopePolicy, "pol", []string{"vol-1", "vol-2"}},
		{utils.ReplicationOpsScopeVolume, "pol-pvc1", []string{"vol-1"}},
		{utils.ReplicationOpsScopeTarget, "pair1", []string{"vol-1", "vol-2"}},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"),
				slotNamed("pol-pvc1", "cluster-u:pool-u:vol-1"),
				slotNamed("pol-pvc2", "cluster-u:pool-u:vol-2"),
				opsFor("trigger", tc.scope, tc.ref))
			log := &callLog{}
			srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
				log.add(req)
				w.WriteHeader(http.StatusNoContent)
			})
			t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

			if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
				t.Fatal(err)
			}

			got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)
			for _, v := range tc.volumes {
				if n := log.count(triggerPath(v)); n != 1 {
					t.Errorf("%s triggered %d times, want 1 (calls %v)", v, n, log.calls)
				}
			}
			if len(got.Status.Results) != len(tc.volumes) {
				t.Errorf("results = %+v, want %d", got.Status.Results, len(tc.volumes))
			}
			for _, res := range got.Status.Results {
				if res.Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) {
					t.Errorf("result %+v, want succeeded", res)
				}
			}
		})
	}
}

// A trigger takes a new snapshot per call, so a reconcile that resumes after a
// restart does not trigger again a volume its status already records.
func TestOps_Trigger_DoesNotRetriggerARecordedVolume(t *testing.T) {
	ops := opsFor("trigger", utils.ReplicationOpsScopePolicy, "pol")
	ops.Status.Phase = string(simplyblockv1alpha1.ReplicationOpsPhaseRunning)
	ops.Status.Results = []simplyblockv1alpha1.ReplicationOpsResult{
		{SlotRef: "pol-pvc1", Status: string(simplyblockv1alpha1.ReplicationOpsResultSucceeded)},
	}
	pol := readyPolicyForOps("pol")
	pol.Status.ActiveOpsRef = testOpsName
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol,
		slotNamed("pol-pvc1", "cluster-u:pool-u:vol-1"),
		slotNamed("pol-pvc2", "cluster-u:pool-u:vol-2"), ops)
	log := &callLog{}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		log.add(req)
		w.WriteHeader(http.StatusNoContent)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatal(err)
	}

	got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)
	if n := log.count(triggerPath("vol-1")); n != 0 {
		t.Errorf("vol-1 was triggered again (%d calls)", n)
	}
	if n := log.count(triggerPath("vol-2")); n != 1 {
		t.Errorf("vol-2 triggered %d times, want 1", n)
	}
	if len(got.Status.Results) != 2 {
		t.Errorf("results = %+v, want both volumes", got.Status.Results)
	}
}

// A volume the control plane refuses fails the operation, and its result says so.
func TestOps_Trigger_RefusedVolumeFailsTheOperation(t *testing.T) {
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"),
		slotNamed("pol-pvc1", "cluster-u:pool-u:vol-1"),
		opsFor("trigger", utils.ReplicationOpsScopePolicy, "pol"))
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatal(err)
	}

	got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseFailed)
	if len(got.Status.Results) != 1 || got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultFailed) {
		t.Errorf("results = %+v, want one failed", got.Status.Results)
	}
}

// ---------- snapshot ----------

const groupsBody = `[{"id":"11111111-1111-1111-1111-111111111111","cluster_id":"` + testClusterUUID +
	`","name":"other","policy_id":"22222222-2222-2222-2222-222222222222","member_count":2,"last_group_seq":3},` +
	`{"id":"33333333-3333-3333-3333-333333333333","cluster_id":"` + testClusterUUID +
	`","name":"pol-group","policy_id":"bpol-uuid","member_count":2,"last_group_seq":6}]`

const groupPath = "/api/v2/clusters/" + testClusterUUID + "/consistency-groups/"

// groupPlane serves the cluster's consistency groups and takes generation 7 of
// the policy's group.
func groupPlane(t *testing.T, log *callLog) {
	t.Helper()
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		log.add(req)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == groupPath:
			_, _ = w.Write([]byte(groupsBody))
		case req.Method == http.MethodPost &&
			req.URL.Path == groupPath+"33333333-3333-3333-3333-333333333333/snapshots":
			_, _ = w.Write([]byte(`{"group_seq":7,"created_at":0,"expected":2,"present":2,"complete":true,"members":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)
}

// A snapshot takes one generation of the policy's consistency group, found by
// the policy's backend id, keyed by the operation so a retry takes no second
// one, and records the generation.
func TestOps_Snapshot_TakesOneGenerationOfThePolicysGroup(t *testing.T) {
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"),
		opsFor("snapshot", utils.ReplicationOpsScopePolicy, "pol"))
	log := &callLog{}
	groupPlane(t, log)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatal(err)
	}

	got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)
	if n := log.count("POST " + groupPath); n != 1 {
		t.Errorf("generation takes = %d, want 1 (calls %v)", n, log.calls)
	}
	if log.body["request_id"] != "ops1-uid" {
		t.Errorf("take body = %v, want request_id ops1-uid", log.body)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].PolicyRef != "pol" ||
		got.Status.Results[0].Generation != 7 ||
		got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) {
		t.Errorf("results = %+v, want policy pol at generation 7", got.Status.Results)
	}
}

// Scope target snapshots every policy of the pair that has a group, and skips
// one that has none rather than failing.
func TestOps_Snapshot_TargetSkipsAPolicyWithoutAGroup(t *testing.T) {
	plain := readyPolicyForOps("plain")
	plain.Status.BackendPolicyID = "no-group-policy"
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"), plain,
		opsFor("snapshot", utils.ReplicationOpsScopeTarget, "pair1"))
	log := &callLog{}
	groupPlane(t, log)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatal(err)
	}

	got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)
	statuses := map[string]string{}
	for _, res := range got.Status.Results {
		statuses[res.PolicyRef] = res.Status
	}
	if statuses["pol"] != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) ||
		statuses["plain"] != string(simplyblockv1alpha1.ReplicationOpsResultSkipped) {
		t.Errorf("results = %+v, want pol succeeded and plain skipped", got.Status.Results)
	}
}

// A snapshot of a policy with no consistency group fails and says why: there is
// nothing to take a generation of.
func TestOps_Snapshot_PolicyWithoutAGroupFails(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pol.Status.BackendPolicyID = "no-group-policy"
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol,
		opsFor("snapshot", utils.ReplicationOpsScopePolicy, "pol"))
	log := &callLog{}
	groupPlane(t, log)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatal(err)
	}

	got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseFailed)
	if !strings.Contains(got.Status.Message, "consistency group") {
		t.Errorf("message %q, want it to name the missing consistency group", got.Status.Message)
	}
	if n := log.count("POST "); n != 0 {
		t.Errorf("a generation was taken (%v)", log.calls)
	}
}

// A snapshot is a policy's, so scope volume is refused. Admission refuses it too,
// and this is the reconciler's own guard for an object that predates the rule.
func TestOps_Snapshot_ScopeVolumeFails(t *testing.T) {
	r, cl := newOpsReplReconciler(t, readyPairForOps(), readyPolicyForOps("pol"),
		slotNamed("pol-pvc1", "cluster-u:pool-u:vol-1"),
		opsFor("snapshot", utils.ReplicationOpsScopeVolume, "pol-pvc1"))
	log := &callLog{}
	groupPlane(t, log)

	if _, err := r.Reconcile(context.Background(), opsRequest(testOpsName)); err != nil {
		t.Fatal(err)
	}

	got := wantPhase(t, cl, simplyblockv1alpha1.ReplicationOpsPhaseFailed)
	if !strings.Contains(got.Status.Message, "scope policy or target") {
		t.Errorf("message %q, want the scope refusal", got.Status.Message)
	}
	if n := log.count("POST "); n != 0 {
		t.Errorf("a generation was taken (%v)", log.calls)
	}
}
