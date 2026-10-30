package switchover

// How a volume's leadership moves to the site a pod stages on
// (design-stretched-sync-replication-ramen.md §9.3): one demote on the site it
// leaves and one promote on the site it moves to, refused moves reported with
// what blocks them, a planned promote against a lost site escalated only when
// allowed, and one move per store at a time.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/link/sitev1"
	"github.com/simplyblock/atlas/lvol"
)

const vol = "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333"

// fakeBackend records the verbs called and answers from its fields.
type fakeBackend struct {
	mu sync.Mutex

	ledFrom    string
	demoteErr  error
	promoteErr map[bool]error // by planned
	inProgress bool
	gate       chan struct{} // when set, a promote waits for it

	demoted  []string
	promoted []string // "site:planned"
}

func (f *fakeBackend) Volume(context.Context, lvol.VolumeHandle) (lvol.Volume, error) {
	return lvol.Volume{ID: vol, StorageNodeID: "store-1"}, nil
}

func (f *fakeBackend) SyncStatus(context.Context, lvol.VolumeHandle) (controlplane.VolumeSyncStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return controlplane.VolumeSyncStatus{Site: f.ledFrom}, nil
}

func (f *fakeBackend) SyncDemoteVolume(_ context.Context, _ lvol.VolumeHandle, site string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.demoted = append(f.demoted, site)
	return f.demoteErr
}

func (f *fakeBackend) SyncPromoteVolume(_ context.Context, _ lvol.VolumeHandle, site string, planned bool) (controlplane.SyncPromoteResult, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := "planned"
	if !planned {
		kind = "forced"
	}
	f.promoted = append(f.promoted, site+":"+kind)
	if err := f.promoteErr[planned]; err != nil {
		return controlplane.SyncPromoteResult{}, err
	}
	if f.inProgress {
		return controlplane.SyncPromoteResult{InProgress: true}, nil
	}
	f.ledFrom = site
	return controlplane.SyncPromoteResult{}, nil
}

func move(t *testing.T, s *Server, allowForced bool) *sitev1.MoveResponse {
	t.Helper()
	resp, err := s.Move(context.Background(), &sitev1.MoveRequest{Volume: vol, ToSite: "site-b", AllowForced: allowForced})
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	return resp
}

// A volume already led from the target site needs nothing.
func TestAVolumeLedFromTheTargetSiteIsAlreadyMoved(t *testing.T) {
	b := &fakeBackend{ledFrom: "site-b"}
	resp := move(t, New(b, nil), true)
	if resp.GetState() != sitev1.MoveResponse_STATE_MOVED || len(b.demoted)+len(b.promoted) != 0 {
		t.Errorf("state %s after demotes %v promotes %v; want MOVED with no verb", resp.GetState(), b.demoted, b.promoted)
	}
}

// A planned move demotes the site the volume leaves and promotes the target.
func TestAPlannedMoveDemotesThenPromotes(t *testing.T) {
	b := &fakeBackend{ledFrom: "site-a"}
	resp := move(t, New(b, nil), true)
	if resp.GetState() != sitev1.MoveResponse_STATE_MOVED || resp.GetLedFrom() != "site-b" {
		t.Fatalf("state %s led from %q; want MOVED to site-b", resp.GetState(), resp.GetLedFrom())
	}
	if len(b.demoted) != 1 || b.demoted[0] != "site-a" || len(b.promoted) != 1 || b.promoted[0] != "site-b:planned" {
		t.Errorf("demotes %v promotes %v; want demote site-a then a planned promote on site-b", b.demoted, b.promoted)
	}
}

// A promote the backend is still running is in progress, and the caller asks again.
func TestAPromoteStillRunningIsInProgress(t *testing.T) {
	b := &fakeBackend{ledFrom: "site-a", inProgress: true}
	if resp := move(t, New(b, nil), true); resp.GetState() != sitev1.MoveResponse_STATE_IN_PROGRESS {
		t.Errorf("state %s, want IN_PROGRESS", resp.GetState())
	}
}

// A demote refused because the store still serves other volumes on the old site
// is reported with them, and nothing is promoted.
func TestARefusedDemoteNamesWhatBlocksIt(t *testing.T) {
	b := &fakeBackend{ledFrom: "site-a", demoteErr: &controlplane.SyncRefusedError{
		Message: "volumes still served", Volumes: []string{"v-1", "v-2"}}}
	resp := move(t, New(b, nil), true)
	if resp.GetState() != sitev1.MoveResponse_STATE_REFUSED || len(resp.GetBlockingVolumes()) != 2 {
		t.Errorf("state %s blocking %v; want REFUSED naming two volumes", resp.GetState(), resp.GetBlockingVolumes())
	}
	if len(b.promoted) != 0 {
		t.Errorf("promoted %v after a refused demote", b.promoted)
	}
}

// A planned promote refused because the serving site is not online escalates to
// a forced one when the caller allows it, and the escalation is reported.
func TestALostSiteEscalatesToAForcedPromoteWhenAllowed(t *testing.T) {
	offline := map[bool]error{true: controlplane.ErrSyncPeerSiteOffline}
	b := &fakeBackend{ledFrom: "site-a", promoteErr: offline}
	var forced string
	s := New(b, nil)
	s.OnForced = func(volume, from, to string) { forced = from + "->" + to }
	if resp := move(t, s, true); resp.GetState() != sitev1.MoveResponse_STATE_MOVED {
		t.Fatalf("state %s, want MOVED after the escalation", resp.GetState())
	}
	if len(b.promoted) != 2 || b.promoted[1] != "site-b:forced" || forced != "site-a->site-b" {
		t.Errorf("promotes %v, reported %q; want planned then forced, reported site-a->site-b", b.promoted, forced)
	}

	b2 := &fakeBackend{ledFrom: "site-a", promoteErr: offline}
	if resp := move(t, New(b2, nil), false); resp.GetState() != sitev1.MoveResponse_STATE_REFUSED {
		t.Errorf("state %s without allow_forced, want REFUSED", resp.GetState())
	}
	if len(b2.promoted) != 1 {
		t.Errorf("promotes %v without allow_forced; want the planned one only", b2.promoted)
	}
}

// An old site that cannot be reached to demote does not stop the move: the
// promote decides, and is forced only once the backend refuses the planned one.
func TestAnUnreachableOldSiteDoesNotBlockThePromote(t *testing.T) {
	b := &fakeBackend{ledFrom: "site-a", demoteErr: controlplane.ErrSyncPeerSiteOffline}
	if resp := move(t, New(b, nil), true); resp.GetState() != sitev1.MoveResponse_STATE_MOVED {
		t.Errorf("state %s, want MOVED", resp.GetState())
	}
}

// Two moves of one store at once: the second is in progress, not a second
// demote and promote.
func TestOneMovePerStoreAtATime(t *testing.T) {
	b := &fakeBackend{ledFrom: "site-a", gate: make(chan struct{})}
	s := New(b, nil)
	first := make(chan *sitev1.MoveResponse, 1)
	go func() { first <- move(t, s, true) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		demoted := len(b.demoted)
		b.mu.Unlock()
		if demoted == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first move never demoted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if resp := move(t, s, true); resp.GetState() != sitev1.MoveResponse_STATE_IN_PROGRESS {
		t.Errorf("second move of the same store: %s, want IN_PROGRESS", resp.GetState())
	}
	close(b.gate)
	if resp := <-first; resp.GetState() != sitev1.MoveResponse_STATE_MOVED {
		t.Errorf("first move: %s, want MOVED", resp.GetState())
	}
	if len(b.demoted) != 1 {
		t.Errorf("demotes %v; want one", b.demoted)
	}
}

// A malformed request is an error, not a move.
func TestAMoveWithoutATargetSiteIsRefused(t *testing.T) {
	_, err := New(&fakeBackend{}, nil).Move(context.Background(), &sitev1.MoveRequest{Volume: vol})
	if err == nil {
		t.Error("a move with no target site was accepted")
	}
}
