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
		VolumeHandle(cs),            // one UUID
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

// TestVolumeHandleSplitTrimsSurroundingWhitespace covers handles that arrive
// with whitespace around them. A CSI volume handle is read back out of a
// PersistentVolume, and a PV is a YAML document a human may have written or
// edited, so a trailing newline or an indent-induced space is a property of
// how the value was transported and not of the identity it names.
func TestVolumeHandleSplitTrimsSurroundingWhitespace(t *testing.T) {
	const (
		cs = "11111111-1111-1111-1111-111111111111"
		ps = "22222222-2222-2222-2222-222222222222"
		vs = "33333333-3333-3333-3333-333333333333"
	)
	handle := cs + ":" + ps + ":" + vs
	for _, spelling := range []string{
		" " + handle,
		handle + " ",
		"  " + handle + "  ",
		handle + "\n",
		"\t" + handle + "\r\n",
	} {
		c, p, v, err := VolumeHandle(spelling).Split()
		if err != nil {
			t.Errorf("Split(%q) = %v, want the handle it wraps", spelling, err)
			continue
		}
		if c.String() != cs || p.String() != ps || v.String() != vs {
			t.Errorf("Split(%q) = %s, %s, %s", spelling, c, p, v)
		}
	}

	// Whitespace inside the handle is not transport, it is a malformed value.
	for _, spelling := range []string{
		cs + " :" + ps + ":" + vs,
		cs + ": " + ps + ":" + vs,
		cs + ":" + ps + ":" + vs[:8] + " " + vs[9:],
	} {
		if _, _, _, err := VolumeHandle(spelling).Split(); err == nil {
			t.Errorf("Split(%q) = nil error, want error", spelling)
		}
	}
}

// NewVolumeHandle is Split's inverse, so a round trip must be the identity for
// every well-formed handle.
func TestNewVolumeHandleRoundTrips(t *testing.T) {
	const (
		cs = "11111111-1111-1111-1111-111111111111"
		ps = "22222222-2222-2222-2222-222222222222"
		vs = "33333333-3333-3333-3333-333333333333"
	)
	h := NewVolumeHandle(cs, ps, vs)
	if string(h) != cs+":"+ps+":"+vs {
		t.Fatalf("NewVolumeHandle() = %q, want the three ids colon-separated", h)
	}
	c, p, v, err := h.Split()
	if err != nil {
		t.Fatalf("Split() on a constructed handle: %v", err)
	}
	if c.String() != cs || p.String() != ps || v.String() != vs {
		t.Errorf("round trip = %s/%s/%s, want %s/%s/%s", c, p, v, cs, ps, vs)
	}
}
