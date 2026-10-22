// Unit tests for the namespaced reference helpers.

package utils

import "testing"

func TestSplitRef(t *testing.T) {
	cases := []struct{ ref, def, ns, name string }{
		{"pol", "apps", "apps", "pol"},
		{"simplyblock/pol", "apps", "simplyblock", "pol"},
	}
	for _, c := range cases {
		if ns, name := SplitRef(c.ref, c.def); ns != c.ns || name != c.name {
			t.Errorf("SplitRef(%q, %q) = %s/%s, want %s/%s", c.ref, c.def, ns, name, c.ns, c.name)
		}
	}
}

func TestCanonicalRefMakesSpellingsEqual(t *testing.T) {
	if CanonicalRef("pol", "simplyblock") != CanonicalRef("simplyblock/pol", "apps") {
		t.Errorf("a bare and a qualified reference to the same policy differ")
	}
}

func TestRefFromIsBareWithinANamespace(t *testing.T) {
	if got := RefFrom("simplyblock", "pol", "simplyblock"); got != "pol" {
		t.Errorf("same namespace = %q, want pol", got)
	}
	if got := RefFrom("simplyblock", "pol", "apps"); got != "simplyblock/pol" {
		t.Errorf("other namespace = %q, want simplyblock/pol", got)
	}
}
