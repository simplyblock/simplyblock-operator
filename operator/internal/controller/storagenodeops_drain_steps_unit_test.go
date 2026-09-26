// The two drain steps that were previously invisible inside the node delete.
//
// Both are latch-then-poll: one POST starts the work, subsequent passes GET its
// progress. The tests cover what that shape can get wrong -- starting twice,
// advancing before the work is done, and walking past a failure -- because each
// of those ends with a node deleted while something still depended on it.

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// drainStepServer answers the step endpoints: POST always succeeds, GET returns
// the given progress body. postCount records how many times the work was started.
func drainStepServer(t *testing.T, progress string, postCount *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if postCount != nil {
				*postCount++
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(progress))
	}))
}

func drainStepFixtures(t *testing.T, subPhase simplyblockv1alpha1.StorageNodeOpsSubPhase) (
	*StorageNodeOpsReconciler, *simplyblockv1alpha1.StorageNodeOps, *simplyblockv1alpha1.StorageNode,
) {
	t.Helper()
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.Ports = &simplyblockv1alpha1.StorageNodePorts{Management: "10.0.0.2"}
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", utils.NodeActionRemove)
	ops.Status.SubPhase = subPhase
	return newOpsReconciler(t, sn, ops), ops, sn
}

func reloadOps(t *testing.T, r *StorageNodeOpsReconciler) *simplyblockv1alpha1.StorageNodeOps {
	t.Helper()
	var updated simplyblockv1alpha1.StorageNodeOps
	if err := r.Get(context.Background(),
		types.NamespacedName{Name: opsTestOpsName, Namespace: opsTestNS}, &updated); err != nil {
		t.Fatalf("failed to fetch ops: %v", err)
	}
	return &updated
}

// The device rebuild is started once. Without the latch every pass re-POSTs, and
// a step polled every 20s would restart the work it is waiting for.
func TestDrainMigrateDevices_StartsTheRebuildOnlyOnce(t *testing.T) {
	posts := 0
	srv := drainStepServer(t, `{"done":false,"total":4,"completed":1}`, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	client := webapi.NewClient(srv.URL)

	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid", client); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if posts != 1 {
		t.Fatalf("first pass sent %d POSTs, want 1", posts)
	}
	if !reloadOps(t, r).Status.DevicesTriggered {
		t.Fatal("the start was not latched, so every later pass would restart the rebuild")
	}

	ops = reloadOps(t, r)
	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid", client); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if posts != 1 {
		t.Errorf("the rebuild was started %d times; a latched step must start it once", posts)
	}
}

// Progress is published while the rebuild runs. This is the phase's whole
// purpose: the volumes on this node are served from replicas meanwhile, which is
// far slower, and without a count that slowdown has nothing explaining it.
func TestDrainMigrateDevices_PublishesProgressWhileItWaits(t *testing.T) {
	srv := drainStepServer(t, `{"done":false,"total":4,"completed":3}`, nil)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	ops.Status.DevicesTriggered = true

	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid",
		webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainMigrateDevices: %v", err)
	}

	updated := reloadOps(t, r)
	if updated.Status.DevicesMigrated != 3 || updated.Status.DevicesTotal != 4 {
		t.Errorf("progress: got %d of %d, want 3 of 4",
			updated.Status.DevicesMigrated, updated.Status.DevicesTotal)
	}
	if updated.Status.SubPhase == simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating {
		t.Error("advanced to the volume drain while devices were still rebuilding")
	}
}

// Done advances to the volume drain, and only then.
func TestDrainMigrateDevices_AdvancesOnlyWhenTheRebuildIsDone(t *testing.T) {
	srv := drainStepServer(t, `{"done":true,"total":4,"completed":4}`, nil)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	ops.Status.DevicesTriggered = true

	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid",
		webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainMigrateDevices: %v", err)
	}

	if got := reloadOps(t, r).Status.SubPhase; got != simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating {
		t.Errorf("subPhase: got %q, want Migrating once the rebuild finished", got)
	}
}

// A device that could not be rebuilt stops the drain. Draining volumes on top of
// a failed rebuild moves them while their data is one replica short, which
// widens the exposure the removal was supposed to end.
func TestDrainMigrateDevices_AFailedDeviceStopsTheDrain(t *testing.T) {
	srv := drainStepServer(t, `{"done":false,"total":4,"completed":2,"failed":1,"message":"nvme3 rebuild failed"}`, nil)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseMigratingDevices)
	ops.Status.DevicesTriggered = true

	if _, err := r.drainMigrateDevices(context.Background(), ops, sn, "cluster-uuid",
		webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainMigrateDevices: %v", err)
	}

	updated := reloadOps(t, r)
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q, want Failed after a device rebuild failure", updated.Status.Phase)
	}
	if updated.Status.SubPhase == simplyblockv1alpha1.StorageNodeOpsSubPhaseMigrating {
		t.Error("drained volumes past a failed device rebuild")
	}
}

// Replica reallocation is no longer a drain phase: it is phase 3b of the
// control plane's removal, and it depends on phase 3a having freed the
// departing node's own replica slots first. Calling it on its own skipped 3a,
// so on a cluster with every replica slot occupied it had nowhere to move to
// and refused on a cycle -- retrying for four hours. Verifying therefore hands
// straight to the delete, which runs both phases in the order 3b requires.
func TestDrainVerify_AdvancesStraightToRemoving(t *testing.T) {
	// No pools, so the node holds no volume: verification passes and the drain
	// is finished with this node's data.
	srv := drainStepServer(t, `[]`, nil)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseVerifying)

	if _, err := r.drainVerify(context.Background(), ops, sn, "cluster-uuid",
		webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainVerify: %v", err)
	}

	got := reloadOps(t, r).Status.SubPhase
	if got == simplyblockv1alpha1.StorageNodeOpsSubPhaseReshuffling {
		t.Fatal("Verifying still routes through Reshuffling, which runs phase 3b without 3a")
	}
	if got != simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving {
		t.Errorf("subPhase: got %q, want Removing", got)
	}
}

// An op already sitting in Reshuffling when the operator is upgraded must be
// able to leave it. Failing instead would strand a node that is shut down with
// its volumes already moved -- exactly the state the stuck drain was left in.
func TestDrainReshuffle_LegacyPhaseAdvancesInsteadOfStranding(t *testing.T) {
	srv := drainStepServer(t, `{"done":false,"total":2,"completed":0}`, nil)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseReshuffling)
	ops.Status.ReshuffleTriggered = true

	if _, err := r.runDrain(context.Background(), ops, sn, "cluster-uuid",
		webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	updated := reloadOps(t, r)
	if updated.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Error("a legacy Reshuffling op was failed rather than carried forward")
	}
	if updated.Status.SubPhase != simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving {
		t.Errorf("subPhase: got %q, want Removing", updated.Status.SubPhase)
	}
}

// removeStepServer answers the delete step: DELETE on the node is accepted (or
// refused with deleteStatus), GET on the node reports *nodeStatus, and every
// POST (a resume) is counted so a test can assert none was attempted.
func removeStepServer(t *testing.T, nodeStatus *string, deleteStatus int, deletes, posts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			*deletes++
			w.WriteHeader(deleteStatus)
		case http.MethodPost:
			*posts++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + opsTestNodeUUID + `","status":"` + *nodeStatus + `"}`))
		}
	}))
}

// The DELETE queues the removal; the node is removed only when the control
// plane says so. Succeeding on the 204 reported a node removed while it was
// still at the first step of its removal, and hid one that never finished.
func TestDrainRemove_SendsTheDeleteOnceAndWaitsForTheNode(t *testing.T) {
	status := nodeStatusMigratingLvols // what the drain hands over; also what a running removal reads as
	deletes, posts := 0, 0
	srv := removeStepServer(t, &status, http.StatusNoContent, &deletes, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving)
	client := webapi.NewClient(srv.URL)

	if _, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", client); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	ops = reloadOps(t, r)
	if !ops.Status.RemoveTriggered {
		t.Fatal("the DELETE was not latched")
	}
	if ops.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded {
		t.Fatal("succeeded on the 204, before the node was removed")
	}

	status = nodeStatusInRemoval
	if _, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", client); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	ops = reloadOps(t, r)
	if ops.Status.Phase == simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded {
		t.Fatal("succeeded while the node was still in_removal")
	}
	if deletes != 1 {
		t.Errorf("the DELETE was sent %d times, want 1", deletes)
	}

	status = utils.NodeStatusRemoved
	if _, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", client); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	final := reloadOps(t, r)
	if final.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded {
		t.Errorf("phase: got %q, want Succeeded once the node reads removed", final.Status.Phase)
	}
	if final.Status.Message != "" {
		t.Errorf("a Succeeded op still carries progress commentary: %q", final.Status.Message)
	}
	if deletes != 1 {
		t.Errorf("the DELETE was sent %d times, want 1", deletes)
	}
}

// A removal the control plane gave up on ends the op, and does not try to
// resume a node the drain has already dismantled.
func TestDrainRemove_RemovedFailedFailsTheOpWithoutResuming(t *testing.T) {
	status := nodeStatusRemovedFailed
	deletes, posts := 0, 0
	srv := removeStepServer(t, &status, http.StatusNoContent, &deletes, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving)
	ops.Status.RemoveTriggered = true

	if _, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainRemove: %v", err)
	}
	if got := reloadOps(t, r).Status.Phase; got != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q, want Failed", got)
	}
	if posts != 0 {
		t.Errorf("attempted %d resume(s) of a node that is already dismantled", posts)
	}
}

// A DELETE refused for a node the drain has already stopped fails the op
// outright. Resuming such a node fails too (its SPDK is gone), and retrying
// that resume forever is how a plain 400 became an op that never ended.
func TestDrainRemove_RefusedDeleteOnAStoppedNodeFailsWithoutResuming(t *testing.T) {
	status := nodeStatusMigratingLvols
	deletes, posts := 0, 0
	srv := removeStepServer(t, &status, http.StatusBadRequest, &deletes, &posts)
	defer srv.Close()

	r, ops, sn := drainStepFixtures(t, simplyblockv1alpha1.StorageNodeOpsSubPhaseRemoving)

	if _, err := r.drainRemove(context.Background(), ops, sn, "cluster-uuid", webapi.NewClient(srv.URL)); err != nil {
		t.Fatalf("drainRemove: %v", err)
	}
	updated := reloadOps(t, r)
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q, want Failed", updated.Status.Phase)
	}
	if posts != 0 {
		t.Errorf("attempted %d resume(s) of a stopped node", posts)
	}
	if updated.Status.RemoveTriggered {
		t.Error("a refused DELETE was latched as sent")
	}
}
