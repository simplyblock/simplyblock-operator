package controller

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testReplVolumeID = "88888888-8888-8888-8888-888888888888"
	testReplVolID    = sanityClusterID + ":" + sanityPoolUUID + ":" + testReplVolumeID
	testReplPolicyID = "77777777-7777-7777-7777-777777777777"
)

func newReplicationTestServer(t *testing.T, mock *mockSBCLI) *Server {
	t.Helper()
	mock.volumes[testReplVolumeID] = &mockVolume{UUID: testReplVolumeID, Name: "repl-vol", Size: 1 << 30}
	return newTestControllerServer(t, mock)
}

// replicationSourceFor builds the ReplicationSource the real
// kubernetes-csi-addons v0.15.0 sidecar sends on every Replication RPC
// instead of the legacy flat VolumeId field (internal/sidecar/service's
// ReplicationServer proxy never sets it).
func replicationSourceFor() *replication.ReplicationSource {
	return &replication.ReplicationSource{
		Type: &replication.ReplicationSource_Volume{
			Volume: &replication.ReplicationSource_VolumeSource{VolumeId: testReplVolID},
		},
	}
}

func TestEnableVolumeReplication(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		VolumeId:   testReplVolID,
		Parameters: map[string]string{replicationPolicyParam: testReplPolicyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mock.volumes[testReplVolumeID].ReplicationPolicyID; got != testReplPolicyID {
		t.Errorf("ReplicationPolicyID = %q, want %q", got, testReplPolicyID)
	}
}

// Repeating an enable already in effect is success without a second
// meaningful change (P0-2's own idempotency; the mock does not distinguish,
// it just re-applies the same value).
func TestEnableVolumeReplicationRepeatedIsIdempotent(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	req := &replication.EnableVolumeReplicationRequest{
		VolumeId:   testReplVolID,
		Parameters: map[string]string{replicationPolicyParam: testReplPolicyID},
	}
	if _, err := cs.EnableVolumeReplication(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.EnableVolumeReplication(context.Background(), req); err != nil {
		t.Errorf("second EnableVolumeReplication = %v, want nil (idempotent)", err)
	}
}

// The real controller-manager/sidecar chain (v0.15.0) sends the volume
// identity via ReplicationSource, leaving the legacy flat VolumeId field
// empty -- confirmed against a live cluster, where every Replication RPC
// failed with "invalid volume handle \"\"" despite the controller-manager's
// own log showing it resolved a correct handle.
func TestEnableVolumeReplicationUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		ReplicationSource: replicationSourceFor(),
		Parameters:        map[string]string{replicationPolicyParam: testReplPolicyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mock.volumes[testReplVolumeID].ReplicationPolicyID; got != testReplPolicyID {
		t.Errorf("ReplicationPolicyID = %q, want %q", got, testReplPolicyID)
	}
}

func TestEnableVolumeReplicationMissingPolicyParam(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		VolumeId: testReplVolID,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestEnableVolumeReplicationBackendRefusal(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationPUTStatus = 412

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		VolumeId:   testReplVolID,
		Parameters: map[string]string{replicationPolicyParam: testReplPolicyID},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

func TestEnableVolumeReplicationMalformedVolumeHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		VolumeId:   "not-a-valid-handle",
		Parameters: map[string]string{replicationPolicyParam: testReplPolicyID},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// A cluster this deployment's secret.json has no entry for is unreachable in
// the same sense a network partition would be: there is no client to make the
// call with, so the RPC must not be confused with a backend-side refusal.
func TestEnableVolumeReplicationUnknownCluster(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	unregisteredClusterID := "99999999-9999-9999-9999-999999999999"
	unknownClusterVolID := unregisteredClusterID + ":" + sanityPoolUUID + ":" + testReplVolumeID

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		VolumeId:   unknownClusterVolID,
		Parameters: map[string]string{replicationPolicyParam: testReplPolicyID},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

func TestDisableVolumeReplication(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.volumes[testReplVolumeID].ReplicationPolicyID = testReplPolicyID

	_, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mock.volumes[testReplVolumeID].ReplicationPolicyID; got != "" {
		t.Errorf("ReplicationPolicyID = %q, want cleared", got)
	}
}

// Disabling a volume that follows no policy is success, matching the
// backend's own idempotency (P0-2): there is nothing to detach.
func TestDisableVolumeReplicationNotAttachedIsSuccess(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	_, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Errorf("DisableVolumeReplication = %v, want nil", err)
	}
}

func TestDisableVolumeReplicationUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.volumes[testReplVolumeID].ReplicationPolicyID = testReplPolicyID

	_, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		ReplicationSource: replicationSourceFor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mock.volumes[testReplVolumeID].ReplicationPolicyID; got != "" {
		t.Errorf("ReplicationPolicyID = %q, want cleared", got)
	}
}

func TestDisableVolumeReplicationDuringCutoverIsAborted(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationPUTStatus = 409

	_, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		VolumeId: testReplVolID,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.Aborted {
		t.Errorf("code = %v, want Aborted (retryable)", st.Code())
	}
}

func TestGetVolumeReplicationInfo(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "source", "state": "in_sync",
		"last_replicated_at": "2026-09-17T12:00:00Z",
		"lag_seconds":        42,
		"outstanding_count":  0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": false,
	}

	resp, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.LastSyncTime == nil || resp.LastSyncTime.AsTime().Unix() != 1789646400 {
		t.Errorf("LastSyncTime = %v, want 2026-09-17T12:00:00Z", resp.LastSyncTime)
	}
}

// A volume that never replicated is a valid answer, never a 404: the mock's
// default GET .../replication/status body for a volume nothing configured.
func TestGetVolumeReplicationInfoNeverReplicated(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)

	resp, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.LastSyncTime != nil {
		t.Errorf("LastSyncTime = %v, want nil", resp.LastSyncTime)
	}
}

func TestGetVolumeReplicationInfoUsesReplicationSourceWhenVolumeIdIsEmpty(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	mock.replicationStatus[testReplVolumeID] = map[string]any{
		"role": "source", "state": "in_sync",
		"last_replicated_at": "2026-09-17T12:00:00Z",
		"lag_seconds":        42,
		"outstanding_count":  0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": false,
	}

	resp, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		ReplicationSource: replicationSourceFor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.LastSyncTime == nil || resp.LastSyncTime.AsTime().Unix() != 1789646400 {
		t.Errorf("LastSyncTime = %v, want 2026-09-17T12:00:00Z", resp.LastSyncTime)
	}
}

func TestGetVolumeReplicationInfoUnknownVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	delete(mock.volumes, testReplVolumeID)

	_, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		VolumeId: testReplVolID,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}
