// A retried CreateSnapshot of a striped volume: answered with the snapshot an
// earlier attempt took when that attempt sealed it on its VolumeSnapshotContent,
// and taken again otherwise.
package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// fakeSealer is the VolumeSnapshotContents' seals, by content name.
type fakeSealer struct {
	seals map[string]string
}

func (f *fakeSealer) Sealed(_ context.Context, content string) (string, error) {
	return f.seals[content], nil
}

func (f *fakeSealer) Seal(_ context.Context, content, id string) error {
	f.seals[content] = id
	return nil
}

func sealedRequest(volumeID string) *csi.CreateSnapshotRequest {
	return &csi.CreateSnapshotRequest{
		SourceVolumeId: volumeID,
		Name:           "snapshot-1",
		Parameters:     map[string]string{paramSnapshotContentName: "snapcontent-1"},
	}
}

func countOf(events []string, what string) int {
	n := 0
	for _, e := range events {
		if e == what {
			n++
		}
	}
	return n
}

// Regression: 2026-11-17-striped-snapshot-retry (PR #922 review). A retried
// CreateSnapshot, which a lost response or a failed status update makes,
// deleted the member snapshots the first attempt had taken under a freeze that
// held, froze the workload again, and answered a new ID. The first attempt
// seals the snapshot on its VolumeSnapshotContent, and the retry answers the
// same snapshot without a second freeze.
func TestARetriedStripedSnapshotAnswersTheSealedOne(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	cs := frozenServer(t, mock, &fakeFreezer{events: ev, frozen: true, held: true}, ev)
	sealer := &fakeSealer{seals: map[string]string{}}
	cs.snapshotSeals = sealer
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

	first, err := cs.CreateSnapshot(context.Background(), sealedRequest(vol.GetVolumeId()))
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if got := sealer.seals["snapcontent-1"]; got != first.GetSnapshot().GetSnapshotId() {
		t.Fatalf("the content is sealed with %q, want %q", got, first.GetSnapshot().GetSnapshotId())
	}

	again, err := cs.CreateSnapshot(context.Background(), sealedRequest(vol.GetVolumeId()))
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if again.GetSnapshot().GetSnapshotId() != first.GetSnapshot().GetSnapshotId() {
		t.Errorf("the retry answered %s, want the sealed %s",
			again.GetSnapshot().GetSnapshotId(), first.GetSnapshot().GetSnapshotId())
	}
	if again.GetSnapshot().GetSizeBytes() != first.GetSnapshot().GetSizeBytes() {
		t.Errorf("the retry answered %d bytes, want %d",
			again.GetSnapshot().GetSizeBytes(), first.GetSnapshot().GetSizeBytes())
	}
	if n := countOf(ev.all(), "freeze worker-1"); n != 1 {
		t.Errorf("the workload was frozen %d times, want once: %v", n, ev.all())
	}
	if n := snapshotCount(mock); n != 3 {
		t.Errorf("%d member snapshots, want the first attempt's 3", n)
	}
}

// An unsealed set is not trusted, whatever it holds: the attempt that left it
// may have had a freeze that did not hold, and its rollback may have failed.
// It is taken again under a new freeze.
func TestAnUnsealedStripedSnapshotIsTakenAgain(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	cs := frozenServer(t, mock, &fakeFreezer{events: ev, frozen: true, held: true}, ev)
	cs.snapshotSeals = &fakeSealer{seals: map[string]string{}}
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

	if _, err := cs.CreateSnapshot(context.Background(), sealedRequest(vol.GetVolumeId())); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	cs.snapshotSeals = &fakeSealer{seals: map[string]string{}}
	if _, err := cs.CreateSnapshot(context.Background(), sealedRequest(vol.GetVolumeId())); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if n := countOf(ev.all(), "freeze worker-1"); n != 2 {
		t.Errorf("an unsealed set was answered without a new freeze: %v", ev.all())
	}
}

// A seal whose member snapshots are no longer all there is stale, and the
// snapshot is taken again rather than answered with a set that has a hole.
func TestAStaleSealIsTakenAgain(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	ev := &events{}
	cs := frozenServer(t, mock, &fakeFreezer{events: ev, frozen: true, held: true}, ev)
	cs.snapshotSeals = &fakeSealer{seals: map[string]string{}}
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 3*gib))

	first, err := cs.CreateSnapshot(context.Background(), sealedRequest(vol.GetVolumeId()))
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	mock.mu.Lock()
	for id, s := range mock.snapshots {
		if s.Name == "snapshot-1-s2" {
			delete(mock.snapshots, id)
		}
	}
	mock.mu.Unlock()

	again, err := cs.CreateSnapshot(context.Background(), sealedRequest(vol.GetVolumeId()))
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if again.GetSnapshot().GetSnapshotId() == first.GetSnapshot().GetSnapshotId() ||
		!slices.Contains(ev.all(), "freeze worker-1") || countOf(ev.all(), "freeze worker-1") != 2 {
		t.Errorf("a seal missing a member was answered as is: %s, events %v", again.GetSnapshot().GetSnapshotId(), ev.all())
	}
}
