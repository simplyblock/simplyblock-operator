// What the layer does when the device reads blank and something else says it
// was formatted.
//
// A blank reading is the only state that permits a mkfs, so anything that can
// make it wrong is a way to destroy a volume. The reading this layer rests on is
// a positive one, taken with O_DIRECT against the signature offsets, and a
// device it cannot read is refused rather than called empty. The prior-format
// record is defense behind that: a second, independent statement that this
// volume was formatted once, kept away from the device so that losing the device
// does not lose it too.
//
// The consumer supplies it, because where such a record lives is the consumer's
// business. The CSI driver keeps it on the volume's claim.

package layers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
)

// priorFormat is a record that answers, and counts how often it was asked.
type priorFormat struct {
	fsType string
	err    error
	asked  int
}

func (p *priorFormat) read(context.Context) (string, error) {
	p.asked++
	return p.fsType, p.err
}

// newGuardedFS is the layer over a blank device with a prior-format record.
func newGuardedFS(fsType string, reading blockdev.Reading, prior *priorFormat) (*Filesystem, *fakeFS) {
	ops := newFakeFS()
	return NewFilesystem(FilesystemConfig{
		FsType:      fsType,
		StagingPath: stagingPath,
		Ops:         ops,
		Content:     fakeReader{reading: reading},
		PriorFormat: prior.read,
	}), ops
}

// TestFilesystemFormatsWhenNothingIsRecorded is the ordinary first stage: the
// device reads blank and no record contradicts it, so the volume is formatted.
func TestFilesystemFormatsWhenNothingIsRecorded(t *testing.T) {
	prior := &priorFormat{}
	l, ops := newGuardedFS("ext4", blockdev.Reading{Content: blockdev.ContentBlank}, prior)

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(ops.formatted) != 1 {
		t.Fatalf("formatted %d times, want once", len(ops.formatted))
	}
	if prior.asked == 0 {
		t.Errorf("the record was never consulted, so it guards nothing")
	}
}

// TestFilesystemDoesNotFormatWhatIsRecordedAsFormatted is the guard itself. The
// device reads blank and the record says this volume carries a filesystem, so
// the reading is a failed probe rather than an empty device: it is mounted, and
// under no circumstance formatted. A mount that then fails is the correct
// outcome, because it means the record and the device genuinely disagree, and
// that is not a reason to destroy the volume.
func TestFilesystemDoesNotFormatWhatIsRecordedAsFormatted(t *testing.T) {
	prior := &priorFormat{fsType: "ext4"}
	l, ops := newGuardedFS("ext4", blockdev.Reading{Content: blockdev.ContentBlank}, prior)

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(ops.formatted) != 0 {
		t.Fatalf("formatted a volume recorded as already carrying a filesystem: %+v", ops.formatted)
	}
	if len(ops.mounted) != 1 {
		t.Fatalf("mounted %d times, want once", len(ops.mounted))
	}
}

// TestFilesystemRefusesARecordedFilesystemItWasNotAskedFor is the mismatch the
// content reading cannot see, because there is nothing on the device to read.
// Reformatting destroys the volume and mounting the recorded one serves a
// filesystem the plan does not declare, so neither happens.
func TestFilesystemRefusesARecordedFilesystemItWasNotAskedFor(t *testing.T) {
	prior := &priorFormat{fsType: "ext4"}
	l, ops := newGuardedFS("xfs", blockdev.Reading{Content: blockdev.ContentBlank}, prior)

	_, err := l.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatal("staged a volume recorded as ext4 against a plan asking for xfs")
	}
	if !strings.Contains(err.Error(), "ext4") || !strings.Contains(err.Error(), "xfs") {
		t.Errorf("the refusal names neither filesystem: %v", err)
	}
	if len(ops.formatted) != 0 || len(ops.mounted) != 0 {
		t.Errorf("acted on the device anyway: formatted %+v, mounted %+v", ops.formatted, ops.mounted)
	}
}

// TestFilesystemFailsClosedWhenTheRecordCannotBeRead is the property that makes
// this a guard rather than a hint. An unreachable record is not evidence that
// the volume is blank, and formatting through the doubt is the one outcome that
// cannot be undone.
func TestFilesystemFailsClosedWhenTheRecordCannotBeRead(t *testing.T) {
	prior := &priorFormat{err: errors.New("the API server said no")}
	l, ops := newGuardedFS("ext4", blockdev.Reading{Content: blockdev.ContentBlank}, prior)

	_, err := l.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatal("formatted through a record it could not read")
	}
	if len(ops.formatted) != 0 {
		t.Errorf("formatted anyway: %+v", ops.formatted)
	}
}

// TestFilesystemLeavesTheRecordAloneWhenTheDeviceAnswers is what keeps the guard
// cheap. A device carrying a readable filesystem has settled the question, and
// asking a Kubernetes API on a path that has its answer is latency on every
// stage for nothing.
func TestFilesystemLeavesTheRecordAloneWhenTheDeviceAnswers(t *testing.T) {
	prior := &priorFormat{fsType: "ext4"}
	l, _ := newGuardedFS("ext4",
		blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, prior)

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if prior.asked != 0 {
		t.Errorf("consulted the record %d times for a device that answered for itself", prior.asked)
	}
}

// TestFilesystemWithoutARecordFormatsAsBefore is the consumer that keeps no
// record at all. The seam is optional, and a nil one leaves the layer deciding
// from the reading alone.
func TestFilesystemWithoutARecordFormatsAsBefore(t *testing.T) {
	ops := newFakeFS()
	l := NewFilesystem(FilesystemConfig{
		FsType:      "ext4",
		StagingPath: stagingPath,
		Ops:         ops,
		Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}},
	})

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(ops.formatted) != 1 {
		t.Fatalf("formatted %d times, want once", len(ops.formatted))
	}
}
