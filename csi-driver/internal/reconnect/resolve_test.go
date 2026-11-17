package reconnect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

const (
	lvolMoved = "d4444444-4444-4444-8444-444444444444"
	clusterA  = "aaaaaaaa-0000-4000-8000-000000000000" // the cluster the NQN/handle names
	clusterB  = "bbbbbbbb-0000-4000-8000-000000000000"
	clusterC  = "cccccccc-0000-4000-8000-000000000000"
)

// fakeVolumeClient answers VolumeNodeInfo with err, or with a NodeInfo naming
// its cluster when err is nil.
type fakeVolumeClient struct {
	cluster string
	err     error
}

func (f *fakeVolumeClient) VolumeNodeInfo(_ context.Context, _ string) (*controlplane.NodeInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &controlplane.NodeInfo{NodeID: "node-of-" + f.cluster}, nil
}

func (f *fakeVolumeClient) LvolConnections(
	_ context.Context, _, _ string,
) ([]*controlplane.LvolConnectResp, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []*controlplane.LvolConnectResp{{IP: f.cluster}}, nil
}

func (f *fakeVolumeClient) StorageNodeStatus(context.Context, string) (string, error) {
	return "online", nil
}

// notInPool is what findPoolForVolume reports for a volume a cluster lacks.
var notInPool = fmt.Errorf("failed to resolve pool for volume %s: %w", lvolMoved,
	fmt.Errorf("volume %s not found in any pool: %w", lvolMoved, controlplane.ErrVolumeNotFound))

// testResolver builds a resolver over fake clusters, counting the queries each
// cluster answers.
func testResolver(answers map[string]error, queries map[string]int) *clusterResolver {
	ids := []string{clusterA, clusterB, clusterC}
	return &clusterResolver{
		resolved: map[string]string{},
		newClient: func(_ context.Context, id string) (volumeClient, error) {
			ans, ok := answers[id]
			if !ok {
				return nil, fmt.Errorf("cluster %s: %w", id, controlplane.ErrClusterNotFound)
			}
			queries[id]++
			return &fakeVolumeClient{cluster: id, err: ans}, nil
		},
		list: func() ([]string, error) { return ids, nil },
	}
}

func nodeInfoQuery(info **controlplane.NodeInfo) func(volumeClient) error {
	return func(c volumeClient) error {
		var err error
		*info, err = c.VolumeNodeInfo(context.Background(), lvolMoved)
		return err
	}
}

func TestResolverFoundInNamedCluster(t *testing.T) {
	queries := map[string]int{}
	r := testResolver(map[string]error{clusterA: nil, clusterB: nil, clusterC: nil}, queries)

	var info *controlplane.NodeInfo
	_, id, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
	if err != nil || id != clusterA || info.NodeID != "node-of-"+clusterA {
		t.Fatalf("lookup = (%s, %v, %+v), want cluster A", id, err, info)
	}
	if queries[clusterA] != 1 || queries[clusterB] != 0 || queries[clusterC] != 0 {
		t.Fatalf("queries = %v, want only one to the named cluster", queries)
	}
	if len(r.resolved) != 0 {
		t.Fatalf("cache = %v, want empty for a volume in its named cluster", r.resolved)
	}
}

func TestResolverFollowsMovedVolumeAndCaches(t *testing.T) {
	queries := map[string]int{}
	r := testResolver(map[string]error{clusterA: notInPool, clusterB: notInPool, clusterC: nil}, queries)

	var info *controlplane.NodeInfo
	_, id, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
	if err != nil || id != clusterC || info.NodeID != "node-of-"+clusterC {
		t.Fatalf("lookup = (%s, %v, %+v), want cluster C", id, err, info)
	}
	if r.resolved[lvolMoved] != clusterC {
		t.Fatalf("cache = %v, want %s -> C", r.resolved, lvolMoved)
	}

	// Steady state: one query, straight to the remembered cluster.
	for k := range queries {
		delete(queries, k)
	}
	_, id, err = r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
	if err != nil || id != clusterC {
		t.Fatalf("second lookup = (%s, %v), want cluster C", id, err)
	}
	if queries[clusterC] != 1 || queries[clusterA] != 0 || queries[clusterB] != 0 {
		t.Fatalf("second lookup queries = %v, want exactly one to C", queries)
	}
}

func TestResolverInvalidatesOnLaterNotFound(t *testing.T) {
	answers := map[string]error{clusterA: notInPool, clusterB: nil, clusterC: notInPool}
	queries := map[string]int{}
	r := testResolver(answers, queries)

	var info *controlplane.NodeInfo
	if _, id, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info)); err != nil ||
		id != clusterB {
		t.Fatalf("lookup = (%s, %v), want B", id, err)
	}

	// The volume moves back to its named cluster.
	answers[clusterB] = notInPool
	answers[clusterA] = nil
	_, id, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
	if err != nil || id != clusterA {
		t.Fatalf("lookup after move back = (%s, %v), want A", id, err)
	}
	if _, ok := r.resolved[lvolMoved]; ok {
		t.Fatalf("cache = %v, want the stale entry dropped", r.resolved)
	}
}

func TestResolverNotFoundAnywherePreservesError(t *testing.T) {
	queries := map[string]int{}
	r := testResolver(map[string]error{clusterA: notInPool, clusterB: notInPool, clusterC: notInPool}, queries)

	var info *controlplane.NodeInfo
	_, _, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
	if !errors.Is(err, notInPool) {
		t.Fatalf("error = %v, want the named cluster's own error", err)
	}
	if queries[clusterA] != 1 || queries[clusterB] != 1 || queries[clusterC] != 1 {
		t.Fatalf("queries = %v, want each cluster asked once", queries)
	}
	if len(r.resolved) != 0 {
		t.Fatalf("cache = %v, want nothing cached", r.resolved)
	}
}

func TestResolverTransportErrorDoesNotScan(t *testing.T) {
	for name, transport := range map[string]error{
		"connection refused": errors.New("GET: dial tcp 10.0.0.1:443: connect: connection refused"),
		"server error":       &controlplane.HTTPError{Method: "GET", StatusCode: http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			queries := map[string]int{}
			r := testResolver(map[string]error{clusterA: transport, clusterB: nil, clusterC: nil}, queries)

			var info *controlplane.NodeInfo
			_, _, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
			if !errors.Is(err, transport) {
				t.Fatalf("error = %v, want the transport error", err)
			}
			if queries[clusterB] != 0 || queries[clusterC] != 0 {
				t.Fatalf("queries = %v, want no cross-cluster scan", queries)
			}
		})
	}
}

func TestResolverTransportErrorKeepsCache(t *testing.T) {
	answers := map[string]error{clusterA: notInPool, clusterB: nil, clusterC: notInPool}
	r := testResolver(answers, map[string]int{})

	var info *controlplane.NodeInfo
	if _, _, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info)); err != nil {
		t.Fatal(err)
	}
	answers[clusterB] = errors.New("i/o timeout")
	if _, _, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info)); err == nil {
		t.Fatal("want the transport error")
	}
	if r.resolved[lvolMoved] != clusterB {
		t.Fatalf("cache = %v, want B kept across a transport error", r.resolved)
	}
}

func TestResolverNamedClusterGoneFromSecret(t *testing.T) {
	// The named cluster was removed from the secret after the volume left it.
	queries := map[string]int{}
	r := testResolver(map[string]error{clusterB: nil}, queries)

	var info *controlplane.NodeInfo
	_, id, err := r.lookup(context.Background(), clusterA, lvolMoved, nodeInfoQuery(&info))
	if err != nil || id != clusterB {
		t.Fatalf("lookup = (%s, %v), want B", id, err)
	}
}

func TestIsVolumeNotFound(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"typed":       {notInPool, true},
		"raw 404":     {&controlplane.HTTPError{StatusCode: http.StatusNotFound}, true},
		"wrapped 404": {fmt.Errorf("x: %w", &controlplane.HTTPError{StatusCode: http.StatusNotFound}), true},
		"500":         {&controlplane.HTTPError{StatusCode: http.StatusInternalServerError}, false},
		"transport":   {errors.New("connection reset by peer"), false},
		"nil":         {nil, false},
	}
	for name, tc := range cases {
		if got := isVolumeNotFound(tc.err); got != tc.want {
			t.Errorf("%s: isVolumeNotFound = %v, want %v", name, got, tc.want)
		}
	}
}
