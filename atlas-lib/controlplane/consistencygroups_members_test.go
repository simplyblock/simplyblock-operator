package controlplane

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

// The member listing carries bare lvol ids; the handles need the pool, which
// the group's first member is probed for across the cluster's pools. Here it
// lives in the SECOND pool, and a removed member is left out.
func TestConsistencyGroupMemberHandlesResolvesThePool(t *testing.T) {
	const (
		group     = "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"
		m1        = "a1111111-1111-4111-8111-111111111111"
		m2        = "b2222222-2222-4222-8222-222222222222"
		gone      = "d4444444-4444-4444-8444-444444444444"
		otherPool = "55555555-5555-5555-5555-555555555555"
	)
	pool := func(id, name string) string {
		return `{"id":"` + id + `","cluster_id":"` + testCluster + `","name":"` + name + `",` +
			`"max_size":1000,"capacity":{},"max_r_mbytes":0,"max_rw_iops":0,"max_rw_mbytes":0,` +
			`"max_w_mbytes":0,"volume_max_size":0,"status":"active"}`
	}
	member := func(id string, removed int) string {
		return `{"lvol_id":"` + id + `","joined_seq":1,"removed_seq":` + string(rune('0'+removed)) +
			`,"node_id":"n","lvs_name":"l","online":true}`
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/consistency-groups/"+group+"/members"):
			_, _ = w.Write([]byte("[" + member(m1, 0) + "," + member(gone, 3) + "," + member(m2, 0) + "]"))
		case strings.HasSuffix(r.URL.Path, "/storage-pools/"):
			_, _ = w.Write([]byte("[" + pool(otherPool, "other") + "," + pool(testPool, "pool1") + "]"))
		case strings.Contains(r.URL.Path, "/storage-pools/"+testPool+"/volumes/"+m1):
			_, _ = w.Write([]byte(`{"id":"` + m1 + `","name":"pvc-1","pool_name":"pool1",` +
				`"size":20971520,"ns_id":1,"nqn":"nqn.2023-02.io.simplyblock:c:lvol:v"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"not found"}`))
		}
	})

	got, err := c.ConsistencyGroupMemberHandles(context.Background(),
		lvol.GroupHandle{ClusterID: testCluster, GroupID: group})
	if err != nil {
		t.Fatal(err)
	}
	want := []lvol.VolumeHandle{
		lvol.VolumeHandle(testCluster + ":" + testPool + ":" + m1),
		lvol.VolumeHandle(testCluster + ":" + testPool + ":" + m2),
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("handles = %v, want %v", got, want)
	}
}

func TestConsistencyGroupMemberHandlesOfAnEmptyGroupIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	_, err := c.ConsistencyGroupMemberHandles(context.Background(),
		lvol.GroupHandle{ClusterID: testCluster, GroupID: "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"})
	if err == nil {
		t.Fatal("an empty group answered without an error")
	}
}
