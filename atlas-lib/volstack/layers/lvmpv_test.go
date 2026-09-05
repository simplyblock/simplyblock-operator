// What the physical-volume layer has to guarantee.
//
// It writes a durable thing onto a device, so like the filesystem layer above it
// the question that matters is when it must not. A pvcreate over a device holding
// somebody's data destroys it exactly as a mkfs does, and the evidence it may
// rest on is the same reading rather than a tool's silence.

package layers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// lvmCommands records what the layer asked LVM to do and answers what a test told
// it to. It fakes the command execution rather than the Manager that builds the
// commands, so the real device scoping, argument building, and output parsing all
// run.
//
// It keys on the command word rather than on a substring of the whole line. A
// substring match answers yes for pvs when the layer ran pvscan, and the same
// prefix trap has already produced two wrong-looking green tests in this package.
type lvmCommands struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func newLVM() *lvmCommands {
	return &lvmCommands{out: map[string]string{}, err: map[string]error{}}
}

func (l *lvmCommands) run(_ context.Context, args ...string) (string, error) {
	l.calls = append(l.calls, args)
	for _, key := range keysFor(args) {
		if err, ok := l.err[key]; ok {
			return "", err
		}
		if out, ok := l.out[key]; ok {
			return out, nil
		}
	}
	return "", nil
}

// keysFor is what a test may key an answer on, most specific first.
//
// The command word alone is enough for most of them, but not for lvs, which
// answers two different questions distinguished only by the field it was asked
// for: the volumes in a group, and one volume's attributes. A test keying only on
// "lvs" would answer both with whichever it set.
func keysFor(args []string) []string {
	keys := []string{}
	for i, arg := range args {
		if arg == "-o" && i+1 < len(args) {
			keys = append(keys, args[0]+":"+args[i+1])
			break
		}
	}
	return append(keys, args[0])
}

// ran reports whether this command was issued at all.
func (l *lvmCommands) ran(command string) bool { return l.indexOf(command) >= 0 }

// indexOf is where this command was issued, or -1, so a test can assert an order
// as well as a presence.
func (l *lvmCommands) indexOf(command string) int {
	for i, call := range l.calls {
		if call[0] == command {
			return i
		}
	}
	return -1
}

// issued renders the calls for a failure message.
func (l *lvmCommands) issued() string {
	lines := make([]string, 0, len(l.calls))
	for _, call := range l.calls {
		lines = append(lines, strings.Join(call, " "))
	}
	return strings.Join(lines, "\n")
}

func (l *lvmCommands) manager() *lvm.Manager { return lvm.NewManagerWithRunner(l.run) }

const (
	testVG = "vol-33333333-3333-3333-3333-333333333333"
	testLV = "lv-33333333-3333-3333-3333-333333333333"
)

func newLVMPV(cmds *lvmCommands, reading blockdev.Reading, readErr error) *LVMPhysicalVolume {
	return NewLVMPhysicalVolume(LVMPhysicalVolumeConfig{
		VolumeGroup:   testVG,
		LogicalVolume: testLV,
		Manager:       cmds.manager(),
		Content:       fakeReader{reading: reading, err: readErr},
	})
}

// lvmLabel is the reading a device carrying a physical-volume label produces.
func lvmLabel() blockdev.Reading {
	return blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "LVM2_member"}
}

// A device positively read as blank is the one thing a pvcreate may run over.
func TestLVMPVCreatesOnlyOnABlankDevice(t *testing.T) {
	cmds := newLVM()
	l := newLVMPV(cmds, blockdev.Reading{Content: blockdev.ContentBlank}, nil)

	state, own, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Fatalf("state = %s, want Absent", state)
	}
	if len(own.Devices) != 0 {
		t.Errorf("an absent layer exposed %d devices", len(own.Devices))
	}

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !cmds.ran("pvcreate") {
		t.Fatalf("no pvcreate was issued:\n%s", cmds.issued())
	}
}

// Regression: 2026-09-05-pvcreate-decided-from-pvs-output — the VDO stack decided
// "no volume group here, create one" from pvs's output text, which reports the
// absence of a label the same way whether the device is empty or merely
// unreadable. A degraded device serving nothing carries no label, so the create
// path was armed over a volume holding data.
//
// A positive reading of blank is what establishes StateAbsent now, and a device
// that cannot be read has produced no reading at all.
func TestLVMPVNeverCreatesOnADeviceItCouldNotRead(t *testing.T) {
	cmds := newLVM()
	l := newLVMPV(cmds, blockdev.Reading{}, errors.New("input/output error"))

	if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
		t.Error("Observe accepted a device it could not read")
	}
	if _, err := l.Ensure(context.Background(), belowArtifact()); err == nil {
		t.Error("Ensure proceeded on a device it could not read")
	}
	if cmds.ran("pvcreate") {
		t.Fatalf("it ran pvcreate anyway:\n%s", cmds.issued())
	}
}

// A device carrying anything else is not a device to put a physical volume on,
// whatever LVM would go on to say about it.
func TestLVMPVRefusesADeviceCarryingSomethingElse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading blockdev.Reading
	}{
		{"a filesystem", blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}},
		{"a partition table", blockdev.Reading{Content: blockdev.ContentForeign, Type: "gpt"}},
		{"another stack's layer", blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "crypto_LUKS"}},
		{"bytes matching nothing known", blockdev.Reading{Content: blockdev.ContentUnknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmds := newLVM()
			l := newLVMPV(cmds, tc.reading, nil)

			if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
				t.Error("Observe reported a state for it rather than refusing")
			}
			if _, err := l.Ensure(context.Background(), belowArtifact()); err == nil {
				t.Error("Ensure proceeded")
			}
			if cmds.ran("pvcreate") {
				t.Fatalf("it ran pvcreate anyway:\n%s", cmds.issued())
			}
		})
	}
}

// A label naming this volume's own group is the layer already in place.
func TestLVMPVOwnLabelIsReady(t *testing.T) {
	cmds := newLVM()
	cmds.out["pvs"] = "  " + testVG + "\n"
	l := newLVMPV(cmds, lvmLabel(), nil)

	state, own, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Fatalf("state = %s, want Ready", state)
	}
	if len(own.Devices) != 1 {
		t.Errorf("exposed %d devices, want the one it sits on", len(own.Devices))
	}
}

// A label belonging to no volume group at all is this layer's own object,
// finished. pvcreate is the whole of what it does, and a group over the label is
// the next layer's business, so this is what an Ensure that succeeded and then
// crashed leaves behind. Reading it as anything else makes the bring-up
// non-convergent.
func TestLVMPVLabelWithNoGroupIsReady(t *testing.T) {
	cmds := newLVM()
	cmds.out["pvs"] = "\n"
	l := newLVMPV(cmds, lvmLabel(), nil)

	state, _, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Fatalf("state = %s, want Ready", state)
	}

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if cmds.ran("pvcreate") {
		t.Errorf("a label that is already there was written again:\n%s", cmds.issued())
	}
	if cmds.ran("vgimportclone") {
		t.Errorf("a label belonging to nobody was treated as a clone:\n%s", cmds.issued())
	}
}

// A label naming another volume's group is what a byte-level clone produces: the
// copy carries its source's identity until it is re-stamped.
func TestLVMPVForeignLabelIsForeign(t *testing.T) {
	cmds := newLVM()
	cmds.out["pvs"] = "  vol-somebody-elses-volume\n"
	l := newLVMPV(cmds, lvmLabel(), nil)

	state, own, err := l.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateForeign {
		t.Fatalf("state = %s, want Foreign", state)
	}
	if len(own.Devices) != 1 {
		t.Errorf("exposed %d devices, want the one the label is on, which a release still has to reach",
			len(own.Devices))
	}
}

// Ensure on a foreign label re-identifies the device rather than creating over it.
// The data is the clone's own, and the only thing wrong with it is whose name is
// on it.
func TestLVMPVEnsureReidentifiesAForeignLabel(t *testing.T) {
	cmds := newLVM()
	cmds.out["pvs"] = "  vol-somebody-elses-volume\n"
	cmds.out["lvs"] = "  lv-somebody-elses-volume\n"
	l := newLVMPV(cmds, lvmLabel(), nil)

	if _, err := l.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if cmds.ran("pvcreate") {
		t.Fatalf("a clone's label was overwritten rather than re-identified:\n%s", cmds.issued())
	}
	if !cmds.ran("vgimportclone") {
		t.Fatalf("the clone was not re-identified:\n%s", cmds.issued())
	}
	if !cmds.ran("lvrename") {
		t.Fatalf("the clone kept its source's logical volume name, which the layer above looks for by name:\n%s",
			cmds.issued())
	}
}

// A probe failure over a device that does carry a label is an error, not a
// reading of a group that must therefore be ours. LVM answers the same way for a
// device it could not lock as for one that belongs to nobody.
func TestLVMPVRefusesWhenTheGroupCannotBeProbed(t *testing.T) {
	cmds := newLVM()
	cmds.err["pvs"] = errors.New("cannot open /dev/nvme0n1 exclusively")
	l := newLVMPV(cmds, lvmLabel(), nil)

	if _, _, err := l.Observe(context.Background(), belowArtifact()); err == nil {
		t.Fatal("Observe folded a probe failure into a state")
	}
	if _, err := l.Ensure(context.Background(), belowArtifact()); err == nil {
		t.Error("Ensure proceeded on a device whose identity is unknown")
	}
}

// A physical-volume signature is not a hold, so there is nothing for this host to
// give up. Release does nothing, and above all does not remove the label, which
// is what separates it from Destroy.
func TestLVMPVReleaseDoesNothing(t *testing.T) {
	cmds := newLVM()
	cmds.out["pvs"] = "  " + testVG + "\n"
	l := newLVMPV(cmds, lvmLabel(), nil)

	if err := l.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(cmds.calls) != 0 {
		t.Fatalf("Release touched LVM, and an unstage reaches it on every pod restart:\n%s", cmds.issued())
	}
}

// Destroy removes the label, and only a deletion path reaches it.
func TestLVMPVDestroyRemovesTheLabel(t *testing.T) {
	cmds := newLVM()
	l := newLVMPV(cmds, lvmLabel(), nil)

	if err := l.Destroy(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !cmds.ran("pvremove") {
		t.Fatalf("no pvremove was issued:\n%s", cmds.issued())
	}
}

// The layer's identity is the volume group it belongs to, which a plan derives
// from the volume handle, so there is nothing for the record to carry. It is the
// worked example the Recorder contract names.
func TestLVMPVRecordsNoParameters(t *testing.T) {
	l := newLVMPV(newLVM(), blockdev.Reading{Content: blockdev.ContentBlank}, nil)
	if _, ok := any(l).(volstack.Recorder); ok {
		t.Error("the physical-volume layer declares parameters, which the record contract says it has none of")
	}
}
