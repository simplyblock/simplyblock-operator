package webapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// routingServer serves fixed JSON bodies keyed by request path, so a test can
// stand in for the control plane without the openapi spec mock.
func routingServer(t *testing.T, routes map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

func TestResolveGroupPolicyIDMatchesByPlacement(t *testing.T) {
	// Two consistency-group policies plus a non-group one; only the policy whose
	// placement matches the group's lvs/node is the group's.
	c := routingServer(t, map[string]string{
		"/api/v2/clusters/C/replication/policies/": `[
			{"id":"p-other","consistency_group":true,"group_lvs_name":"lvs-x","group_node_id":"node-x"},
			{"id":"p-plain","consistency_group":false,"group_lvs_name":"","group_node_id":""},
			{"id":"p-ours","consistency_group":true,"group_lvs_name":"lvs-a","group_node_id":"node-a"}
		]`,
	})
	id, err := c.ResolveGroupPolicyID(context.Background(), "C", "lvs-a", "node-a")
	if err != nil {
		t.Fatalf("ResolveGroupPolicyID: %v", err)
	}
	if id != "p-ours" {
		t.Errorf("policy id = %q, want p-ours", id)
	}
}

func TestResolveGroupPolicyIDNoMatchReturnsEmpty(t *testing.T) {
	c := routingServer(t, map[string]string{
		"/api/v2/clusters/C/replication/policies/": `[
			{"id":"p-other","consistency_group":true,"group_lvs_name":"lvs-x","group_node_id":"node-x"}
		]`,
	})
	id, err := c.ResolveGroupPolicyID(context.Background(), "C", "lvs-a", "node-a")
	if err != nil {
		t.Fatalf("ResolveGroupPolicyID: %v", err)
	}
	if id != "" {
		t.Errorf("policy id = %q, want empty for no match", id)
	}
}

func TestLatestReplicatedGenerationReturnsPerMemberSnapshots(t *testing.T) {
	c := routingServer(t, map[string]string{
		"/api/v2/clusters/C/replication/policies/P/latest-generation": `{
			"group_seq": 7,
			"members": [
				{"snapshot_id":"s1","cluster_id":"B","pool_id":"pb","lvol_id":"t1","size":1073741824,"group_seq":7},
				{"snapshot_id":"s2","cluster_id":"B","pool_id":"pb","lvol_id":"t2","size":1073741824,"group_seq":7}
			]
		}`,
	})
	seq, members, found, err := c.LatestReplicatedGeneration(context.Background(), "C", "P")
	if err != nil {
		t.Fatalf("LatestReplicatedGeneration: %v", err)
	}
	if !found {
		t.Fatalf("found = false, want true when a generation exists")
	}
	if seq != 7 {
		t.Errorf("group_seq = %d, want 7", seq)
	}
	if len(members) != 2 || members[0].SnapshotID != "s1" || members[1].SnapshotID != "s2" {
		t.Errorf("members = %+v, want two with s1,s2", members)
	}
	if members[0].ClusterID != "B" || members[0].PoolID != "pb" || members[0].Size != 1073741824 {
		t.Errorf("member[0] = %+v, want the target cluster/pool/size", members[0])
	}
}

func TestLatestReplicatedGenerationNotFoundWhenNothingReplicated(t *testing.T) {
	// No route registered -> 404 -> found=false, no error (nothing has replicated).
	c := routingServer(t, map[string]string{})
	_, _, found, err := c.LatestReplicatedGeneration(context.Background(), "C", "P")
	if err != nil {
		t.Fatalf("LatestReplicatedGeneration on 404: %v", err)
	}
	if found {
		t.Errorf("found = true, want false on 404")
	}
}

func TestResolveMemberVolumesMapsLvolIDsToPVCNames(t *testing.T) {
	// Two pools; the two members live in different pools. Resolution must find
	// both and carry their pvc name, namespace, pool, and size.
	c := routingServer(t, map[string]string{
		"/api/v2/clusters/C/storage-pools/": `[{"id":"pool-1"},{"id":"pool-2"}]`,
		"/api/v2/clusters/C/storage-pools/pool-1/volumes": `[
			{"id":"lvol-a","pvc_name":"data-1","namespace":"app","pool_id":"pool-1","size":1073741824},
			{"id":"lvol-x","pvc_name":"other","namespace":"app","pool_id":"pool-1","size":1073741824}
		]`,
		"/api/v2/clusters/C/storage-pools/pool-2/volumes": `[
			{"id":"lvol-b","pvc_name":"data-2","namespace":"app","pool_id":"pool-2","size":1073741824}
		]`,
	})
	got, err := c.ResolveMemberVolumes(context.Background(), "C", []string{"lvol-a", "lvol-b"})
	if err != nil {
		t.Fatalf("ResolveMemberVolumes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved %d members, want 2: %+v", len(got), got)
	}
	if got["lvol-a"].PVCName != "data-1" || got["lvol-a"].Namespace != "app" || got["lvol-a"].PoolID != "pool-1" {
		t.Errorf("lvol-a = %+v, want data-1/app/pool-1", got["lvol-a"])
	}
	if got["lvol-b"].PVCName != "data-2" || got["lvol-b"].PoolID != "pool-2" {
		t.Errorf("lvol-b = %+v, want data-2/pool-2", got["lvol-b"])
	}
	if _, ok := got["lvol-x"]; ok {
		t.Errorf("resolved an unrequested member lvol-x")
	}
}
