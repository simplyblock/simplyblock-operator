// What the filesystem layer does when the device disagrees with the plan.
//
// A volume formatted as one filesystem and asked for as another is somebody
// having changed what a class says about a volume that already exists. The data
// on it is fine and must stay that way, so the layer neither reformats nor
// quietly serves the one it found: it refuses, while the volume is still intact
// and while the disagreement is still visible.

package layers

import (
	"context"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// Regression: 2026-09-06-mounted-a-filesystem-it-was-not-asked-for. The layer
// mounted whatever the device carried, warning about the disagreement and
// carrying on. Nothing was destroyed by that, and it is still the wrong answer:
// a volume serving ext4 to a plan that says XFS is a misconfiguration nobody is
// told about, and the next thing to notice is whatever decides to make the
// device match the plan again. Refusing keeps it loud while the data is intact.
func TestFilesystemRefusesTheFilesystemItWasNotAskedFor(t *testing.T) {
	ops := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "xfs",
		StagingPath: stagingPath,
		Ops:         ops,
		Content: fakeReader{reading: blockdev.Reading{
			Content: blockdev.ContentFilesystem,
			Type:    "ext4",
			Detail:  "ext4 superblock at 1024",
		}},
	})

	_, err := l.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatal("Ensure staged a device carrying ext4 for a plan that asks for xfs")
	}
	for _, want := range []string{"ext4", "xfs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so a reader cannot tell what disagreed: %v", want, err)
		}
	}

	if len(ops.formatted) != 0 {
		t.Fatalf("it reformatted the device: %v", ops.formatted)
	}
	if len(ops.mounted) != 0 {
		t.Errorf("it mounted the device anyway: %v", ops.mounted)
	}
}

// Observe reports the same refusal, so a caller that only looks before acting
// sees it too.
func TestFilesystemObserveRefusesTheMismatch(t *testing.T) {
	ops := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "xfs",
		StagingPath: stagingPath,
		Ops:         ops,
		Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}},
	})

	if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
		t.Error("Observe reported a state for a device carrying a filesystem the plan did not ask for")
	}
}

// A heal is a remount of what is already there, so it refuses the same
// disagreement rather than mounting through it.
func TestFilesystemHealRefusesTheMismatch(t *testing.T) {
	ops := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "xfs",
		StagingPath: stagingPath,
		Ops:         ops,
		Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}},
	})

	if err := l.Heal(context.Background(), belowArtifact(), volstack.Artifact{Path: stagingPath}); err == nil {
		t.Fatal("Heal remounted a device carrying a filesystem the plan did not ask for")
	}
	if len(ops.mounted) != 0 {
		t.Errorf("it mounted the device anyway: %v", ops.mounted)
	}
}

// The agreeing case still stages, which is what keeps the refusal about the
// disagreement rather than about having found a filesystem at all.
func TestFilesystemStagesWhatItWasAskedFor(t *testing.T) {
	ops := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "ext4",
		StagingPath: stagingPath,
		Ops:         ops,
		Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}},
	})

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(ops.formatted) != 0 {
		t.Fatalf("it reformatted a filesystem it recognized: %v", ops.formatted)
	}
	if len(ops.mounted) != 1 || ops.mounted[0].fsType != "ext4" {
		t.Errorf("mounted %v, want one ext4 mount", ops.mounted)
	}
}
