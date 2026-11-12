// Tests for the group replication verbs in groupreplication.go, against a
// test server standing in for the control plane's v2 API.
package controlplane

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

const (
	localCluster  = "aaaaaaaa-0000-0000-0000-000000000001"
	sourceCluster = "bbbbbbbb-0000-0000-0000-000000000002"
	sourceGroup   = "cccccccc-0000-0000-0000-000000000003"
)

func TestFailoverReplicatedGroupPostsToTheLocalCluster(t *testing.T) {
	var gotPath, gotMethod string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"members":[]}`))
	})
	gh := lvol.GroupHandle{ClusterID: sourceCluster, GroupID: sourceGroup}

	if err := c.FailoverReplicatedGroup(context.Background(), localCluster, gh); err != nil {
		t.Fatalf("FailoverReplicatedGroup: %v", err)
	}
	want := "/api/v2/clusters/" + localCluster + "/replication/replicated-groups/" +
		sourceCluster + "/" + sourceGroup + "/failover"
	if gotMethod != http.MethodPost || gotPath != want {
		t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, want)
	}
}

func TestFailoverReplicatedGroupReportsAConflict(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"no complete replicated generation"}`))
	})
	gh := lvol.GroupHandle{ClusterID: sourceCluster, GroupID: sourceGroup}

	err := c.FailoverReplicatedGroup(context.Background(), localCluster, gh)
	if err == nil {
		t.Fatal("FailoverReplicatedGroup: want an error for a 409")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict {
		t.Errorf("error %v: want a *StatusError with status 409", err)
	}
}

func TestFailoverReplicatedGroupRefusesALocalClusterThatIsNotAUUID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	gh := lvol.GroupHandle{ClusterID: sourceCluster, GroupID: sourceGroup}

	if err := c.FailoverReplicatedGroup(context.Background(), "site-b", gh); err == nil {
		t.Fatal("FailoverReplicatedGroup: want an error for a cluster id that is not a UUID")
	}
}

func TestResolveReplicatedGroupAsksTheLocalCluster(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cluster_id":"` + sourceCluster + `","group_id":"` + sourceGroup + `",` +
			`"active_cluster_id":"` + localCluster + `","active_group_id":"dddddddd-0000-0000-0000-000000000004",` +
			`"members":[{"origin_handle":"` + sourceCluster + `:p:m1","active_handle":"` + localCluster + `:p:c1"}]}`))
	})
	gh := lvol.GroupHandle{ClusterID: sourceCluster, GroupID: sourceGroup}

	res, err := c.ResolveReplicatedGroup(context.Background(), localCluster, gh)
	if err != nil {
		t.Fatalf("ResolveReplicatedGroup: %v", err)
	}
	want := "/api/v2/clusters/" + localCluster + "/replication/replicated-groups/" +
		sourceCluster + "/" + sourceGroup + "/resolution"
	if gotPath != want {
		t.Errorf("request path %s, want %s", gotPath, want)
	}
	if res.Active == nil || *res.Active != (lvol.GroupHandle{ClusterID: localCluster, GroupID: "dddddddd-0000-0000-0000-000000000004"}) {
		t.Errorf("active = %v, want the group on %s", res.Active, localCluster)
	}
	if len(res.Members) != 1 || res.Members[0].Active != lvol.VolumeHandle(localCluster+":p:c1") {
		t.Errorf("members = %+v", res.Members)
	}
}

func TestResolveReplicatedGroupNotFailedOverHereHasNoActiveGroup(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cluster_id":"` + sourceCluster + `","group_id":"` + sourceGroup + `",` +
			`"active_cluster_id":"","active_group_id":"","members":[]}`))
	})
	gh := lvol.GroupHandle{ClusterID: sourceCluster, GroupID: sourceGroup}

	res, err := c.ResolveReplicatedGroup(context.Background(), localCluster, gh)
	if err != nil {
		t.Fatalf("ResolveReplicatedGroup: %v", err)
	}
	if res.Active != nil {
		t.Errorf("active = %v, want none", *res.Active)
	}
}
