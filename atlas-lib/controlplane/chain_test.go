// The walk from a volume handle to the volume serving its data, across the
// replication relationships the control plane keeps after a move or a
// fail-over. Each cluster is its own test server, as each is its own entry
// in a caller's credentials.

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

// relationships serves a cluster's relationship records by volume id, and
// counts the reads.
type relationships struct {
	records map[string]string
	reads   int
}

func (r *relationships) serve(w http.ResponseWriter, req *http.Request) {
	r.reads++
	parts := strings.Split(strings.TrimSuffix(req.URL.Path, "/"), "/")
	record, ok := r.records[parts[len(parts)-1]]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(record))
}

// moved is the record a commit with delete_source leaves on the source: the
// source names the target, which is the active volume.
func moved(source, targetCluster, targetPool, target, active string) string {
	return fmt.Sprintf(`{"replication_id":"77777777-7777-7777-7777-777777777777","is_source":true,"state":"cutover_done","mode":"migration",`+
		`"direction":"to_target","source_lvol_id":%q,"target_cluster_id":%q,"target_pool_id":%q,`+
		`"target_lvol_id":%q,"active_lvol_id":%q}`, source, targetCluster, targetPool, target, active)
}

const (
	clusterA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	clusterB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	poolB    = "b0b0b0b0-b0b0-b0b0-b0b0-b0b0b0b0b0b0"
	volA     = "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1"
	volB     = "b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1"
)

func twoClusters(t *testing.T, a, b *relationships) (*Client, ClientFor[*Client]) {
	t.Helper()
	clients := map[string]*Client{
		clusterA: newTestClient(t, a.serve),
		clusterB: newTestClient(t, b.serve),
	}
	return clients[clusterA], func(_ context.Context, id string) (*Client, error) {
		if c, ok := clients[id]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("no credentials for cluster %s", id)
	}
}

// U-116, the shared half: a volume moved from A to B resolves to B's volume,
// with B's client.
func TestTheChainOfAMovedVolumeEndsAtTheTarget(t *testing.T) {
	a := &relationships{records: map[string]string{volA: moved(volA, clusterB, poolB, volB, volB)}}
	b := &relationships{records: map[string]string{volB: `{"replication_id":"77777777-7777-7777-7777-777777777777","is_source":false,"target_lvol_id":"` + volB + `"}`}}
	c, clientFor := twoClusters(t, a, b)

	hops, known, err := WalkChain(context.Background(), lvol.Handle{ClusterID: clusterA, PoolRef: testPool, VolumeID: volA}, c, clientFor)

	if err != nil || !known {
		t.Fatalf("WalkChain = %v, known %v", err, known)
	}
	end := hops[len(hops)-1]
	if len(hops) != 2 || end.Handle != (lvol.Handle{ClusterID: clusterB, PoolRef: poolB, VolumeID: volB}) {
		t.Errorf("hops = %+v, want A then B's volume", hops)
	}
	if end.Client == nil || end.Client == c {
		t.Error("the target hop carries no client")
	}
}

// A volume without a relationship is its own chain, and known is false.
func TestAVolumeWithoutARelationshipIsItsOwnChain(t *testing.T) {
	a := &relationships{records: map[string]string{}}
	c, clientFor := twoClusters(t, a, &relationships{})
	start := lvol.Handle{ClusterID: clusterA, PoolRef: testPool, VolumeID: volA}

	hops, known, err := WalkChain(context.Background(), start, c, clientFor)

	if err != nil || known || len(hops) != 1 || hops[0].Handle != start {
		t.Errorf("WalkChain = %+v, known %v, %v; want the start alone", hops, known, err)
	}
}

// U-118: a chain whose records loop fails with a named error instead of
// walking forever.
func TestALoopingChainFailsWithANamedError(t *testing.T) {
	elsewhere := "e1e1e1e1-e1e1-e1e1-e1e1-e1e1e1e1e1e1"
	a := &relationships{records: map[string]string{volA: moved(volA, clusterB, poolB, volB, elsewhere)}}
	b := &relationships{records: map[string]string{volB: moved(volB, clusterA, testPool, volA, elsewhere)}}
	c, clientFor := twoClusters(t, a, b)

	_, _, err := WalkChain(context.Background(), lvol.Handle{ClusterID: clusterA, PoolRef: testPool, VolumeID: volA}, c, clientFor)

	if !errors.Is(err, ErrChainDoesNotConverge) {
		t.Errorf("WalkChain = %v, want ErrChainDoesNotConverge", err)
	}
}

// A hop into a cluster the caller holds no credentials for is an error, not
// a chain that ends early.
func TestAHopIntoAnUnknownClusterIsAnError(t *testing.T) {
	a := &relationships{records: map[string]string{volA: moved(volA, "cccccccc-cccc-cccc-cccc-cccccccccccc", poolB, volB, volB)}}
	c, clientFor := twoClusters(t, a, &relationships{})

	_, _, err := WalkChain(context.Background(), lvol.Handle{ClusterID: clusterA, PoolRef: testPool, VolumeID: volA}, c, clientFor)

	if err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("WalkChain = %v, want the credentials error", err)
	}
}
