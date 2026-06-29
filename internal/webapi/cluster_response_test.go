package webapi

import "testing"

func TestParseClusterResponse(t *testing.T) {
	resp, err := ParseClusterResponse([]byte(`{
		"id":"cluster-dto-uuid",
		"name":"cluster-dto",
		"secret":"cluster-dto-secret",
		"nqn":"nqn.2026-04.io.simplyblock:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
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
