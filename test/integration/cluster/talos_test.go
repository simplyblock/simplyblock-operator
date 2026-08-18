package cluster

import (
	"strings"
	"testing"
)

// The failure this guards against cost an investigation: a two-node spec whose
// cluster name was four characters longer than the smoke test's could not boot a
// single VM, and what the test saw was a one-minute timeout waiting for a network
// bridge. The QEMU error naming the real cause was in a log file inside the
// state directory.
func TestCheckNameFits(t *testing.T) {
	// A real macOS home, since the budget depends on it.
	const state = "/Users/noctarius/.talos/clusters"

	tests := []struct {
		name    string
		cluster string
		wantErr bool
	}{
		{"the name that failed", "sb-integration-98062-cnc", true},
		{"the name that worked", "sb-integration-98062", false},
		{"the short scheme, with a suffix", "sbi-98062-cnc", false},
		// 58 + 2N bytes with this state directory, against a 104-byte limit.
		{"the longest name that fits", strings.Repeat("x", 23), false},
		{"one character too many", strings.Repeat("x", 24), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNameFits(state, tc.cluster)
			if tc.wantErr && err == nil {
				t.Fatalf("checkNameFits(%q) = nil, want a rejection", tc.cluster)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkNameFits(%q) = %v, want it accepted", tc.cluster, err)
			}
			// The message has to name the cause, or it is no better than the
			// timeout it replaces.
			if err != nil && !strings.Contains(err.Error(), "twice") {
				t.Errorf("rejection does not explain why: %v", err)
			}
		})
	}
}

// A longer home directory eats the same budget, so the check cannot be a fixed
// name-length rule.
func TestCheckNameFits_AccountsForTheStateDirectory(t *testing.T) {
	const cluster = "sbi-98062-cnc"

	if err := checkNameFits("/Users/n/.talos/clusters", cluster); err != nil {
		t.Errorf("short home: %v", err)
	}
	long := "/Users/a-rather-long-user-name-on-a-long-path/.talos/clusters"
	if err := checkNameFits(long, cluster); err == nil {
		t.Errorf("checkNameFits(%q, %q) = nil; the state path leaves no room", long, cluster)
	}
}
