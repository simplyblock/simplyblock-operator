// Tests for ActiveVolume: the walk from a provisioning-time handle to the
// volume that serves the data after one or more completed moves.

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/lvol"
)

const (
	avClusterA = "aaaaaaaa-0000-0000-0000-000000000001"
	avClusterB = "bbbbbbbb-0000-0000-0000-000000000001"
	avClusterC = "cccccccc-0000-0000-0000-000000000001"
	avPoolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
	avPoolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
	avPoolC    = "cccccccc-0000-0000-0000-00000000000c"
	avVolA     = "11111111-1111-1111-1111-111111111111"
	avVolB     = "22222222-2222-2222-2222-222222222222"
	avVolC     = "33333333-3333-3333-3333-333333333333"
)

// relationshipsByVolume answers by volume id, which is how the control plane looks a
// relationship up: the newest record naming the volume on either side.
type relationshipsByVolume map[string]Relationship

func (r relationshipsByVolume) lookup(_ context.Context, h lvol.Handle) (Relationship, error) {
	rel, ok := r[h.VolumeID]
	if !ok {
		return Relationship{}, fmt.Errorf("volume %s: %w", h.VolumeID, errs.ErrNotFound)
	}
	return rel, nil
}

func handleOf(cluster, pool, vol string) lvol.Handle {
	return lvol.Handle{ClusterID: cluster, PoolRef: pool, VolumeID: vol}
}

func TestActiveVolume_NoRelationshipIsTheVolumeItself(t *testing.T) {
	got, err := ActiveVolume(context.Background(), handleOf(avClusterA, avPoolA, avVolA), relationshipsByVolume{}.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got != handleOf(avClusterA, avPoolA, avVolA) {
		t.Errorf("got %s, want the start handle", got)
	}
}

func TestActiveVolume_SourceStillServingTakesNoHop(t *testing.T) {
	rels := relationshipsByVolume{avVolA: {IsSource: true, TargetClusterID: avClusterB, TargetPoolID: avPoolB,
		TargetLvolID: avVolB, ActiveLvolID: avVolA}}
	got, err := ActiveVolume(context.Background(), handleOf(avClusterA, avPoolA, avVolA), rels.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != avVolA {
		t.Errorf("got %s, want %s: replication that has not cut over leaves the data on the source", got, avVolA)
	}
}

// A -> B -> C, both moves completed: the PV still names A.
func TestActiveVolume_FollowsTwoMigrations(t *testing.T) {
	rels := relationshipsByVolume{
		avVolA: {IsSource: true, TargetClusterID: avClusterB, TargetPoolID: avPoolB, TargetLvolID: avVolB, ActiveLvolID: avVolC},
		avVolB: {IsSource: true, TargetClusterID: avClusterC, TargetPoolID: avPoolC, TargetLvolID: avVolC, ActiveLvolID: avVolC},
		avVolC: {IsSource: false, TargetClusterID: avClusterC, TargetPoolID: avPoolC, TargetLvolID: avVolC, ActiveLvolID: avVolC},
	}
	got, err := ActiveVolume(context.Background(), handleOf(avClusterA, avPoolA, avVolA), rels.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got != handleOf(avClusterC, avPoolC, avVolC) {
		t.Errorf("got %s, want %s", got, handleOf(avClusterC, avPoolC, avVolC))
	}
}

// A control plane that only knows the first move reports its target as the
// active volume; the target's own relationship carries the walk on.
func TestActiveVolume_ContinuesPastAFirstHopThatNamesItsTargetActive(t *testing.T) {
	rels := relationshipsByVolume{
		avVolA: {IsSource: true, TargetClusterID: avClusterB, TargetPoolID: avPoolB, TargetLvolID: avVolB, ActiveLvolID: avVolB},
		avVolB: {IsSource: true, TargetClusterID: avClusterC, TargetPoolID: avPoolC, TargetLvolID: avVolC, ActiveLvolID: avVolC},
	}
	got, err := ActiveVolume(context.Background(), handleOf(avClusterA, avPoolA, avVolA), rels.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != avVolC {
		t.Errorf("got %s, want %s", got, avVolC)
	}
}

func TestActiveVolume_StopsOnALoop(t *testing.T) {
	rels := relationshipsByVolume{
		avVolA: {IsSource: true, TargetClusterID: avClusterB, TargetPoolID: avPoolB, TargetLvolID: avVolB, ActiveLvolID: avVolB},
		avVolB: {IsSource: true, TargetClusterID: avClusterA, TargetPoolID: avPoolA, TargetLvolID: avVolA, ActiveLvolID: avVolA},
	}
	_, err := ActiveVolume(context.Background(), handleOf(avClusterA, avPoolA, avVolA), rels.lookup)
	if err == nil || !strings.Contains(err.Error(), "loops") {
		t.Errorf("err = %v, want a loop error", err)
	}
}

func TestActiveVolume_LookupFailureIsAnError(t *testing.T) {
	boom := errors.New("control plane unavailable")
	_, err := ActiveVolume(context.Background(), handleOf(avClusterA, avPoolA, avVolA),
		func(context.Context, lvol.Handle) (Relationship, error) { return Relationship{}, boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}
