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

// TestAnAbsentSubsystemCountIsNotSentAsZero covers the other half.
//
// Regression: 2026-09-21-max-lvol-zero — the generator passed
// --max-lvol=${MAX_SUBSYS_COUNT:-0}, and zero is a value the control plane
// refuses, saying that max-lvol must be a positive integer. A cluster that
// states no
// maxSubsystemCount is a cluster with no opinion about it, which is the control
// plane's default rather than zero.
func TestAnAbsentSubsystemCountIsNotSentAsZero(t *testing.T) {
	_, generator := scripts(t)

	if strings.Contains(generator, "${MAX_SUBSYS_COUNT:-0}") {
		t.Error("an unset subsystem count is sent as --max-lvol=0, which is refused")
	}
	if !strings.Contains(generator, "MAX_SUBSYS_COUNT") {
		t.Error("the subsystem count is not passed at all")
	}
}
