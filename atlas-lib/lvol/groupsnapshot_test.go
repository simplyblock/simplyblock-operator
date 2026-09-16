// Round-trip and rejection cases for the group-snapshot handle grammar.
package lvol

import "testing"

const (
	gsCluster = "11111111-1111-1111-1111-111111111111"
	gsGroup   = "22222222-2222-2222-2222-222222222222"
)

func TestGroupSnapshotHandleRoundTrip(t *testing.T) {
	in := NewGroupSnapshotHandle(gsCluster, "pool-1", gsGroup, 7)
	out, ok := ParseGroupSnapshotHandle(in.String())
	if !ok {
		t.Fatalf("round trip did not parse: %q", in.String())
	}
	if out != in {
		t.Fatalf("round trip changed the handle: in=%+v out=%+v", in, out)
	}
}

func TestParseGroupSnapshotHandleTrimsWhitespace(t *testing.T) {
	raw := "  " + gsCluster + ":pool-1:" + gsGroup + ":3\n"
	h, ok := ParseGroupSnapshotHandle(raw)
	if !ok || h.Seq != 3 {
		t.Fatalf("whitespace-wrapped handle rejected: ok=%v h=%+v", ok, h)
	}
}

func TestParseGroupSnapshotHandleRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"three segments":      gsCluster + ":pool-1:" + gsGroup,
		"five segments":       gsCluster + ":pool-1:" + gsGroup + ":1:extra",
		"non-numeric seq":     gsCluster + ":pool-1:" + gsGroup + ":one",
		"negative seq":        gsCluster + ":pool-1:" + gsGroup + ":-1",
		"empty pool":          gsCluster + "::" + gsGroup + ":1",
		"non-UUID cluster":    "not-a-uuid:pool-1:" + gsGroup + ":1",
		"non-UUID group":      gsCluster + ":pool-1:not-a-uuid:1",
		"empty string":        "",
		"plain volume handle": gsCluster + ":pool-1:" + gsGroup,
	}
	for name, raw := range cases {
		if _, ok := ParseGroupSnapshotHandle(raw); ok {
			t.Errorf("%s: parsed %q, want rejection", name, raw)
		}
	}
}
