package node

// The stage-time gate of a stretched, sync-replicated cluster
// (design-stretched-sync-replication-ramen.md §9.2, §9.4): a node attaches a
// volume only once the backend reports it led from the node's own site. A
// volume led from the other site is moved by asking the site's operator, and the
// stage holds until the status names this site.

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/link/sitev1"
	"github.com/simplyblock/atlas/lvol"
)

const (
	gateVolume = "11111111-1111-1111-1111-111111111111:" +
		"22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333"
	thisSite  = "site-b"
	otherSite = "site-a"
)

// fakeSwitchover answers Move from a script and records the requests.
type fakeSwitchover struct {
	answer *sitev1.MoveResponse
	err    error
	asked  []*sitev1.MoveRequest
	onMove func()
}

func (f *fakeSwitchover) Move(
	_ context.Context, req *sitev1.MoveRequest, _ ...grpc.CallOption,
) (*sitev1.MoveResponse, error) {
	f.asked = append(f.asked, req)
	if f.onMove != nil {
		f.onMove()
	}
	return f.answer, f.err
}

// gateServer is a node server on thisSite, whose cluster is sync when sync is,
// whose volume is led from *ledFrom, and whose operator answers through sw.
func gateServer(sync bool, ledFrom *string, sw *fakeSwitchover) *Server {
	return &Server{
		syncSite: func(string) (string, bool, error) { return thisSite, sync, nil },
		syncStatus: func(context.Context, lvol.VolumeHandle) (controlplane.VolumeSyncStatus, error) {
			return controlplane.VolumeSyncStatus{Site: *ledFrom}, nil
		},
		switchover: func() (sitev1.SwitchoverServiceClient, error) {
			if sw == nil {
				return nil, errors.New("not linked")
			}
			return sw, nil
		},
	}
}

// A cluster that is not sync is not gated at all.
func TestANonSyncClusterIsNotGated(t *testing.T) {
	led := otherSite
	sw := &fakeSwitchover{}
	if err := gateServer(false, &led, sw).ensureLedHere(context.Background(), gateVolume); err != nil {
		t.Fatalf("a non-sync cluster was gated: %v", err)
	}
	if len(sw.asked) != 0 {
		t.Error("a non-sync cluster asked for a move")
	}
}

// A sync cluster whose secret entry names no site cannot tell where this node
// is, so it holds before any backend call rather than staging unchecked.
func TestASyncClusterWithoutASiteHoldsBeforeAnyCall(t *testing.T) {
	led := otherSite
	sw := &fakeSwitchover{}
	s := gateServer(true, &led, sw)
	s.syncSite = func(string) (string, bool, error) { return "", true, nil }
	read := false
	s.syncStatus = func(context.Context, lvol.VolumeHandle) (controlplane.VolumeSyncStatus, error) {
		read = true
		return controlplane.VolumeSyncStatus{Site: led}, nil
	}
	err := s.ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
	if read || len(sw.asked) != 0 {
		t.Errorf("status read %v, moves %v; want no backend call", read, sw.asked)
	}
}

// A volume led from this site stages with no move.
func TestAVolumeLedHereStagesWithoutAMove(t *testing.T) {
	led := thisSite
	sw := &fakeSwitchover{}
	if err := gateServer(true, &led, sw).ensureLedHere(context.Background(), gateVolume); err != nil {
		t.Fatalf("a volume led here was held: %v", err)
	}
	if len(sw.asked) != 0 {
		t.Error("a volume led here asked for a move")
	}
}

// A volume led from the other site is moved here, allowing a forced promote,
// and stages once the status names this site.
func TestAVolumeLedElsewhereIsMovedHereFirst(t *testing.T) {
	led := otherSite
	sw := &fakeSwitchover{answer: &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_MOVED, LedFrom: thisSite}}
	sw.onMove = func() { led = thisSite }
	if err := gateServer(true, &led, sw).ensureLedHere(context.Background(), gateVolume); err != nil {
		t.Fatalf("the stage was held after the move: %v", err)
	}
	if len(sw.asked) != 1 || sw.asked[0].GetToSite() != thisSite || !sw.asked[0].GetAllowForced() ||
		sw.asked[0].GetVolume() != gateVolume {
		t.Errorf("asked %v; want one move of the volume to site-b, forcing allowed", sw.asked)
	}
}

// A move the operator reports done that the status does not reflect yet is
// held one more retry: the node never attaches on the operator's word alone.
func TestAMoveTheStatusDoesNotReflectIsHeld(t *testing.T) {
	led := otherSite
	sw := &fakeSwitchover{answer: &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_MOVED}}
	err := gateServer(true, &led, sw).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable while the status still names site-a", err)
	}
}

// A move under way holds the stage, retryably.
func TestAMoveInProgressHoldsTheStage(t *testing.T) {
	led := otherSite
	sw := &fakeSwitchover{answer: &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_IN_PROGRESS}}
	err := gateServer(true, &led, sw).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
}

// A refused move holds the stage with the backend's reason and what blocks it.
func TestARefusedMoveSaysWhy(t *testing.T) {
	led := otherSite
	sw := &fakeSwitchover{answer: &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_REFUSED,
		Reason: "volumes still served", BlockingVolumes: []string{"v-1"}}}
	err := gateServer(true, &led, sw).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if msg := status.Convert(err).Message(); !contains(msg, "volumes still served") || !contains(msg, "v-1") {
		t.Errorf("message %q does not carry the reason and the blocking volume", msg)
	}
}

// A volume led from no site, in the window after a promote, holds without a move.
func TestAVolumeLedFromNoSiteIsHeldWithoutAMove(t *testing.T) {
	led := ""
	sw := &fakeSwitchover{}
	err := gateServer(true, &led, sw).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.Unavailable || len(sw.asked) != 0 {
		t.Errorf("err = %v after %d moves; want Unavailable and no move", err, len(sw.asked))
	}
}

// A node not linked to its operator cannot ask for the move and holds.
func TestANodeNotLinkedToItsOperatorHolds(t *testing.T) {
	led := otherSite
	err := gateServer(true, &led, nil).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A stage on a sync cluster whose volume is led from the other site holds on the
// gate before the stack is brought up: nothing is connected on the wrong site.
func TestAStageHoldsOnTheGateBeforeBringingTheStackUp(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	led := otherSite
	gate := gateServer(true, &led, &fakeSwitchover{
		answer: &sitev1.MoveResponse{State: sitev1.MoveResponse_STATE_IN_PROGRESS}})
	ns.syncSite, ns.syncStatus, ns.switchover = gate.syncSite, gate.syncStatus, gate.switchover
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          gateVolume,
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCapability(),
		VolumeContext:     stagedContext(),
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("NodeStageVolume = %v, want the gate's Unavailable", err)
	}
	if runner.called("up") {
		t.Error("the stack was brought up for a volume led from the other site")
	}
}
