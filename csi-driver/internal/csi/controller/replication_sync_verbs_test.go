// Driver-level tests for the sync-method dispatch of the replication verbs
// (design-sync-replication-csi-addons.md §6, §11; test plan U-05..U-20). A sync
// class routes Promote/Demote to the backend's site-keyed routes with the
// force->planned inversion and the 409-retry / 412-escalate protocol, and the
// Enable/Disable/Resync verbs are no-ops. Driven against the mock backend.
package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/lvol"
)

func syncParams(site string) map[string]string {
	p := map[string]string{methodParam: methodSync}
	if site != "" {
		p[siteParam] = site
	}
	return p
}

func TestSyncPromoteDispatch(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.failoverStatus = 200 // a sync promote succeeds with 200, not the async 204
	cs := newReplicationTestServer(t, mock)

	// Unforced promote -> planned=true (a planned switchover).
	if _, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: false, Parameters: syncParams("site-b"),
	}); err != nil {
		t.Fatalf("PromoteVolume: %v", err)
	}
	if !strings.Contains(mock.lastFailoverQuery, "site=site-b") ||
		!strings.Contains(mock.lastFailoverQuery, "planned=true") {
		t.Errorf("query = %q, want site-b and planned=true", mock.lastFailoverQuery)
	}

	// Forced promote -> planned=false (a disaster fail-over).
	if _, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true, Parameters: syncParams("site-b"),
	}); err != nil {
		t.Fatalf("forced PromoteVolume: %v", err)
	}
	if !strings.Contains(mock.lastFailoverQuery, "planned=false") {
		t.Errorf("forced query = %q, want planned=false", mock.lastFailoverQuery)
	}
}

func TestSyncPromoteStatusProtocol(t *testing.T) {
	cases := []struct {
		name     string
		backend  int
		wantCode codes.Code
	}{
		{"412 is the escalation trigger", 412, codes.FailedPrecondition},
		{"409 in progress is retryable, not escalation", 409, codes.Unavailable},
		{"400 bad site is invalid argument", 400, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockSBCLI()
			defer mock.Close()
			mock.failoverStatus = tc.backend
			cs := newReplicationTestServer(t, mock)

			_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
				VolumeId: testReplVolID, Force: false, Parameters: syncParams("site-b"),
			})
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.wantCode, err)
			}
		})
	}
}

func TestSyncPromoteMissingSite(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	// The volume's VolumeReplication carries no sync-site annotation: the resolver
	// returns an empty site, and the promote must refuse (FailedPrecondition)
	// rather than reach the backend without a site (design §5.2).
	cs.EnableSyncSiteResolution(func(context.Context, lvol.VolumeHandle) (string, error) {
		return "", nil
	})

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: map[string]string{methodParam: methodSync},
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
	if mock.lastFailoverQuery != "" {
		t.Errorf("a missing site must not reach the backend, query = %q", mock.lastFailoverQuery)
	}
}

func TestSyncDemoteDispatch(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock) // demote succeeds with the mock's default 204

	if _, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: syncParams("site-a"),
	}); err != nil {
		t.Fatalf("DemoteVolume: %v", err)
	}
	if mock.lastDemoteVolumeID != testReplVolumeID {
		t.Errorf("demote landed on %q, want %q", mock.lastDemoteVolumeID, testReplVolumeID)
	}
}

func TestSyncDemoteGateRefusalIsRetryable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.demoteStatus = 409
	cs := newReplicationTestServer(t, mock)

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: syncParams("site-a"),
	})
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (retryable, never escalates)", got)
	}
}

func TestSyncNoOpVerbs(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	ctx := context.Background()
	p := syncParams("site-a")

	if _, err := cs.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
		VolumeId: testReplVolID, Parameters: p,
	}); err != nil {
		t.Errorf("EnableVolumeReplication: %v", err)
	}
	if _, err := cs.DisableVolumeReplication(ctx, &replication.DisableVolumeReplicationRequest{
		VolumeId: testReplVolID, Parameters: p,
	}); err != nil {
		t.Errorf("DisableVolumeReplication: %v", err)
	}
	if _, err := cs.ResyncVolume(ctx, &replication.ResyncVolumeRequest{
		VolumeId: testReplVolID, Parameters: p,
	}); err != nil {
		t.Errorf("ResyncVolume: %v", err)
	}
	// No-op means no backend call: Enable never attached a policy.
	if got := mock.volumes[testReplVolumeID].ReplicationPolicyID; got != "" {
		t.Errorf("a sync Enable must attach no policy, got %q", got)
	}
}

// U-13: a forced promote never acts on a live peer. When the other site is
// still live the backend answers 409, which is retryable, never a terminal
// failure that would strand the switchover.
func TestSyncPromoteForcedWhilePeerLiveIsRetryable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.failoverStatus = 409
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Force: true, Parameters: syncParams("site-b"),
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (force waits, never acts on a live peer)", status.Code(err))
	}
}

// U-15: a demote of a volume not served on the site is a 204 no-op, so a redundant
// or retried demote succeeds rather than erroring.
func TestSyncDemoteNotServedIsASuccessfulNoOp(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.demoteStatus = 204 // the backend's answer for a volume not served here
	cs := newReplicationTestServer(t, mock)

	if _, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: syncParams("site-a"),
	}); err != nil {
		t.Fatalf("a 204 demote must succeed (idempotent no-op): %v", err)
	}
}

// U-17: a demote whose ANA RPC failed with nothing recorded answers 500, which
// is retryable.
func TestSyncDemoteServerErrorIsRetryable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.demoteStatus = 500
	cs := newReplicationTestServer(t, mock)

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: syncParams("site-a"),
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (a 500 is retryable)", status.Code(err))
	}
}

// U-38: a sync write against a non-sync cluster answers 400, which the driver
// surfaces as InvalidArgument. The backend returns 400 for both a bad/missing
// site and a non-sync cluster, so the driver cannot distinguish them and treats
// both as InvalidArgument (design §11); it is never the 412 escalation trigger.
func TestSyncWriteOnNonSyncClusterIsInvalidArgument(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.failoverStatus = 400
	cs := newReplicationTestServer(t, mock)

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: syncParams("site-b"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (400 is a bad request, never the 412 escalation)", status.Code(err))
	}
}

// U-39: a transport failure reaching the backend (here, the backend is gone) is
// retryable, not a terminal error.
func TestSyncPromoteTransportErrorIsRetryable(t *testing.T) {
	mock := newMockSBCLI()
	cs := newReplicationTestServer(t, mock)
	mock.Close() // the backend is now unreachable

	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		VolumeId: testReplVolID, Parameters: syncParams("site-b"),
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (a transport error is retryable)", status.Code(err))
	}
}

// Phase 2: a sync class on a consistency-group handle drives the whole group as
// one unit through the group routes (design §13).
func TestSyncGroupDispatch(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.seedGroup(vgGroupID, vgMember1, vgMember2)
	cs := newReplicationTestServer(t, mock)
	ctx := context.Background()

	if _, err := cs.PromoteVolume(ctx, &replication.PromoteVolumeRequest{
		VolumeId: vgGroupHandle, Force: false, Parameters: syncParams("site-b"),
	}); err != nil {
		t.Fatalf("group PromoteVolume: %v", err)
	}
	if !mock.groups[vgGroupID].Promoted {
		t.Error("the group was not promoted on this site")
	}

	if _, err := cs.DemoteVolume(ctx, &replication.DemoteVolumeRequest{
		VolumeId: vgGroupHandle, Parameters: syncParams("site-a"),
	}); err != nil {
		t.Fatalf("group DemoteVolume: %v", err)
	}
}
