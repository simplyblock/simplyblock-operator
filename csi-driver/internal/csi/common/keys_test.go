package csicommon

import "testing"

// TestParseVolumeHandleReadsAStripedHandleAsItsFirstMember holds both forms a
// volume id takes: an ordinary one parses exactly as it always has, and a
// striped one parses to its first member, with the width beside it.
func TestParseVolumeHandleReadsAStripedHandleAsItsFirstMember(t *testing.T) {
	const (
		cluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
		pool    = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
		volume  = "a1111111-1111-4111-8111-111111111111"
		plain   = cluster + ":" + pool + ":" + volume
	)

	for id, wantWidth := range map[string]int{plain: 1, plain + ":s4": 4} {
		h, err := ParseVolumeHandle(id)
		if err != nil {
			t.Fatalf("ParseVolumeHandle(%q): %v", id, err)
		}
		if h.ClusterID != cluster || h.PoolRef != pool || h.VolumeID != volume {
			t.Errorf("ParseVolumeHandle(%q) = %+v, want the first member", id, h)
		}
		if got := StripeWidth(id); got != wantWidth {
			t.Errorf("StripeWidth(%q) = %d, want %d", id, got, wantWidth)
		}
	}

	for _, id := range []string{plain + ":s1", plain + ":x", plain + ":s4:s4", "not-a-handle"} {
		if _, err := ParseVolumeHandle(id); err == nil {
			t.Errorf("ParseVolumeHandle(%q) accepted a malformed id", id)
		}
		if got := StripeWidth(id); got != 1 {
			t.Errorf("StripeWidth(%q) = %d, want 1 for an id that does not parse", id, got)
		}
	}
}
