package kube

import "testing"

func TestParams(t *testing.T) {
	p := map[string]string{"i": "5", "f": "1.5", "b": "True", "s": "x", "bad": "nope", "empty": ""}

	if got := StringParam(p, "s", "d"); got != "x" {
		t.Errorf("StringParam present = %q, want x", got)
	}
	if got := StringParam(p, "empty", "d"); got != "d" {
		t.Errorf("StringParam empty = %q, want d (default)", got)
	}
	if got := StringParam(p, "missing", "d"); got != "d" {
		t.Errorf("StringParam missing = %q, want d", got)
	}

	if n, err := IntParam(p, "i", 0); err != nil || n != 5 {
		t.Errorf("IntParam(i) = %d, %v; want 5, nil", n, err)
	}
	if n, err := IntParam(p, "missing", 9); err != nil || n != 9 {
		t.Errorf("IntParam(missing) = %d, %v; want 9, nil", n, err)
	}
	if n, err := IntParam(p, "bad", 9); err == nil || n != 9 {
		t.Errorf("IntParam(bad) = %d, %v; want default 9 + error", n, err)
	}

	if f, err := FloatParam(p, "f", 0); err != nil || f != 1.5 {
		t.Errorf("FloatParam(f) = %v, %v; want 1.5, nil", f, err)
	}
	if b, err := BoolParam(p, "b", false); err != nil || !b {
		t.Errorf("BoolParam(b) = %v, %v; want true, nil", b, err)
	}
	if b, err := BoolParam(p, "missing", true); err != nil || !b {
		t.Errorf("BoolParam(missing) = %v, %v; want true (default), nil", b, err)
	}
}
