package nqn

import "testing"

const (
	testCluster = "c30a691a-015f-40c1-a7b6-26897264d489"
	testLvol    = "792e184c-43d5-40ba-b497-3b645347cf1d"
)

func TestMake(t *testing.T) {
	want := DefaultPrefix + ":" + testCluster + ":lvol:" + testLvol

	if got := Make(testCluster, testLvol); got != want {
		t.Errorf("Make = %q, want %q", got, want)
	}
	if got := MakeWithPrefix(DefaultPrefix, testCluster, testLvol); got != want {
		t.Errorf("MakeWithPrefix = %q, want %q", got, want)
	}
	// Make is the one-shot string form of Build(...).String().
	if got, s := Make(testCluster, testLvol), Build(testCluster, testLvol); got != s.String() {
		t.Errorf("Make = %q, but Build(...).String() = %q", got, s.String())
	}
}

func TestBuild(t *testing.T) {
	s := Build(testCluster, testLvol)
	if s != (Subsystem{Prefix: DefaultPrefix, ClusterID: testCluster, LvolID: testLvol}) {
		t.Errorf("Build = %+v", s)
	}
	if p := BuildWithPrefix("nqn.custom", testCluster, testLvol); p.Prefix != "nqn.custom" {
		t.Errorf("BuildWithPrefix prefix = %q, want nqn.custom", p.Prefix)
	}
}

func TestParseRoundTrip(t *testing.T) {
	s, ok := Parse(Make(testCluster, testLvol))
	if !ok {
		t.Fatal("Parse failed on a Make output")
	}
	if s.Prefix != DefaultPrefix || s.ClusterID != testCluster || s.LvolID != testLvol {
		t.Errorf("round-trip = %+v", s)
	}

	if _, ok := Parse("nqn.2023-02.io.simplyblock:no-marker-here"); ok {
		t.Error("Parse accepted an NQN without the lvol marker")
	}
}
