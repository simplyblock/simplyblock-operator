// The two init containers that turn a per-node ConfigMap into a configure run.
//
// They are shell, so what they do wrong they do quietly. The assertions here are
// on the scripts as text, which is the weakest kind of test and the only one
// available short of a cluster: what they are guarding is not a value but a
// failure that must not be swallowed.

package utils

import (
	"strings"
	"testing"
)

// scripts returns the two init-container scripts of a storage-node workload.
func scripts(t *testing.T) (writer, generator string) {
	t.Helper()
	g, w := nodeEnvScripts()
	if w == "" || g == "" {
		t.Fatal("the workload renders no init scripts")
	}
	return w, g
}

// TestAMissingPerNodeEntryFailsTheWriter is the defect this guards.
//
// Regression: 2026-09-21-an-absent-node-entry-was-written-as-an-empty-one — the
// writer answered a missing entry with `touch`, so an empty env file was
// indistinguishable from a configured one. The generator then ran with nothing
// set and reported that max-lvol 0 was invalid, which names neither the
// node nor the file nor the ConfigMap.
//
// It matters because the writer runs once. A pod whose ConfigMap volume was not
// yet populated when it started keeps the empty file for its whole life: the
// generator crash-loops, and restarting a later init container never re-runs an
// earlier one. Failing instead is what makes the retry pick the entry up.
func TestAMissingPerNodeEntryFailsTheWriter(t *testing.T) {
	writer, _ := scripts(t)

	if strings.Contains(writer, "touch /etc/node-env/env.sh") {
		t.Error("a missing per-node entry still produces an empty env file, " +
			"which the generator cannot tell from a configured one")
	}
	if !strings.Contains(writer, "exit 1") {
		t.Error("the writer does not fail on a missing entry, so nothing retries it")
	}
}

// TestAnAbsentSubsystemCountStopsTheConfigure covers the other half.
//
// Regression: 2026-09-21-max-lvol-zero — the generator passed
// --max-lvol=${MAX_SUBSYS_COUNT:-0}, so a node that had received no
// configuration asked the control plane for zero subsystems and was refused for
// asking for zero. The message named a value nobody had set rather than the
// configuration that never arrived.
//
// Stopping is the contract rather than a choice: spec.maxSubsystemCount is
// Required and bounded at 10, and its documentation says a node receiving no
// value fails config generation outright rather than falling back to a default.
// So the fix is not to leave the argument out -- that would be the fallback the
// field forbids -- but to fail where the cause can still be named.
func TestAnAbsentSubsystemCountStopsTheConfigure(t *testing.T) {
	_, generator := scripts(t)

	if strings.Contains(generator, "${MAX_SUBSYS_COUNT:-0}") {
		t.Error("an unset subsystem count is sent as --max-lvol=0, and refused for being zero")
	}
	if !strings.Contains(generator, `exit 1`) {
		t.Error("an unset subsystem count does not stop the configure, so the node is " +
			"configured from a default the cluster never stated")
	}
	if !strings.Contains(generator, "--max-lvol=${MAX_SUBSYS_COUNT}") {
		t.Error("the subsystem count the cluster did state is not passed")
	}
}
