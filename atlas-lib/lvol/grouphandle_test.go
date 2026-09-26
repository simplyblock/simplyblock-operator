package lvol

import "testing"

func TestParseGroupHandle(t *testing.T) {
	const (
		cluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
		group   = "a1111111-1111-4111-8111-111111111111"
		pool    = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
		volume  = "b2222222-2222-4222-8222-222222222222"
	)

	t.Run("parses a well-formed group handle", func(t *testing.T) {
		got, ok := ParseGroupHandle(VolumeHandle("cg:" + cluster + ":" + group))
		if !ok {
			t.Fatalf("ParseGroupHandle rejected a well-formed handle")
		}
		if got.ClusterID != cluster || got.GroupID != group {
			t.Fatalf("ParseGroupHandle = %+v, want cluster=%s group=%s", got, cluster, group)
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		if _, ok := ParseGroupHandle(VolumeHandle("  cg:" + cluster + ":" + group + "\n")); !ok {
			t.Fatalf("ParseGroupHandle did not trim whitespace")
		}
	})

	t.Run("round-trips through Handle", func(t *testing.T) {
		h := GroupHandle{ClusterID: cluster, GroupID: group}
		got, ok := ParseGroupHandle(h.Handle())
		if !ok || got != h {
			t.Fatalf("round-trip: got %+v ok=%v, want %+v", got, ok, h)
		}
	})

	reject := []struct {
		name, handle string
	}{
		{"a per-volume handle", cluster + ":" + pool + ":" + volume},
		{"the wrong sentinel", "vg:" + cluster + ":" + group},
		{"a missing sentinel", cluster + ":" + group},
		{"a non-UUID cluster", "cg:not-a-uuid:" + group},
		{"a non-UUID group", "cg:" + cluster + ":not-a-uuid"},
		{"too few segments", "cg:" + cluster},
		{"too many segments", "cg:" + cluster + ":" + group + ":extra"},
		{"empty", ""},
	}
	for _, tc := range reject {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, ok := ParseGroupHandle(VolumeHandle(tc.handle)); ok {
				t.Fatalf("ParseGroupHandle accepted %q", tc.handle)
			}
		})
	}
}

// The two grammars must not overlap: a per-volume handle is never a group
// handle, and a group handle is never a per-volume handle, so the routing branch
// can tell them apart.
func TestGroupAndVolumeHandlesAreDisjoint(t *testing.T) {
	const (
		cluster = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
		group   = "a1111111-1111-4111-8111-111111111111"
		pool    = "df34f16c-1a2b-3c4d-5e6f-7a8b9c0d1e2f"
		volume  = "b2222222-2222-4222-8222-222222222222"
	)
	groupHandle := VolumeHandle("cg:" + cluster + ":" + group)
	volumeHandle := VolumeHandle(cluster + ":" + pool + ":" + volume)

	if _, ok := ParseHandle(groupHandle); ok {
		t.Errorf("ParseHandle accepted a group handle %q", groupHandle)
	}
	if !IsGroupHandle(groupHandle) {
		t.Errorf("IsGroupHandle(%q) = false, want true", groupHandle)
	}
	if IsGroupHandle(volumeHandle) {
		t.Errorf("IsGroupHandle(%q) = true, want false", volumeHandle)
	}
}
