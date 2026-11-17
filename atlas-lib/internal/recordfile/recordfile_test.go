// Tests for the record-file naming rule both node-side record stores share.
package recordfile

import "testing"

func TestPath(t *testing.T) {
	for key, want := range map[string]string{
		"c:p:v":      "/r/c_p_v.json",
		"c:p:v:s2":   "/r/c_p_v_s2.json",
		"../../etc":  "/r/.._.._etc.json",
		"..":         "/r/_.json",
		"":           "/r/_.json",
		"a/b":        "/r/a_b.json",
		"vol-1.data": "/r/vol-1.data.json",
	} {
		if got := Path("/r", key); got != want {
			t.Errorf("Path(%q) = %q, want %q", key, got, want)
		}
	}
}
