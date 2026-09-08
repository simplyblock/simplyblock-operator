// Tests for stageVolume's format decision: once the preflight probe has found a
// filesystem on the device, staging must mount that filesystem and must never
// format, whatever a later probe of the (possibly degraded) device would say.
// The tests script the external commands staging runs, so the contract is
// asserted by what was executed and what was mounted, not by how the code got
// there. They live next to nodeserver.go because they exercise unexported
// staging internals.

package node

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	k8smount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/mount"
)

// fakeDevice stands in for the NVMe-oF block device staging operates on, and
// extFS and xfsFS name the two filesystems the tests move between.
const (
	fakeDevice = "/dev/fake-lvol"
	extFS      = "ext4"
	xfsFS      = "xfs"
)

// scriptedResult is one external command's scripted outcome: its combined
// output and its error, in the order staging runs commands.
type scriptedResult struct {
	out string
	err error
}

// scriptedExec builds a FakeExec that answers successive commands from script
// and records every invocation's argv, so a test can assert which commands
// staging chose to run. Scripting more results than the code under test
// consumes is fine; running more commands than scripted panics, so each test
// scripts the longest path it wants to observe.
func scriptedExec(script []scriptedResult) (*testingexec.FakeExec, *[][]string) {
	fe := &testingexec.FakeExec{}
	calls := &[][]string{}
	for _, r := range script {
		fe.CommandScript = append(fe.CommandScript, func(cmd string, args ...string) utilexec.Cmd {
			*calls = append(*calls, append([]string{cmd}, args...))
			fc := &testingexec.FakeCmd{
				CombinedOutputScript: []testingexec.FakeAction{
					func() ([]byte, []byte, error) { return []byte(r.out), nil, r.err },
				},
			}
			return testingexec.InitFakeCmd(fc, cmd, args...)
		})
	}
	return fe, calls
}

// stagingDir returns a fresh staging path for one test, resolved through
// symlinks because FakeMounter records the resolved target and the comparison
// has to match it (macOS puts temporary directories behind /var → /private/var).
func stagingDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return filepath.Join(base, "stage")
}

// stageRequest builds the NodeStageVolumeRequest the stage tests share: a
// filesystem volume asking for the given fstype.
func stageRequest(fsType string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId: "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:33333333-3333-3333-3333-333333333333", //nolint:lll // one volume handle
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
}

// Regression: 2026-09-04-format-decided-by-upstream-reprobe — staging probed
// the device, found ext4, and then handed the device to
// FormatAndMountSensitiveWithFormatOptions, which probes it again itself and
// formats whenever that second probe reads blank. On a fabric that degraded
// between the two probes this reformatted a filesystem staging had just
// positively identified, and the annotation guard never ran because it is only
// consulted when the preflight reads blank. This is the remaining door of the
// mkfs data-loss incident of 2026-09-03 after the annotation guard.
func TestStageNeverFormatsWhenPreflightFoundFilesystem(t *testing.T) {
	fe, calls := scriptedExec([]scriptedResult{
		{out: "TYPE=ext4\n"},                         // preflight blkid: the device carries ext4
		{err: &testingexec.FakeExitError{Status: 2}}, // a second blkid: the device now reads blank
		{out: ""}, // the mkfs the unfixed code runs next
		{out: ""}, // spare scripting for any further command
	})
	fm := k8smount.NewFakeMounter(nil)
	ns := &Server{mounter: mount.NewWith(fm, fe)}

	stagingPath := stagingDir(t)
	volumeContext := map[string]string{}
	err := ns.stageVolume(context.Background(), fakeDevice, stagingPath, stageRequest(extFS), volumeContext)
	if err != nil {
		t.Fatalf("stageVolume: %v", err)
	}

	for _, call := range *calls {
		if strings.HasPrefix(call[0], "mkfs") {
			t.Fatalf(
				"staging ran %v on a device whose preflight probe found ext4; "+
					"a volume that was formatted once must never be formatted again",
				call,
			)
		}
	}

	mounted := false
	for _, mp := range fm.MountPoints {
		if mp.Path == stagingPath && mp.Type == extFS {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("staging did not mount the ext4 filesystem at %s; mounts: %+v", stagingPath, fm.MountPoints)
	}
}

// Regression: 2026-09-04-format-decided-by-upstream-reprobe — the companion
// contract of the same fix. When the device already carries a filesystem other
// than the one the volume asks for, staging must mount what is actually there
// (mounting ext4 as XFS fails, and XFS needs nouuid — the same reasoning #481
// applied to the annotation branch), must record it for the restage path, and
// must not run fsck: FormatAndMountSensitiveWithFormatOptions preen-repairs
// every existing filesystem it mounts read-write, which writes to a device
// whose path state staging cannot judge.
// Regression: 2026-09-06-staged-a-filesystem-the-class-did-not-ask-for. Staging
// mounted whatever the device carried, warning about the disagreement and
// carrying on. Nothing was destroyed by that, and it is still the wrong answer:
// a volume serving XFS to a class that says ext4 is a misconfiguration nobody is
// told about, and the next thing to notice it is whatever decides to make the
// device match the class again. That decision reformats.
//
// So staging refuses, while the volume is intact and while the disagreement is
// the thing in front of whoever is looking. It must refuse without reformatting,
// which is the half of this that would be catastrophic to get wrong.
func TestStageRefusesAFilesystemTheClassDidNotAskFor(t *testing.T) {
	fe, calls := scriptedExec([]scriptedResult{
		{out: "TYPE=xfs\n"}, // preflight blkid: the device carries XFS
		{out: ""},           // spare scripting, in case anything else runs
		{out: ""},
	})
	fm := k8smount.NewFakeMounter(nil)
	ns := &Server{mounter: mount.NewWith(fm, fe)}

	stagingPath := stagingDir(t)
	volumeContext := map[string]string{}
	err := ns.stageVolume(context.Background(), fakeDevice, stagingPath, stageRequest(extFS), volumeContext)
	if err == nil {
		t.Fatal("staging mounted a device carrying xfs for a volume that asks for ext4")
	}
	for _, want := range []string{xfsFS, extFS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so a reader cannot tell what disagreed: %v", want, err)
		}
	}

	for _, call := range *calls {
		switch call[0] {
		case "mkfs", "mkfs.ext4", "mkfs.xfs":
			t.Fatalf("staging formatted a device carrying a filesystem: %v", call)
		case "fsck":
			t.Fatalf("staging ran %v against a device it refused to stage", call)
		}
	}
	if len(fm.MountPoints) != 0 {
		t.Errorf("staging mounted something anyway: %v", fm.MountPoints)
	}
}

// The same rule on the other branch: a blank probe settled by the claim's
// record. The record says what the volume was formatted as, so a class asking
// for something else is the same disagreement, reached by a different road.
//
// Regression: 2026-09-06-staged-a-filesystem-the-class-did-not-ask-for.
func TestStageRefusesWhenTheRecordedFilesystemIsNotTheClassOne(t *testing.T) {
	ns, _ := newPVCTestNodeServer(t, annotatedPVC(xfsFS))
	fe, calls := scriptedExec([]scriptedResult{
		{out: ""}, // preflight blkid: reads blank, so the claim settles it
		{out: ""},
		{out: ""},
	})
	fm := k8smount.NewFakeMounter(nil)
	ns.mounter = mount.NewWith(fm, fe)

	req := stageRequest(extFS)
	req.VolumeId = pvcTestHandle
	volumeContext := map[string]string{
		csicommon.CSIStorageNamespaceKey: pvcTestNamespace,
		csicommon.CSIStorageNameKey:      pvcTestName,
	}

	err := ns.stageVolume(context.Background(), fakeDevice, stagingDir(t), req, volumeContext)
	if err == nil {
		t.Fatal("staging mounted a volume recorded as xfs for a class that asks for ext4")
	}
	for _, want := range []string{xfsFS, extFS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	for _, call := range *calls {
		if strings.HasPrefix(call[0], "mkfs") {
			t.Fatalf("staging formatted a volume the claim says holds a filesystem: %v", call)
		}
	}
	if len(fm.MountPoints) != 0 {
		t.Errorf("staging mounted something anyway: %v", fm.MountPoints)
	}
}
