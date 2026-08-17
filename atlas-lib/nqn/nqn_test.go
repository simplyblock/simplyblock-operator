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

// The host NQN is what an access-controlled pool authorizes: the operator
// registers it in allowed_hosts and the CSI driver presents it on connect, so
// the two must render byte-identically from the same node UID.
func TestHost(t *testing.T) {
	const nodeUID = "416db8c3-1f3a-4b2e-9a77-1b0d5e6c8f21"
	want := "nqn.2014-08.io.simplyblock:uuid:" + nodeUID
	if got := Host(nodeUID); got != want {
		t.Errorf("Host = %q, want %q", got, want)
	}
	// The host prefix is not the subsystem one; deriving either from the other
	// would authorize the wrong name.
	if HostPrefix == DefaultPrefix {
		t.Error("HostPrefix == DefaultPrefix, want the host and subsystem prefixes distinct")
	}
}

func TestHostUUID(t *testing.T) {
	const nodeUID = "416db8c3-1f3a-4b2e-9a77-1b0d5e6c8f21"
	for _, tc := range []struct {
		nqn  string
		want string
	}{
		// Round-trips what Host built, and reads the NVMe-spec form too: what a
		// caller needs is the UUID identifying the host, and both forms name it
		// the same way.
		{Host(nodeUID), nodeUID},
		{"nqn.2014-08.org.nvmexpress:uuid:" + nodeUID, nodeUID},
		// A host NQN need not carry a UUID at all, and none of these do.
		{"", ""},
		{"nqn.2014-08.org.nvmexpress:client-42", ""},
		{"nqn.2014-08.io.simplyblock:uuid:", ""},
		{"nqn.2014-08.io.simplyblock:uuid:not-a-uuid", ""},
		// A subsystem NQN is not a host NQN.
		{Make(testCluster, testLvol), ""},
	} {
		got, ok := HostUUID(tc.nqn)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("HostUUID(%q) = %q/%v, want %q/%v", tc.nqn, got, ok, tc.want, tc.want != "")
		}
	}
}
