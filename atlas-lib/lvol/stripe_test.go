package lvol

import "testing"

func TestParseStripedHandle(t *testing.T) {
	const (
		cluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
		pool    = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
		volume  = "a1111111-1111-4111-8111-111111111111"
		plain   = cluster + ":" + pool + ":" + volume
	)

	ok := []struct {
		name   string
		handle string
		width  int
	}{
		{"a plain handle is width 1", plain, 1},
		{"a plain handle with a pool name", cluster + ":my-pool:" + volume, 1},
		{"two members", plain + ":s2", 2},
		{"twelve members", plain + ":s12", 12},
		{"LVM's ceiling", plain + ":s128", MaxStripeWidth},
		{"surrounding whitespace", "  " + plain + ":s3\n", 3},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, width, valid := ParseStripedHandle(VolumeHandle(tc.handle))
			if !valid {
				t.Fatalf("ParseStripedHandle(%q) = not ok, want ok", tc.handle)
			}
			if width != tc.width {
				t.Errorf("width = %d, want %d", width, tc.width)
			}
			if got.ClusterID != cluster || got.VolumeID != volume {
				t.Errorf("handle = %+v, want the first member's", got)
			}
		})
	}

	bad := []struct{ name, handle string }{
		{"empty", ""},
		{"width 1 is not a stripe", plain + ":s1"},
		{"width 0", plain + ":s0"},
		{"above LVM's ceiling", plain + ":s129"},
		{"leading zero", plain + ":s03"},
		{"no digits", plain + ":s"},
		{"no prefix", plain + ":3"},
		{"a sign", plain + ":s+3"},
		{"trailing junk", plain + ":s3x"},
		{"five segments", plain + ":s3:s3"},
		{"a bad first member", cluster + ":" + pool + ":not-a-uuid:s3"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, width, valid := ParseStripedHandle(VolumeHandle(tc.handle)); valid {
				t.Errorf("ParseStripedHandle(%q) = %+v, %d, ok; want not ok", tc.handle, got, width)
			}
		})
	}
}

// TestParseHandleStillRefusesTheMarker pins the reason the marker lives in a
// function of its own: a consumer that does not know about striping must not
// read a striped handle as its first member.
func TestParseHandleStillRefusesTheMarker(t *testing.T) {
	const h = "8ffac363-0c46-4714-a71b-f9c0b58a1269:pool:a1111111-1111-4111-8111-111111111111:s3"
	if got, ok := ParseHandle(h); ok {
		t.Errorf("ParseHandle(%q) = %+v, ok; want not ok", h, got)
	}
}

func TestSplitStripeMarkerLeavesLegacyIdentifiersAlone(t *testing.T) {
	// A legacy two-segment snapshot id and a three-segment one whose last
	// segment happens to look like a marker are both returned unchanged.
	for _, id := range []string{"cluster:snap", "cluster:pool:s3", "cluster:pool:snap"} {
		base, width, ok := SplitStripeMarker(id)
		if !ok || base != id || width != 1 {
			t.Errorf("SplitStripeMarker(%q) = %q, %d, %v; want it unchanged at width 1", id, base, width, ok)
		}
	}
}

func TestStripeMarkerRoundTrips(t *testing.T) {
	h := Handle{
		ClusterID: "8ffac363-0c46-4714-a71b-f9c0b58a1269",
		PoolRef:   "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f",
		VolumeID:  "a1111111-1111-4111-8111-111111111111",
	}
	if got := h.StripedString(1); got != h.String() {
		t.Errorf("StripedString(1) = %q, want the plain handle %q", got, h.String())
	}
	s := h.StripedString(4)
	if len(s) > 128 {
		t.Errorf("a striped handle of UUIDs is %d bytes, over the CSI limit of 128", len(s))
	}
	got, width, ok := ParseStripedHandle(VolumeHandle(s))
	if !ok || got != h || width != 4 {
		t.Errorf("ParseStripedHandle(%q) = %+v, %d, %v; want %+v, 4", s, got, width, ok, h)
	}
}
