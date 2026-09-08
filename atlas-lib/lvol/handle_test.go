package lvol

import "testing"

func TestParseHandle(t *testing.T) {
	const (
		cluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
		pool    = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
		volume  = "a1111111-1111-4111-8111-111111111111"
	)

	ok := []struct {
		name                         string
		handle                       string
		clusterID, poolRef, volumeID string
	}{
		{"all three UUIDs", cluster + ":" + pool + ":" + volume, cluster, pool, volume},
		{"pool is a name", cluster + ":my-pool:" + volume, cluster, "my-pool", volume},
		{"pool name with a dash", cluster + ":pool-1:" + volume, cluster, "pool-1", volume},
		{
			"surrounding whitespace is transport",
			"  " + cluster + ":" + pool + ":" + volume + "\n", cluster, pool, volume,
		},
		{
			"uppercase is preserved as written",
			"8FFAC363-0C46-4714-A71B-F9C0B58A1269:" + pool + ":" + volume,
			"8FFAC363-0C46-4714-A71B-F9C0B58A1269", pool, volume,
		},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := ParseHandle(VolumeHandle(tc.handle))
			if !valid {
				t.Fatalf("ParseHandle(%q) = not ok, want ok", tc.handle)
			}
			if got.ClusterID != tc.clusterID || got.PoolRef != tc.poolRef || got.VolumeID != tc.volumeID {
				t.Errorf("ParseHandle(%q) = %+v, want %s / %s / %s",
					tc.handle, got, tc.clusterID, tc.poolRef, tc.volumeID)
			}
		})
	}

	bad := []struct{ name, handle string }{
		{"empty", ""},
		{"no separators", "only-one"},
		{"two parts", cluster + ":" + pool},
		{"four parts", cluster + ":" + pool + ":" + volume + ":extra"},
		{"cluster is not a UUID", "not-a-uuid:" + pool + ":" + volume},
		{"volume is not a UUID", cluster + ":" + pool + ":not-a-uuid"},
		{"empty pool", cluster + "::" + volume},
		{"empty cluster", ":" + pool + ":" + volume},
		{"empty volume", cluster + ":" + pool + ":"},
		// A handle is compared against control-plane and sysfs identifiers as a
		// string, so a UUID spelling that is valid but not canonical would parse
		// here and then silently fail to match anywhere else.
		{"braced cluster", "{" + cluster + "}:" + pool + ":" + volume},
		{"cluster without dashes", "8ffac3630c464714a71bf9c0b58a1269:" + pool + ":" + volume},
		{"whitespace inside", cluster + " :" + pool + ":" + volume},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, valid := ParseHandle(VolumeHandle(tc.handle)); valid {
				t.Errorf("ParseHandle(%q) = %+v, ok; want not ok", tc.handle, got)
			}
		})
	}
}

func TestHandleStringRoundTrips(t *testing.T) {
	const handle = "8ffac363-0c46-4714-a71b-f9c0b58a1269:my-pool:a1111111-1111-4111-8111-111111111111"
	parsed, ok := ParseHandle(handle)
	if !ok {
		t.Fatalf("ParseHandle(%q) = not ok", handle)
	}
	if got := parsed.String(); got != handle {
		t.Errorf("Handle.String() = %q, want %q", got, handle)
	}
}

func TestIsCanonicalUUID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"lowercase", "8ffac363-0c46-4714-a71b-f9c0b58a1269", true},
		{"uppercase", "8FFAC363-0C46-4714-A71B-F9C0B58A1269", true},
		{"mixed case", "8ffAC363-0c46-4714-A71b-f9C0b58a1269", true},
		{"empty", "", false},
		{"pool name", "my-pool", false},
		{"missing hyphens", "8ffac3630c464714a71bf9c0b58a1269", false},
		{"braced", "{8ffac363-0c46-4714-a71b-f9c0b58a1269}", false},
		{"urn form", "urn:uuid:8ffac363-0c46-4714-a71b-f9c0b58a1269", false},
		{"too short", "8ffac363-0c46-4714-a71b-f9c0b58a126", false},
		{"too long", "8ffac363-0c46-4714-a71b-f9c0b58a12690", false},
		{"non-hex digit", "8ffac363-0c46-4714-a71b-f9c0b58a126g", false},
		{"hyphens misplaced", "8ffac3630-c46-4714-a71b-f9c0b58a1269", false},
		{"leading space", " 8ffac363-0c46-4714-a71b-f9c0b58a1269", false},
		{"trailing space", "8ffac363-0c46-4714-a71b-f9c0b58a1269 ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCanonicalUUID(tc.in); got != tc.want {
				t.Fatalf("IsCanonicalUUID(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
