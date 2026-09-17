package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/plans"
)

func TestVDOParams(t *testing.T) {
	cases := []struct {
		name                         string
		vc                           map[string]string
		compression, dedup, wantsVDO bool
	}{
		{"neither set", map[string]string{}, false, false, false},
		{"compression only", map[string]string{kube.ParamClientCompression: "true"}, true, false, true},
		{"deduplication only", map[string]string{kube.ParamClientDeduplication: "true"}, false, true, true},
		{"both", map[string]string{
			kube.ParamClientCompression: "true", kube.ParamClientDeduplication: "true",
		}, true, true, true},
		{"unparsable values are false", map[string]string{
			kube.ParamClientCompression: "not-a-bool",
		}, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			compression, dedup, wantsVDO := vdoParams(c.vc)
			if compression != c.compression || dedup != c.dedup || wantsVDO != c.wantsVDO {
				t.Errorf("vdoParams(%v) = (%v, %v, %v), want (%v, %v, %v)",
					c.vc, compression, dedup, wantsVDO, c.compression, c.dedup, c.wantsVDO)
			}
		})
	}
}

// fakeDMCommands is a minimal fake LVM command runner for the tests in this
// file: it records every call and answers a scripted result keyed on the
// command word, the way atlas-lib/volstack/layers' own fixtures are.
type fakeDMCommands struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func newFakeDMCommands() *fakeDMCommands {
	return &fakeDMCommands{out: map[string]string{}, err: map[string]error{}}
}

func (f *fakeDMCommands) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	for _, key := range dmCommandKeys(args) {
		if err, ok := f.err[key]; ok {
			return "", err
		}
	}
	for _, key := range dmCommandKeys(args) {
		if out, ok := f.out[key]; ok {
			return out, nil
		}
	}
	return "", nil
}

// dmCommandKeys is what a test may key an answer on, most specific first: the
// field an -o flag asked for (lvs distinguishes lv_name from lv_attr this
// way), then the bare command word.
func dmCommandKeys(args []string) []string {
	keys := []string{}
	for i, arg := range args {
		if arg == "-o" && i+1 < len(args) {
			keys = append(keys, args[0]+":"+args[i+1])
			break
		}
	}
	return append(keys, args[0])
}

func (f *fakeDMCommands) ran(command string) bool {
	for _, call := range f.calls {
		if call[0] == command {
			return true
		}
	}
	return false
}

func (f *fakeDMCommands) issued() string {
	lines := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		lines = append(lines, strings.Join(call, " "))
	}
	return strings.Join(lines, "\n")
}

// fakeContentReader answers a fixed reading for every device, which is enough
// for these tests: none of them exercises a mixed-membership plan where two
// devices disagree.
type fakeContentReader struct {
	reading blockdev.Reading
	err     error
}

func (f fakeContentReader) Read(context.Context, blockdev.Device) (blockdev.Reading, error) {
	return f.reading, f.err
}

// fakeResolve answers a device for exactly the paths it was told about, and
// errors — the same way blockdev.ResolveDevice does for a path the kernel no
// longer has anything under — for every other path, which is how a test
// scripts "the raw device is gone."
type fakeResolve struct {
	present map[string]blockdev.Device
}

func (f fakeResolve) resolve(path string) (blockdev.Device, error) {
	if dev, ok := f.present[path]; ok {
		return dev, nil
	}
	return blockdev.Device{}, errors.New("no such device")
}

const (
	testLvolID     = "33333333-3333-3333-3333-333333333333"
	testRawDevPath = "/dev/disk/by-id/nvme-uuid." + testLvolID
)

func testRawDevice() blockdev.Device {
	return blockdev.Device{Path: testRawDevPath, Name: "nvme0n1", LogicalBlockSize: 512, SizeBytes: 10 << 30}
}

// testMappedDevPath is the device-mapper path the VDO logical volume maps to
// once it is active, which LVMLogicalVolume.artifact resolves independently
// of the raw device below it.
func testMappedDevPath() string {
	return "/dev/" + plans.VolumeGroupName(testLvolID) + "/" + plans.LogicalVolumeName(testLvolID)
}

func testMappedDevice() blockdev.Device {
	return blockdev.Device{Path: testMappedDevPath(), Name: "dm-0", LogicalBlockSize: 512, SizeBytes: 10 << 30}
}

// resolvableStack answers both the raw device and, once it exists, the
// device-mapper path the VDO logical volume maps to.
func resolvableStack() fakeResolve {
	return fakeResolve{present: map[string]blockdev.Device{
		testRawDevPath:      testRawDevice(),
		testMappedDevPath(): testMappedDevice(),
	}}
}

func newTestVDOStack(t *testing.T, cmds *fakeDMCommands, content fakeContentReader, resolve fakeResolve) *vdoStack {
	t.Helper()
	return &vdoStack{
		manager: lvm.NewManagerWithRunner(cmds.run),
		content: content,
		runner:  volstack.NewRunner(volstack.NewStore(t.TempDir())),
		resolve: resolve.resolve,
	}
}

// rawDeviceLayer's Observe/Ensure over a device that is, and is not, there.

func TestRawDeviceLayer_PresentDevice(t *testing.T) {
	resolve := fakeResolve{present: map[string]blockdev.Device{testRawDevPath: testRawDevice()}}
	l := newRawDeviceLayer(testRawDevPath, resolve.resolve)

	state, artifact, err := l.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Errorf("state = %s, want Ready", state)
	}
	dev, ok := artifact.Device()
	if !ok || dev.Path != testRawDevPath {
		t.Errorf("Observe exposed %+v, want the resolved device at %s", artifact, testRawDevPath)
	}

	ensured, err := l.Ensure(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if dev, ok := ensured.Device(); !ok || dev.Path != testRawDevPath {
		t.Errorf("Ensure exposed %+v, want the resolved device", ensured)
	}
}

func TestRawDeviceLayer_AbsentDevice(t *testing.T) {
	resolve := fakeResolve{present: map[string]blockdev.Device{}}
	l := newRawDeviceLayer(testRawDevPath, resolve.resolve)

	state, artifact, err := l.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v, want no error when the device is simply gone", err)
	}
	if state != volstack.StateAbsent {
		t.Errorf("state = %s, want Absent", state)
	}
	if len(artifact.Devices) != 0 {
		t.Errorf("an absent layer exposed %d devices", len(artifact.Devices))
	}

	if _, err := l.Ensure(context.Background(), volstack.Artifact{}); err == nil {
		t.Error("Ensure succeeded with no device to build the VDO stack on")
	}
}

func TestRawDeviceLayer_ReleaseAndDestroyAreNoOps(t *testing.T) {
	l := newRawDeviceLayer(testRawDevPath, nil)
	if err := l.Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Errorf("Release: %v, want nil — disconnecting the fabric stays the initiator's job", err)
	}
	if err := l.Destroy(context.Background(), volstack.Artifact{}); err != nil {
		t.Errorf("Destroy: %v, want nil — the namespace belongs to the control plane", err)
	}
}

// vdoStack.Up: a fresh volume is created with the full VDO lvcreate
// invocation, named via atlas-lib/volstack/plans' own naming rule.

func TestVDOStack_Up_CreatesFreshStack(t *testing.T) {
	cmds := newFakeDMCommands()
	resolve := resolvableStack()
	content := fakeContentReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}}
	stack := newTestVDOStack(t, cmds, content, resolve)

	devicePath, err := stack.Up(context.Background(), testLvolID, testRawDevPath, true, true)
	if err != nil {
		t.Fatalf("Up: %v\n%s", err, cmds.issued())
	}

	wantVG := plans.VolumeGroupName(testLvolID)
	wantLV := plans.LogicalVolumeName(testLvolID)
	wantDevicePath := "/dev/" + wantVG + "/" + wantLV
	if devicePath != wantDevicePath {
		t.Errorf("Up() device path = %q, want %q", devicePath, wantDevicePath)
	}
	for _, want := range []string{"pvcreate", "vgcreate", "lvcreate", "vgchange"} {
		if !cmds.ran(want) {
			t.Errorf("Up did not run %s, a fresh stack needs it:\n%s", want, cmds.issued())
		}
	}
	var lvcreateArgs []string
	for _, call := range cmds.calls {
		if call[0] == "lvcreate" {
			lvcreateArgs = call
		}
	}
	wantContains := []string{"--type", "vdo", "--compression", "y", "--deduplication", "y", wantVG + "/vdopool"}
	for _, want := range wantContains {
		found := false
		for _, arg := range lvcreateArgs {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("lvcreate args %v missing %q", lvcreateArgs, want)
		}
	}
}

// vdoStack.Up is idempotent: a stack already present and active is
// reactivated, never recreated (design-issue-277 §7.2's whole point).

func TestVDOStack_Up_ReactivatesExistingStackWithoutRecreating(t *testing.T) {
	cmds := newFakeDMCommands()
	resolve := resolvableStack()
	wantVG := plans.VolumeGroupName(testLvolID)
	wantLV := plans.LogicalVolumeName(testLvolID)
	content := fakeContentReader{reading: blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "LVM2_member"}}
	cmds.out["pvs"] = "  " + wantVG + "\n"
	cmds.out["lvs:lv_name"] = "  " + wantLV + "\n"
	cmds.out["lvs:lv_attr"] = "-wi-ao----\n" // 'a' at lvAttrStateIndex: already active

	stack := newTestVDOStack(t, cmds, content, resolve)

	if _, err := stack.Up(context.Background(), testLvolID, testRawDevPath, true, true); err != nil {
		t.Fatalf("Up: %v\n%s", err, cmds.issued())
	}
	if cmds.ran("lvcreate") {
		t.Errorf("Up recreated an already-present stack, which destroys its data:\n%s", cmds.issued())
	}
	if cmds.ran("vgcreate") {
		t.Errorf("Up recreated an already-present group:\n%s", cmds.issued())
	}
}

// vdoStack.Down releases a normal, still-connected stack.

func TestVDOStack_Down_Deactivates(t *testing.T) {
	cmds := newFakeDMCommands()
	resolve := resolvableStack()
	wantVG := plans.VolumeGroupName(testLvolID)
	wantLV := plans.LogicalVolumeName(testLvolID)
	content := fakeContentReader{reading: blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "LVM2_member"}}
	cmds.out["pvs"] = "  " + wantVG + "\n"
	cmds.out["lvs:lv_name"] = "  " + wantLV + "\n"
	cmds.out["lvs:lv_attr"] = "-wi-ao----\n"

	stack := newTestVDOStack(t, cmds, content, resolve)
	if err := stack.Down(context.Background(), testLvolID, testRawDevPath); err != nil {
		t.Fatalf("Down: %v\n%s", err, cmds.issued())
	}
	if !cmds.ran("vgchange") {
		t.Errorf("Down did not deactivate the group:\n%s", cmds.issued())
	}
	for _, forbidden := range []string{"lvremove", "vgremove", "pvremove"} {
		if cmds.ran(forbidden) {
			t.Fatalf("Down ran %s, which an unstage must never do:\n%s", forbidden, cmds.issued())
		}
	}
}

// vdoStack.Down still releases the stack when the raw device is entirely
// gone (total path loss without a clean unstage), by falling back to
// removing the live device-mapper nodes directly — design-issue-277 §8, the
// scenario the atlas-lib volstack layers' own total-path-loss handling
// (lvmVolumeGroup.Observe's HasOrphanedDMNodes check) exists for.
func TestVDOStack_Down_FallsBackWhenRawDeviceIsGone(t *testing.T) {
	cmds := newFakeDMCommands()
	resolve := fakeResolve{present: map[string]blockdev.Device{}} // the raw device is gone
	wantVG := plans.VolumeGroupName(testLvolID)
	escapedVG := strings.ReplaceAll(wantVG, "-", "--")
	cmds.out["dmsetup"] = escapedVG + "-vdopool-vpool\t(253:3)\n"
	cmds.err["vgchange"] = errors.New("Volume group " + wantVG + " not found")

	stack := newTestVDOStack(t, cmds, fakeContentReader{}, resolve)
	if err := stack.Down(context.Background(), testLvolID, testRawDevPath); err != nil {
		t.Fatalf("Down: %v, want the force path to succeed even with no raw device left:\n%s", err, cmds.issued())
	}
	if !cmds.ran("dmsetup") {
		t.Errorf("Down never reached the device-mapper force path:\n%s", cmds.issued())
	}
}

// vdoStack.Grow extends the pool and the logical volume ahead of the
// filesystem resize (design-issue-277 §9).

func TestVDOStack_Grow(t *testing.T) {
	cmds := newFakeDMCommands()
	resolve := resolvableStack()
	wantVG := plans.VolumeGroupName(testLvolID)
	content := fakeContentReader{reading: blockdev.Reading{Content: blockdev.ContentStackLayer, Type: "LVM2_member"}}
	cmds.out["pvs"] = "  " + wantVG + "\n"
	// LogicalVolumeSize (lvs -o lv_size), read for the pool once the pool LV has
	// been extended, to size the VDO logical volume on top of it to match.
	cmds.out["lvs:lv_size"] = "5368709120"

	stack := newTestVDOStack(t, cmds, content, resolve)
	if err := stack.Grow(context.Background(), testLvolID, testRawDevPath); err != nil {
		t.Fatalf("Grow: %v\n%s", err, cmds.issued())
	}
	if !cmds.ran("pvresize") {
		t.Errorf("Grow did not extend the physical volume:\n%s", cmds.issued())
	}
	if !cmds.ran("lvextend") {
		t.Errorf("Grow did not extend the logical volume:\n%s", cmds.issued())
	}
}
