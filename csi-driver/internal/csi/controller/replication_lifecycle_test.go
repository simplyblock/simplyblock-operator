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

// A Ramen-restored destination PV inherits the ORIGINAL source's own
// volumeHandle verbatim (confirmed live 2026-09-23, relocate M-02): Ramen's
// S3-restore recreates the exact PV/PVC object it archived at protect time,
// including the source cluster+lvol identity, on a cluster that never
// provisioned that volume at all. Every Replication RPC parses its target
// straight from the given handle, so without resolving through the backend's
// own source->target relationship first, "promote" would be asking the
// ORIGINAL, foreign volume to fail over -- not the local replica that has
// actually been receiving replicated data. PromoteVolume must resolve a
// handle whose relationship says IsSource and redirect to TargetLvolId
// (on TargetClusterId/TargetPoolId) before calling failover.
func TestPromoteVolumeResolvesToTargetWhenGivenTheSourceSideOfARelationship(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.volumes[testReplTargetVolumeID] = &mockVolume{
		UUID: testReplTargetVolumeID, Name: "repl-vol-target", Size: 1 << 30,
	}
	mock.replicationRelationship[testReplVolumeID] = map[string]any{
		"replication_id":    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"direction":         "to_target",
		"mode":              "migration",
		"state":             "replicating",
		"is_source":         true,
		"source_cluster_id": sanityClusterID, "source_lvol_id": testReplVolumeID,
		"target_cluster_id": sanityClusterID, "target_pool_id": sanityPoolUUID, "target_lvol_id": testReplTargetVolumeID,
		"target_nqn": "nqn.test", "target_ns_id": 1,
	}

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mock.lastFailoverVolumeID != testReplTargetVolumeID {
		t.Errorf("failover landed on volume %q, want the resolved target %q",
			mock.lastFailoverVolumeID, testReplTargetVolumeID)
	}
}

// A volume already naming the target side of its own relationship (IsSource
// false) is promoted directly, unchanged -- resolving again would be a
// harmless no-op, but this proves it takes that path rather than one that
// happens to work only by coincidence.
func TestPromoteVolumeAlreadyNamingTheTargetIsUnchanged(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationRelationship[testReplVolumeID] = map[string]any{
		"replication_id":    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"direction":         "to_target",
		"mode":              "migration",
		"state":             "replicating",
		"is_source":         false,
		"source_cluster_id": sanityClusterID, "source_lvol_id": "99999999-9999-9999-9999-999999999998",
		"target_cluster_id": sanityClusterID, "target_pool_id": sanityPoolUUID, "target_lvol_id": testReplVolumeID,
		"target_nqn": "nqn.test", "target_ns_id": 1,
	}

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mock.lastFailoverVolumeID != testReplVolumeID {
		t.Errorf("failover landed on volume %q, want %q unchanged", mock.lastFailoverVolumeID, testReplVolumeID)
	}
}

// The ordinary case, and by far the most common: a volume never enabled for
// replication (M-01's own first-ever protect) has no relationship at all yet.
// This must promote the given handle directly rather than fail the whole
// call over a 404 that just means "nothing to resolve."
func TestPromoteVolumeWithNoRelationshipYetUsesTheGivenHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mock.lastFailoverVolumeID != testReplVolumeID {
		t.Errorf("failover landed on volume %q, want %q", mock.lastFailoverVolumeID, testReplVolumeID)
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

// Same real-world shape as TestEnableVolumeReplicationUsesReplicationSourceWhenVolumeIdIsEmpty:
// the vendored controller-manager/sidecar chain sends every Replication RPC,
// Promote included, via ReplicationSource with the legacy flat VolumeId left
// empty.
func TestPromoteVolumeUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: replicationSourceFor(), Force: true,
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
		ReplicationSource: replicationSourceFor(),
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
		ReplicationSource: replicationSourceFor(),
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
