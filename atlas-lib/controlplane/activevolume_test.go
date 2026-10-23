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
	clusterA = "aaaaaaaa-0000-0000-0000-000000000001"
	clusterB = "bbbbbbbb-0000-0000-0000-000000000001"
	clusterC = "cccccccc-0000-0000-0000-000000000001"
	poolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
	poolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
	poolC    = "cccccccc-0000-0000-0000-00000000000c"
	volA     = "11111111-1111-1111-1111-111111111111"
	volB     = "22222222-2222-2222-2222-222222222222"
	volC     = "33333333-3333-3333-3333-333333333333"
)

// relationships answers by volume id, which is how the control plane looks a
// relationship up: the newest record naming the volume on either side.
type relationships map[string]Relationship

func (r relationships) lookup(_ context.Context, h lvol.Handle) (Relationship, error) {
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
	got, err := ActiveVolume(context.Background(), handleOf(clusterA, poolA, volA), relationships{}.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got != handleOf(clusterA, poolA, volA) {
		t.Errorf("got %s, want the start handle", got)
	}
}

func TestActiveVolume_SourceStillServingTakesNoHop(t *testing.T) {
	rels := relationships{volA: {IsSource: true, TargetClusterID: clusterB, TargetPoolID: poolB,
		TargetLvolID: volB, ActiveLvolID: volA}}
	got, err := ActiveVolume(context.Background(), handleOf(clusterA, poolA, volA), rels.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != volA {
		t.Errorf("got %s, want %s: replication that has not cut over leaves the data on the source", got, volA)
	}
}

// A -> B -> C, both moves completed: the PV still names A.
func TestActiveVolume_FollowsTwoMigrations(t *testing.T) {
	rels := relationships{
		volA: {IsSource: true, TargetClusterID: clusterB, TargetPoolID: poolB, TargetLvolID: volB, ActiveLvolID: volC},
		volB: {IsSource: true, TargetClusterID: clusterC, TargetPoolID: poolC, TargetLvolID: volC, ActiveLvolID: volC},
		volC: {IsSource: false, TargetClusterID: clusterC, TargetPoolID: poolC, TargetLvolID: volC, ActiveLvolID: volC},
	}
	got, err := ActiveVolume(context.Background(), handleOf(clusterA, poolA, volA), rels.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got != handleOf(clusterC, poolC, volC) {
		t.Errorf("got %s, want %s", got, handleOf(clusterC, poolC, volC))
	}
}

// A control plane that only knows the first move reports its target as the
// active volume; the target's own relationship carries the walk on.
func TestActiveVolume_ContinuesPastAFirstHopThatNamesItsTargetActive(t *testing.T) {
	rels := relationships{
		volA: {IsSource: true, TargetClusterID: clusterB, TargetPoolID: poolB, TargetLvolID: volB, ActiveLvolID: volB},
		volB: {IsSource: true, TargetClusterID: clusterC, TargetPoolID: poolC, TargetLvolID: volC, ActiveLvolID: volC},
	}
	got, err := ActiveVolume(context.Background(), handleOf(clusterA, poolA, volA), rels.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != volC {
		t.Errorf("got %s, want %s", got, volC)
	}
}

func TestActiveVolume_StopsOnALoop(t *testing.T) {
	rels := relationships{
		volA: {IsSource: true, TargetClusterID: clusterB, TargetPoolID: poolB, TargetLvolID: volB, ActiveLvolID: volB},
		volB: {IsSource: true, TargetClusterID: clusterA, TargetPoolID: poolA, TargetLvolID: volA, ActiveLvolID: volA},
	}
	_, err := ActiveVolume(context.Background(), handleOf(clusterA, poolA, volA), rels.lookup)
	if err == nil || !strings.Contains(err.Error(), "loops") {
		t.Errorf("err = %v, want a loop error", err)
	}
}

func TestActiveVolume_LookupFailureIsAnError(t *testing.T) {
	boom := errors.New("control plane unavailable")
	_, err := ActiveVolume(context.Background(), handleOf(clusterA, poolA, volA),
		func(context.Context, lvol.Handle) (Relationship, error) { return Relationship{}, boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}
