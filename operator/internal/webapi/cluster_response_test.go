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

// Volume migrations make the control plane report is_re_balancing; a drain
// must look at is_data_rebalancing, and fall back to the old flag only when
// the control plane does not report the new one.
func TestIsDataRebalancing(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"own migrations only", `{"id":"c","is_re_balancing":true,"is_data_rebalancing":false}`, false},
		{"data rebalancing", `{"id":"c","is_re_balancing":true,"is_data_rebalancing":true}`, true},
		{"older control plane, rebalancing", `{"id":"c","is_re_balancing":true}`, true},
		{"older control plane, idle", `{"id":"c","is_re_balancing":false}`, false},
	}
	for _, tc := range cases {
		resp, err := ParseClusterResponse([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := resp.IsDataRebalancing(); got != tc.want {
			t.Errorf("%s: IsDataRebalancing = %v, want %v", tc.name, got, tc.want)
		}
	}
}
