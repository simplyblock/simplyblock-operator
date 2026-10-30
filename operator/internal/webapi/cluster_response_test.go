package webapi

import "testing"

func TestParseClusterResponse(t *testing.T) {
	resp, err := ParseClusterResponse([]byte(`{
		"id":"cluster-dto-uuid",
		"name":"cluster-dto",
		"secret":"cluster-dto-secret",
		"nqn":"nqn.2026-04.io.simplyblock:dto",
		"status":"active",
		"is_re_balancing":false,
		"distr_ndcs":3,
		"distr_npcs":1
	}`))
	if err != nil {
		t.Fatalf("ParseClusterResponse returned error: %v", err)
	}
	if resp.UUID != "cluster-dto-uuid" || resp.Secret != "cluster-dto-secret" {
		t.Fatalf("unexpected identity fields: %#v", resp)
	}
	if resp.NDCS != 3 || resp.NPCS != 1 || resp.Rebalancing {
		t.Fatalf("unexpected coding fields: %#v", resp)
	}
}

func TestParseClusterResponseRejectsMissingIdentity(t *testing.T) {
	if _, err := ParseClusterResponse([]byte(`{"status":"active"}`)); err == nil {
		t.Fatalf("expected ParseClusterResponse to reject payloads without id")
	}
}

// Regression: 2026-10-02-cluster-shrinking-reads-degraded: the detail read is the
// fallback before the stream has synced, and it has to carry is_shrinking for the
// same reason the stream does.
func TestParseClusterResponseReadsTheShrinkingFlag(t *testing.T) {
	resp, err := ParseClusterResponse([]byte(`{
		"id":"cluster-dto-uuid",
		"status":"degraded",
		"is_shrinking":true
	}`))
	if err != nil {
		t.Fatalf("ParseClusterResponse returned error: %v", err)
	}
	if !resp.Shrinking {
		t.Errorf("a cluster reporting is_shrinking parsed as not shrinking: %#v", resp)
	}
}

// Regression: 2026-10-22-cluster-response-drops-sync-fields: the parser decoded
// sync_replication, lost_site and lost_site_state and left them out of its
// result, so every cluster read as one without sync replication. A stretched
// follower handed the leader's sync cluster refused to adopt it as "not a
// sync-replication cluster" and its StorageCluster went Unavailable, and the lost
// site never reached status.stretched.
func TestParseClusterResponseReadsTheSyncReplicationFacts(t *testing.T) {
	resp, err := ParseClusterResponse([]byte(`{
		"id": "cluster-1",
		"status": "active",
		"sync_replication": true,
		"lost_site": "site-a",
		"lost_site_state": "fenced"
	}`))
	if err != nil {
		t.Fatalf("ParseClusterResponse returned error: %v", err)
	}
	if !resp.SyncReplication {
		t.Error("SyncReplication = false, want true")
	}
	if resp.LostSite != "site-a" || resp.LostSiteState != "fenced" {
		t.Errorf("LostSite, LostSiteState = %q, %q, want site-a, fenced", resp.LostSite, resp.LostSiteState)
	}
}

// The cluster record names its leader site, which a follower checks at adoption
// and a leader keeps naming itself (design-stretched-sync-replication-ramen.md
// §11.1).
func TestParseClusterResponseReadsTheLeaderSite(t *testing.T) {
	resp, err := ParseClusterResponse([]byte(`{"id": "cluster-1", "sync_replication": true, "leader_site": "site-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.LeaderSite != "site-a" {
		t.Errorf("LeaderSite = %q, want site-a", resp.LeaderSite)
	}
}
