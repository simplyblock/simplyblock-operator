package controlplane

// The sync-replication verbs of a stretched cluster: where a volume is led
// from, the demote on the site it leaves, and the promote on the site it moves
// to, each naming its site.

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// The status is read with no site, which the backend answers for the site the
// volume is led from.
func TestClientSyncStatusReadsTheLedSite(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("site") {
			t.Errorf("sync-status sent site %q; it must ask for the led site", r.URL.Query().Get("site"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"site":"site-b","role":"primary","state":"in_sync","peer_ready":true,"diverged":false,
			"completed":true,"degraded":false,"resyncing":false,"bytes_behind":0}`))
	})
	st, err := c.SyncStatus(context.Background(), testHandle)
	if err != nil {
		t.Fatal(err)
	}
	if st.Site != "site-b" || st.Role != "primary" || st.State != "in_sync" || !st.PeerReady {
		t.Errorf("status = %+v", st)
	}
}

// A demote names the site the volume leaves.
func TestClientSyncDemoteNamesTheSite(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("site"); got != "site-a" {
			t.Errorf("demote site = %q, want site-a", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.SyncDemoteVolume(context.Background(), testHandle, "site-a"); err != nil {
		t.Fatal(err)
	}
}

// A demote the backend refuses because the store still serves other volumes on
// the old site names them.
func TestClientSyncDemoteRefusalNamesTheBlockingVolumes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":{"message":"volumes still served","volumes":["v-1","v-2"]}}`))
	})
	err := c.SyncDemoteVolume(context.Background(), testHandle, "site-a")
	var refused *SyncRefusedError
	if !errors.As(err, &refused) || len(refused.Volumes) != 2 {
		t.Fatalf("err = %v, want a SyncRefusedError naming two volumes", err)
	}
}

// A promote names the site and whether it is planned; 200 is done.
func TestClientSyncPromoteIsPlannedOnTheTargetSite(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("site") != "site-b" || q.Get("planned") != "true" {
			t.Errorf("promote query = %v, want site=site-b planned=true", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lvol_id":"6e2b9f4a-1c3d-4e5f-8a9b-0c1d2e3f4a5b","connection_strings":[]}`))
	})
	res, err := c.SyncPromoteVolume(context.Background(), testHandle, "site-b", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.InProgress {
		t.Error("a 200 promote reported in progress")
	}
}

// A promote still running is in progress, not a refusal.
func TestClientSyncPromoteInProgressIsNotARefusal(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":{"message":"promote in progress; call again","task_id":"t-1"}}`))
	})
	res, err := c.SyncPromoteVolume(context.Background(), testHandle, "site-b", true)
	if err != nil {
		t.Fatalf("an in-progress promote is an error: %v", err)
	}
	if !res.InProgress {
		t.Error("a promote still running was not reported in progress")
	}
}

// A planned promote whose other site is not online is the 412 a caller
// escalates on, distinct from a refusal.
func TestClientSyncPromoteReportsAPeerSiteOffline(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"detail":{"message":"site site-a is not online"}}`))
	})
	_, err := c.SyncPromoteVolume(context.Background(), testHandle, "site-b", true)
	if !errors.Is(err, ErrSyncPeerSiteOffline) {
		t.Errorf("err = %v, want ErrSyncPeerSiteOffline", err)
	}
}

// The node listing a fence resolves its CIDRs through: every node's management
// address and site.
func TestClientSiteNodesReadsEachNodesSite(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"n-1","mgmt_ip":"192.0.2.1","site":"site-a"},{"id":"n-2","mgmt_ip":"192.0.2.2","site":"site-b"}]`))
	})
	nodes, err := c.SiteNodes(context.Background(), testSiteCluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].MgmtIP != "192.0.2.1" || nodes[1].Site != "site-b" {
		t.Errorf("nodes = %+v", nodes)
	}
}

const testSiteCluster = "11111111-1111-1111-1111-111111111111"

// A site fence the control plane is still running answers 202 with its task:
// not done, and not an error.
func TestClientFenceSiteStillRunningIsNotDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/clusters/"+testSiteCluster+"/sites/site-a/fence" {
			t.Errorf("fence went to %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":"t-1"}`))
	})
	done, err := c.FenceSite(context.Background(), testSiteCluster, "site-a")
	if err != nil || done {
		t.Errorf("FenceSite = %v, %v; want not done, no error", done, err)
	}
}

// A 200 is the site recorded fenced.
func TestClientFenceSiteRecordedIsDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"site":"site-a","lost":true,"state":"done","fence_task_id":null,"nodes":[]}`))
	})
	done, err := c.FenceSite(context.Background(), testSiteCluster, "site-a")
	if err != nil || !done {
		t.Errorf("FenceSite = %v, %v; want done", done, err)
	}
}

// A fence the control plane refuses, for a node of the site still answering
// among other reasons, is a refusal the caller reports rather than retries.
func TestClientFenceSiteRefusalIsASyncRefusal(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":{"message":"node a-1 of site site-a is online","task_id":null}}`))
	})
	_, err := c.FenceSite(context.Background(), testSiteCluster, "site-a")
	var refused *SyncRefusedError
	if !errors.As(err, &refused) || refused.Message == "" {
		t.Errorf("err = %v, want a SyncRefusedError with its message", err)
	}
}

// An unfence is a confirmation: 200 once the site is no longer recorded, 409
// while it still is, and neither is an error.
func TestClientUnfenceSiteConfirmsTheRecordIsCleared(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{{http.StatusOK, true}, {http.StatusConflict, false}} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v2/clusters/"+testSiteCluster+"/sites/site-a/unfence" {
				t.Errorf("unfence went to %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(`{"detail":{"message":"site site-a is recorded down"}}`))
		})
		cleared, err := c.UnfenceSite(context.Background(), testSiteCluster, "site-a")
		if err != nil || cleared != tc.want {
			t.Errorf("%d: UnfenceSite = %v, %v; want %v, no error", tc.code, cleared, err, tc.want)
		}
	}
}
