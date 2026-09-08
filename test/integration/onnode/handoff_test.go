//go:build linux

// A volume moving between hosts.
//
// A pod is rescheduled and its volume follows: released on one node and staged
// on another, with nothing in between but the fabric. Every layer above the
// fabric has to recognize what is already there rather than build it again, and
// the recognizing is done by a process that never saw the volume made.
//
// This is where a bring-up that quietly recreates does its worst. On one host a
// second bring-up finds its own mount and its own volume group and is right by
// accident; on another there is nothing of the sort, only bytes, and a layer
// that reads them wrong reformats a volume that was serving a pod a moment ago.
//
// So the two phases run on different nodes, and the second is given no help: it
// blanks nothing, is told nothing about what was written, and checks the volume
// against a record the volume itself carries.

package onnode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"
)

// handoffFiles is what the volume is filled with, and how much of it. Enough
// files to span more than one allocation, small enough that writing them is not
// what the case spends its time on.
const handoffFiles = 24

// uuidRecord holds the filesystem's own identity, written into the filesystem.
//
// Kept on the volume rather than handed between the phases, because a value
// carried around outside would prove only that the two phases agree with each
// other. Read back on the far host it says something stronger: that this is the
// same filesystem, not a new one over the same bytes. A reformat gives the
// volume a new UUID and cannot leave the old one behind in a file.
const uuidRecord = "filesystem-uuid"

// handoffPlan is the stack under test, built the same way on both hosts.
func (h *harness) handoffPlan(t *testing.T) volstack.Plan {
	t.Helper()
	volume := h.volume
	volume.FsType = envOr("SB_HANDOFF_FS", "ext4")

	switch shape := envOr("SB_HANDOFF_PLAN", "lvm"); shape {
	case "plain":
		return h.node.Plain(h.targets[0].Connection(), volume)
	case "lvm":
		return h.node.LVM(h.targets[0].Connection(), volume, plans.LogicalVolumeOptions{})
	default:
		t.Fatalf("SB_HANDOFF_PLAN is %q, which is not a plan this suite builds", shape)
		return nil
	}
}

// requireHandoffPhase skips unless a driver is moving a volume between hosts.
func requireHandoffPhase(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_HANDOFF_PLAN") == "" {
		t.Skip("no handoff is being driven: SB_HANDOFF_PLAN is unset")
	}
}

// TestHandoffWrite stages the volume on this host, fills it, and releases it.
//
// Released rather than left up, because that is what an unstage does and it is
// the state the other host has to find: everything the volume is made of still
// on the device, and nothing of it mapped anywhere.
func TestHandoffWrite(t *testing.T) {
	requireHandoffPhase(t)
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])
	plan := h.handoffPlan(t)

	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("stage the volume on this host: %v", err)
	}

	for i := range handoffFiles {
		name, content := handoffContent(i)
		if err := os.WriteFile(filepath.Join(art.Path, name), content, 0o600); err != nil {
			t.Fatalf("fill the volume: %v", err)
		}
	}

	dev := h.stagedDevice(ctx, t, art)
	uuid := filesystemUUID(ctx, t, dev)
	if uuid == "" {
		t.Fatalf("the filesystem on %s has no UUID, and this case rests on it having one", dev)
	}
	if err := os.WriteFile(filepath.Join(art.Path, uuidRecord), []byte(uuid), 0o600); err != nil {
		t.Fatalf("record the filesystem's identity on the volume: %v", err)
	}

	if err := h.runner().Down(ctx, h.handle(), plan); err != nil {
		t.Fatalf("release the volume on this host: %v", err)
	}
	t.Logf("wrote %d files to %s, filesystem %s, and released it", handoffFiles, art.Path, uuid)
}

// TestHandoffRead stages the same volume on a different host and checks that all
// of it arrived.
func TestHandoffRead(t *testing.T) {
	requireHandoffPhase(t)
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	// Nothing is blanked and nothing is assumed. This host has never seen the
	// volume, which is the whole point.
	plan := h.handoffPlan(t)
	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("stage the volume on this host: %v", err)
	}
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	// The filesystem's own identity, compared with what the volume says it was.
	// A bring-up that recreated anything would have made a new filesystem, and a
	// new filesystem has a new UUID and none of the old one's files.
	recorded, err := os.ReadFile(filepath.Join(art.Path, uuidRecord)) //nolint:gosec // a path this suite wrote
	if err != nil {
		t.Fatalf("the volume does not carry what the other host wrote: %v", err)
	}
	live := filesystemUUID(ctx, t, h.stagedDevice(ctx, t, art))
	if got, want := live, strings.TrimSpace(string(recorded)); got != want {
		t.Fatalf("the volume carries filesystem %s and the other host wrote %s, "+
			"so this is not the filesystem that was staged there", got, want)
	}

	for i := range handoffFiles {
		name, want := handoffContent(i)
		got, readErr := os.ReadFile(filepath.Join(art.Path, name)) //nolint:gosec // a path this suite wrote
		if readErr != nil {
			t.Fatalf("%s did not survive the move: %v", name, readErr)
		}
		if string(got) != string(want) {
			t.Fatalf("%s came back changed: %d bytes, want %d", name, len(got), len(want))
		}
	}
	t.Logf("all %d files and filesystem %s arrived intact", handoffFiles, live)
}

// handoffContent is one file's name and contents, derived from its number so
// that the host reading them needs to be told nothing at all.
func handoffContent(i int) (name string, content []byte) {
	name = fmt.Sprintf("handoff-%02d", i)
	sum := sha256.Sum256([]byte(name))
	// Repeated so a file is larger than the metadata around it, which is what
	// makes a partial arrival visible as a difference rather than as an absence.
	return name, []byte(strings.Repeat(hex.EncodeToString(sum[:]), 64))
}

// stagedDevice is the device the filesystem sits on, which is the volume layer's
// output rather than the namespace underneath it.
func (h *harness) stagedDevice(ctx context.Context, t *testing.T, art volstack.Artifact) string {
	t.Helper()
	if dev, ok := art.Device(); ok {
		return dev.Path
	}
	t.Fatalf("the staged volume exposes %d devices, want one", len(art.Devices))
	return ""
}

// filesystemUUID is what the filesystem on this device calls itself.
func filesystemUUID(ctx context.Context, t *testing.T, device string) string {
	t.Helper()
	out, err := runToolOutput(ctx, "blkid", "-s", "UUID", "-o", "value", device)
	if err != nil {
		t.Fatalf("read the filesystem UUID of %s: %v", device, err)
	}
	return strings.TrimSpace(out)
}
