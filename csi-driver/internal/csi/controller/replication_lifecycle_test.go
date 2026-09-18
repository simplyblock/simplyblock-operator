package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPromoteVolumeForced(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mock.lastFailoverQuery, "planned=true") {
		t.Errorf("query = %q, forced promote must not ask for the planned gate", mock.lastFailoverQuery)
	}
}

func TestPromoteVolumePlannedSendsThePlannedFlag(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mock.lastFailoverQuery, "planned=true") {
		t.Errorf("query = %q, want planned=true", mock.lastFailoverQuery)
	}
}

// The whole point of the planned gate: a demote still converging must map to
// ABORTED (retryable), never FAILED_PRECONDITION -- the vendored csi-addons
// controller auto-escalates ANY FAILED_PRECONDITION from a force=false
// promote to force=true inline, in the same reconcile, with no
// wait-and-retry grace period of its own. Mapping this to FailedPrecondition
// would silently force through a promote while demote is still converging.
func TestPromoteVolumePlannedWhileDemoteConvergingIsAborted(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.failoverStatus = http.StatusConflict

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: false,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.Aborted {
		t.Errorf("code = %v, want Aborted (retryable, never auto-forced by the vendored controller)", st.Code())
	}
}

// The case that SHOULD let the vendored controller's own force-escalation
// take over: no demote was ever requested, so this planned attempt only
// makes sense as a genuinely unplanned failover the caller mislabeled.
func TestPromoteVolumePlannedWithNoDemoteIsFailedPrecondition(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.failoverStatus = http.StatusPreconditionFailed

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: false,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

// The vendored csi-addons controller has no "already primary" awareness of
// its own (markVolumeAsPrimary calls Promote unconditionally, every time a
// VolumeReplication first declares primary intent -- including day-one
// protection of a volume that has always lived at this cluster, never
// failed over). So the driver must supply that check itself: a volume
// already reporting role=source has nothing to fail over, and calling the
// backend's failover would wrongly clone+retire against a healthy source --
// confirmed against a live cluster, where protecting a brand-new PVC
// (VolumeReplication created directly as primary) triggered a real target-
// cluster clone with no failover ever intended.
func TestPromoteVolumeAlreadySourceIsNoOp(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "source", "state": "in_sync",
		"outstanding_count": 0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": false,
	}

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mock.lastFailoverQuery != "" {
		t.Errorf("failover query = %q, want no failover call: the volume is already the live source", mock.lastFailoverQuery)
	}
}

// A repeat promote after an earlier one already succeeded is the same
// no-op: nothing to fail over a second time.
func TestPromoteVolumeAlreadyFailedOverIsNoOp(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "failed_over", "state": "in_sync",
		"outstanding_count": 0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": false,
	}

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mock.lastFailoverQuery != "" {
		t.Errorf("failover query = %q, want no failover call: already promoted", mock.lastFailoverQuery)
	}
}

// A volume genuinely holding the replica side (role=secondary) is the real
// promotion case, and must still reach the backend's failover exactly as
// before.
func TestPromoteVolumeSecondaryProceedsToFailover(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "secondary", "state": "in_sync",
		"outstanding_count": 0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": false,
	}

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mock.lastFailoverQuery == "" {
		t.Error("want a failover call: the volume genuinely holds the replica side")
	}
}

// Same real-world shape as TestEnableVolumeReplicationUsesReplicationSourceWhenVolumeIdIsEmpty:
// the vendored controller-manager/sidecar chain sends every Replication RPC,
// Promote included, via ReplicationSource with the legacy flat VolumeId left
// empty.
func TestPromoteVolumeUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: replicationSourceFor(testReplVolID), Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDemoteVolumeDone(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Non-blocking and re-driven: a still-converging demote must not hang the
// RPC or report success, it must fail with a retryable code so the
// controller-manager's own reconcile loop re-invokes DemoteVolume later --
// matching the actual upstream reconciler's requeue-until-ready behavior.
func TestDemoteVolumeNotYetDoneIsAborted(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.demoteStatus = http.StatusAccepted

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.Aborted {
		t.Errorf("code = %v, want Aborted (retryable)", st.Code())
	}
}

func TestDemoteVolumeBackendFailureIsUnavailable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.demoteStatus = http.StatusInternalServerError

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID,
	})
	if err == nil {
		t.Fatal("want an error on a genuine backend failure")
	}
}

func TestDemoteVolumeUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		ReplicationSource: replicationSourceFor(testReplVolID),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResyncVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResyncVolumeUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		ReplicationSource: replicationSourceFor(testReplVolID),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResyncVolumeForwardsTheSourceClusterParameter(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		VolumeId:   testReplVolID,
		Parameters: map[string]string{sourceClusterIDParam: sanityClusterID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mock.lastFailbackBody), `"source_cluster_id":"`+sanityClusterID+`"`) {
		t.Errorf("failback body = %q, want source_cluster_id %s", mock.lastFailbackBody, sanityClusterID)
	}
}

func TestResyncVolumeReadyReflectsLag(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "source", "state": "in_sync",
		"lag_seconds": 120, "lag_budget_seconds": 900,
		"outstanding_count": 0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": true,
	}

	resp, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ready {
		t.Error("Ready = false, want true: lag (120s) is inside the budget (900s)")
	}
}

func TestResyncVolumeNotReadyWhileLagExceedsBudget(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "source", "state": "in_sync",
		"lag_seconds": 1800, "lag_budget_seconds": 900,
		"outstanding_count": 0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": true,
	}

	resp, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ready {
		t.Error("Ready = true, want false: lag (1800s) exceeds the budget (900s)")
	}
}

func TestResyncVolumeBackendFailureIsUnavailable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.failbackStatus = http.StatusInternalServerError

	_, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		VolumeId: testReplVolID,
	})
	if err == nil {
		t.Fatal("want an error on a genuine backend failure")
	}
}
