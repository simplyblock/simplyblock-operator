package webapi

import "testing"

func TestParseClusterResponseSupportsLegacyAndDTOShapes(t *testing.T) {
	t.Run("legacy wrapped create_first payload", func(t *testing.T) {
		resp, err := ParseClusterResponse([]byte(`{
			"results":{
				"uuid":"cluster-legacy-uuid",
				"secret":"cluster-legacy-secret",
				"nqn":"nqn.2026-04.io.simplyblock:legacy",
				"distr_ndcs":2,
				"distr_npcs":1,
				"is_re_balancing":true,
				"status":"active"
			}
		}`))
		if err != nil {
			t.Fatalf("ParseClusterResponse returned error: %v", err)
		}
		if resp.UUID != "cluster-legacy-uuid" || resp.Secret != "cluster-legacy-secret" {
			t.Fatalf("unexpected legacy identity fields: %#v", resp)
		}
		if resp.NDCS != 2 || resp.NPCS != 1 || !resp.Rebalancing {
			t.Fatalf("unexpected legacy coding fields: %#v", resp)
		}
	})

	t.Run("v2 dto payload", func(t *testing.T) {
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
			t.Fatalf("unexpected dto identity fields: %#v", resp)
		}
		if resp.NDCS != 3 || resp.NPCS != 1 || resp.Rebalancing {
			t.Fatalf("unexpected dto coding fields: %#v", resp)
		}
	})
}

func TestParseClusterResponseRejectsMissingIdentity(t *testing.T) {
	if _, err := ParseClusterResponse([]byte(`{"status":"active"}`)); err == nil {
		t.Fatalf("expected ParseClusterResponse to reject payloads without id/uuid")
	}
}
