package cluster

import (
	"strings"
	"testing"
)

func TestCheckNameFits(t *testing.T) {
	// A real macOS home, since the budget depends on it.
	const state = "/Users/noctarius/.talos/clusters"

	tests := []struct {
		name    string
		cluster string
		wantErr bool
	}{
		{"four characters too long", "sb-integration-98062-cnc", true},
		{"the same name without the suffix", "sb-integration-98062", false},
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
			// The message has to name the cause to be worth more than the
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

// TestSurvivorMessageSaysWhatToDo. A create that fails leaves the cluster's
// QEMU processes running, and talosctl reports the teardown of a cluster whose
// state it could not read as a success and removes the state directory anyway.
// Nothing can find those processes afterward, and they hold a vmnet interface,
// so every later create on the host fails too, with an error about a port or an
// interface that says nothing about the cause. The teardown cannot kill them,
// since they belong to root, so what it owes is the exact command that can.
func TestSurvivorMessageSaysWhatToDo(t *testing.T) {
	msg := survivorMessage("sbi-4899", []string{"4914", "4916"})

	for _, want := range []string{"sbi-4899", "4914", "4916", "sudo kill -9"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not carry %q:\n%s", want, msg)
		}
	}
}

// TestSurvivorMessageIsEmptyWhenNothingSurvived keeps a clean teardown quiet.
func TestSurvivorMessageIsEmptyWhenNothingSurvived(t *testing.T) {
	if msg := survivorMessage("sbi-4899", nil); msg != "" {
		t.Errorf("a clean teardown reported: %s", msg)
	}
}
