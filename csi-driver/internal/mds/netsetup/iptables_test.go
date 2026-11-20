package netsetup

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeIPTables answers -C from a set of installed rules and records every call.
type fakeIPTables struct {
	installed map[string]bool
	checkCode int // returned for -C on a rule that is not installed
	calls     [][]string
	runErr    error
}

func (f *fakeIPTables) run(_ context.Context, args ...string) (int, string, error) {
	f.calls = append(f.calls, slices.Clone(args))
	if f.runErr != nil {
		return 0, "", f.runErr
	}
	cmd, key := commandOf(args)
	switch cmd {
	case "-C":
		if f.installed[key] {
			return 0, "", nil
		}
		return f.checkCode, "iptables: Bad rule (does a matching rule exist in that chain?).", nil
	case "-A":
		f.installed[key] = true
		return 0, "", nil
	}
	return 2, "unexpected command", nil
}

// commandOf returns the command flag and the rule with the command removed,
// so a -C and an -A of the same rule share a key.
func commandOf(args []string) (string, string) {
	var cmd string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-C" || args[i] == "-A" {
			cmd = args[i]
			continue
		}
		rest = append(rest, args[i])
	}
	return cmd, strings.Join(rest, " ")
}

var testRule = Rule{Table: "nat", Chain: "POSTROUTING", Spec: []string{"-o", "eth0", "-j", "MASQUERADE"}}

func TestMissingRuleIsAppendedOnce(t *testing.T) {
	f := &fakeIPTables{installed: map[string]bool{}, checkCode: 1}
	ctx := context.Background()

	if err := EnsureRules(ctx, f.run, []Rule{testRule}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := EnsureRules(ctx, f.run, []Rule{testRule}); err != nil {
		t.Fatalf("second run: %v", err)
	}

	var appends [][]string
	for _, c := range f.calls {
		if slices.Contains(c, "-A") {
			appends = append(appends, c)
		}
	}
	want := []string{"-w", "-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"}
	if len(appends) != 1 || !slices.Equal(appends[0], want) {
		t.Errorf("appends = %v, want exactly %v", appends, want)
	}
}

// Exit code 2 is iptables refusing the arguments (or a missing module), not
// `no such rule`. Appending after it would install a rule the check could not
// see, and the next run would install it again.
func TestFailedCheckIsAnErrorAndAppendsNothing(t *testing.T) {
	f := &fakeIPTables{installed: map[string]bool{}, checkCode: 2}
	if err := EnsureRules(context.Background(), f.run, []Rule{testRule}); err == nil {
		t.Fatal("EnsureRules succeeded after iptables -C exited 2")
	}
	for _, c := range f.calls {
		if slices.Contains(c, "-A") {
			t.Errorf("appended after a failed check: %v", c)
		}
	}
}

func TestIPTablesThatCannotRunIsAnError(t *testing.T) {
	f := &fakeIPTables{installed: map[string]bool{}, runErr: errors.New("exec: iptables: not found")}
	if err := EnsureRules(context.Background(), f.run, []Rule{testRule}); err == nil {
		t.Fatal("EnsureRules succeeded without iptables")
	}
}
