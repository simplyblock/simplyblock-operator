// Tests that GetVolumeReplicationInfo reports the control plane's replication
// state as the csi-addons status, which csi-addons turns into the VR and VGR
// Replicating condition and Ramen copies onto the VRG as ReplicationHealthy.
package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
)

func volumeInfoFor(t *testing.T, body map[string]any) *replication.GetVolumeReplicationInfoResponse {
	t.Helper()
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	if body != nil {
		mock.replicationStatus[testReplVolumeID] = body
	}
	resp, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		VolumeId: testReplVolID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func statusBody(state string) map[string]any {
	return map[string]any{
		"role": "source", "state": state,
		"outstanding_count": 0, "outstanding_bytes": 0,
		"failing_count": 0, "max_retry_reached": false, "resyncing": false,
	}
}

func TestVolumeReplicationInfoReportsTheControlPlaneState(t *testing.T) {
	cases := map[string]replication.GetVolumeReplicationInfoResponse_Status{
		"in_sync":         replication.GetVolumeReplicationInfoResponse_HEALTHY,
		"replicating":     replication.GetVolumeReplicationInfoResponse_HEALTHY,
		"lagging":         replication.GetVolumeReplicationInfoResponse_DEGRADED,
		"degraded":        replication.GetVolumeReplicationInfoResponse_DEGRADED,
		"not_replicating": replication.GetVolumeReplicationInfoResponse_DEGRADED,
		"error":           replication.GetVolumeReplicationInfoResponse_ERROR,
		"some_new_state":  replication.GetVolumeReplicationInfoResponse_UNKNOWN,
	}
	for state, want := range cases {
		t.Run(state, func(t *testing.T) {
			resp := volumeInfoFor(t, statusBody(state))
			if resp.GetStatus() != want {
				t.Errorf("Status = %v, want %v", resp.GetStatus(), want)
			}
			if !strings.Contains(resp.GetStatusMessage(), state) {
				t.Errorf("StatusMessage = %q, want it to name the state %q", resp.GetStatusMessage(), state)
			}
		})
	}
}

func TestVolumeReplicationInfoReportsTheLastCycle(t *testing.T) {
	body := statusBody("in_sync")
	body["last_replicated_at"] = "2026-09-17T12:00:00Z"
	body["last_cycle_seconds"] = 113
	body["last_cycle_bytes"] = 4096
	resp := volumeInfoFor(t, body)
	if d := resp.GetLastSyncDuration(); d == nil || d.AsDuration().Seconds() != 113 {
		t.Errorf("LastSyncDuration = %v, want 113s", d)
	}
	if resp.GetLastSyncBytes() != 4096 {
		t.Errorf("LastSyncBytes = %d, want 4096", resp.GetLastSyncBytes())
	}
}

func TestGroupReplicationInfoReportsTheControlPlaneState(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)
	mock.groups[vgGroupID].ReplState = "lagging"

	resp, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		ReplicationSource: groupSource(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != replication.GetVolumeReplicationInfoResponse_DEGRADED {
		t.Errorf("Status = %v, want DEGRADED", resp.GetStatus())
	}
}
