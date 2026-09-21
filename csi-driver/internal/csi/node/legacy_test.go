// Naming a volume this driver did not stage.
//
// The cases here are all the same situation from different angles: an upgrade
// leaves volumes that are up on the host and were built by a version that wrote
// no stack record, and a teardown of one has to know which namespace it is
// allowed to detach. What it must never do is guess, which is why the refusal
// these tests bracket is worth keeping.

package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

// legacyContext is what a volume staged before the stack has when the stash
// never landed: nothing. The old node service wrote volume-context.json as the
// last step of NodeStageVolume, after the connect and after the mount, so a
// driver that died in that window left a staged volume with no context at all.
func legacyContext() map[string]string { return map[string]string{} }

// A volume that neither the record nor the stash names is named by the host:
// the staging path is a mount, the mount has a device under it, and the device
// says which subsystem and namespace it belongs to. That is evidence rather
// than a guess, which is the only thing that may lift the refusal.
func TestTeardownNamesALegacyVolumeFromTheHost(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())

	asked := ""
	ns.identifyStaged = func(_ context.Context, stagingTargetPath string) (lvol.Connection, error) {
		asked = stagingTargetPath
		return lvol.Connection{
			NQN:  "nqn.2023-02.io.simplyblock:cluster:lvol:abc",
			NSID: 1,
			UUID: "abc",
		}, nil
	}

	plan, err := ns.teardownPlan(
		context.Background(), pvcTestHandle, "/staging", legacyContext())
	if err != nil {
		t.Fatalf("teardownPlan refused a volume the host can name: %v", err)
	}
	if got := strings.Join(plan.Names(), " → "); got != plainShape {
		t.Errorf("the volume is released as %s, want the legacy plan", got)
	}
	if asked != "/staging" {
		t.Errorf("the host was asked about %q, want the staging path", asked)
	}
}

// The refusal stands where the host cannot name it either. Releasing a
// namespace nothing identifies detaches whichever one is found first, which on
// a node serving several volumes is somebody else's.
func TestTeardownRefusesAVolumeTheHostCannotName(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
		return lvol.Connection{}, errors.New("no device is mounted at /staging")
	}

	_, err := ns.teardownPlan(context.Background(), pvcTestHandle, "/staging", legacyContext())
	if err == nil {
		t.Fatal("a volume nothing identifies was accepted for release")
	}
	if !strings.Contains(err.Error(), "identifies") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// A context that names the volume is not overridden by the host. The stash is
// what the volume was staged with, the host reading is a reconstruction, and
// the two agreeing is not something to rely on.
func TestTeardownPrefersTheStashOverTheHost(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	asked := false
	ns.identifyStaged = func(context.Context, string) (lvol.Connection, error) {
		asked = true
		return lvol.Connection{NQN: "nqn.from.the.host", NSID: 9}, nil
	}

	if _, err := ns.teardownPlan(
		context.Background(), pvcTestHandle, "/staging", stagedContext()); err != nil {
		t.Fatalf("teardownPlan: %v", err)
	}
	if asked {
		t.Error("the host was read for a volume whose stashed context already named it")
	}
}
