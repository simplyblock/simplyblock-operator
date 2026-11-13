package node

// The stage-time gate of a stretched, sync-replicated cluster
// (design-stretched-sync-replication-ramen.md §9.2, §9.4): a node attaches a
// volume only once the backend reports it led from the node's own site. The
// node never moves a volume; the site's operator moves it when Ramen makes its
// VolumeReplicationGroup primary on the site, and the stage waits for that.

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
)

const (
	gateVolume = "11111111-1111-1111-1111-111111111111:" +
		"22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333"
	thisSite  = "site-b"
	otherSite = "site-a"
)

// gateServer is a node server on thisSite, whose cluster is sync when sync is,
// and whose volume is led from *ledFrom.
func gateServer(sync bool, ledFrom *string) *Server {
	return &Server{
		syncSite: func(string) (string, bool, error) { return thisSite, sync, nil },
		syncStatus: func(context.Context, lvol.VolumeHandle) (controlplane.VolumeSyncStatus, error) {
			return controlplane.VolumeSyncStatus{Site: *ledFrom}, nil
		},
	}
}

// A cluster that is not sync is not gated at all.
func TestANonSyncClusterIsNotGated(t *testing.T) {
	led := otherSite
	if err := gateServer(false, &led).ensureLedHere(context.Background(), gateVolume); err != nil {
		t.Fatalf("a non-sync cluster was gated: %v", err)
	}
}

// A sync cluster whose secret entry names no site cannot tell where this node
// is, so it holds before any backend call rather than staging unchecked.
func TestASyncClusterWithoutASiteHoldsBeforeAnyCall(t *testing.T) {
	led := otherSite
	s := gateServer(true, &led)
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
	if read {
		t.Error("the status was read; want no backend call")
	}
}

// A volume led from this site stages.
func TestAVolumeLedHereStages(t *testing.T) {
	led := thisSite
	if err := gateServer(true, &led).ensureLedHere(context.Background(), gateVolume); err != nil {
		t.Fatalf("a volume led here was held: %v", err)
	}
}

// Ramen drives a stretched cluster's moves: the operator moves a volume when
// Ramen makes its VRG primary on a site, before the pods start there. A stage
// that finds the volume led from the other site waits for that move, retryably,
// and says what it waits for (design-stretched-sync-replication-ramen.md §9.4).
func TestAVolumeLedElsewhereIsWaitedFor(t *testing.T) {
	led := otherSite
	err := gateServer(true, &led).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable while the volume is led from %s", err, otherSite)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, otherSite) || !strings.Contains(msg, "waiting") {
		t.Errorf("message %q does not say where the volume is led from and that the stage waits", msg)
	}
}

// A volume led from no site, in the window after a promote, holds.
func TestAVolumeLedFromNoSiteIsHeld(t *testing.T) {
	led := ""
	err := gateServer(true, &led).ensureLedHere(context.Background(), gateVolume)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
}

// A stage on a sync cluster whose volume is led from the other site holds on the
// gate before the stack is brought up: nothing is connected on the wrong site.
func TestAStageHoldsOnTheGateBeforeBringingTheStackUp(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	led := otherSite
	gate := gateServer(true, &led)
	ns.syncSite, ns.syncStatus = gate.syncSite, gate.syncStatus
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
