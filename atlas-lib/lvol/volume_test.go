package lvol

import "testing"

func TestVolumeHandleSplit(t *testing.T) {
	const (
		cs = "11111111-1111-1111-1111-111111111111"
		ps = "22222222-2222-2222-2222-222222222222"
		vs = "33333333-3333-3333-3333-333333333333"
	)
	c, p, v, err := VolumeHandle(cs + ":" + ps + ":" + vs).Split()
	if err != nil {
		t.Fatal(err)
	}
	if c.String() != cs || p.String() != ps || v.String() != vs {
		t.Errorf("Split = %s, %s, %s", c, p, v)
	}

	bad := []VolumeHandle{
		"",                          // empty
		"only-one",                  // no separators
		VolumeHandle(cs),            // one uuid
		VolumeHandle(cs + ":" + ps), // two parts
		VolumeHandle(cs + ":" + ps + ":" + vs + ":extra"), // four parts
		"x:y:z",                      // three parts, not UUIDs
		VolumeHandle(cs + "::" + vs), // empty middle
	}
	for _, h := range bad {
		if _, _, _, err := h.Split(); err == nil {
			t.Errorf("Split(%q) = nil error, want error", h)
		}
	}
}
