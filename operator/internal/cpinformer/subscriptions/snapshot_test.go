// Tests for the snapshot subscription: finding a snapshot by the name its
// requester gave it, and the name-carrying trigger a request reconciler maps
// back to its object.

package subscriptions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

const (
	snapshotCluster = "11111111-1111-1111-1111-111111111111"
	snapshotPool    = "22222222-2222-2222-2222-222222222222"
	snapshotID      = "33333333-3333-3333-3333-333333333333"
	snapshotName    = "sbk-99999999-9999-9999-9999-999999999999"
)

func snapshotScope() cpinformer.Scope { return cpinformer.Scope{snapshotCluster, snapshotPool} }

func snapshotFrame(t *testing.T, snaps ...SnapshotDTO) cpinformer.Event {
	t.Helper()
	data, err := json.Marshal(snaps)
	if err != nil {
		t.Fatalf("encode the snapshot frame: %v", err)
	}
	return cpinformer.Event{Kind: cpinformer.EventSnapshot, Scope: snapshotScope(), Data: data}
}

func TestSnapshotSubscriptionStreamsOnePoolsSnapshots(t *testing.T) {
	got := NewSnapshotSubscription().Path(snapshotScope())

	want := "/api/v2/clusters/" + snapshotCluster + "/storage-pools/" + snapshotPool + "/snapshots/"
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestSnapshotSubscriptionFindsASnapshotByName(t *testing.T) {
	s := NewSnapshotSubscription()
	if err := s.Ingest(context.Background(),
		snapshotFrame(t, SnapshotDTO{ID: snapshotID, Name: snapshotName, Status: "online"})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	dto, ok := s.FindByName(snapshotScope(), snapshotName)

	if !ok || dto.ID != snapshotID {
		t.Errorf("FindByName = %+v, %v, want the snapshot %s", dto, ok, snapshotID)
	}
	if _, ok := s.FindByName(snapshotScope(), "sbk-other"); ok {
		t.Error("FindByName found a snapshot nobody named that")
	}
	if _, ok := s.FindByName(cpinformer.Scope{snapshotCluster, "other-pool"}, snapshotName); ok {
		t.Error("FindByName found the snapshot in a pool it is not in")
	}
}

func TestSnapshotSubscriptionIsAuthoritativeOnlyOnceSynced(t *testing.T) {
	s := NewSnapshotSubscription()
	if s.Synced(snapshotScope()) {
		t.Fatal("a scope that has seen no frame reports itself synced")
	}

	if err := s.Ingest(context.Background(), snapshotFrame(t)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if !s.Synced(snapshotScope()) {
		t.Error("an empty first frame did not sync the scope")
	}
}

// A request is a person's object with a name of their choosing, so the stream
// cannot name it. The trigger names the snapshot, and the reconciler maps that
// back.
func TestSnapshotSubscriptionTriggersByTheSnapshotsName(t *testing.T) {
	s := NewSnapshotSubscription()

	if err := s.Ingest(context.Background(),
		snapshotFrame(t, SnapshotDTO{ID: snapshotID, Name: snapshotName})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	select {
	case ev := <-s.Triggers():
		if _, ok := ev.Object.(*metav1.PartialObjectMetadata); !ok || ev.Object.GetName() != snapshotName {
			t.Errorf("trigger = %T %q, want metadata named %q", ev.Object, ev.Object.GetName(), snapshotName)
		}
	case <-time.After(time.Second):
		t.Fatal("no trigger for a snapshot the stream reported")
	}
}
