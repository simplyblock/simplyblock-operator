// Tests for the one decision NodeStageVolume can never get wrong: whether the
// device it just connected may be handed to mkfs. They live apart from the
// other node-server tests because they are about the format decision alone,
// and they drive stageVolume through the mounter and exec seams so no kernel,
// blkid, or mkfs on the host takes part.
package spdk

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	mount "k8s.io/mount-utils"
	"k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"
)

// ext4SuperblockOffset and ext4MagicOffset locate the ext2/3/4 magic: the
// superblock starts 1024 bytes into the device and carries s_magic 0x38 bytes
// into itself.
const (
	ext4SuperblockOffset = 1024
	ext4MagicOffset      = 0x38
	ext4Magic            = 0xEF53
)

// writeExt4Device creates a file standing in for a block device that holds an
// ext4 filesystem. Only the magic matters: every probe that decides whether a
// device is formatted reads it from this offset.
func writeExt4Device(t *testing.T) string {
	t.Helper()

	device := filepath.Join(t.TempDir(), "nvme0n1")
	content := make([]byte, 256<<10)
	binary.LittleEndian.PutUint16(content[ext4SuperblockOffset+ext4MagicOffset:], ext4Magic)
	if err := os.WriteFile(device, content, 0o600); err != nil {
		t.Fatalf("write fake device: %v", err)
	}
	return device
}

// newBlindBlkidExec returns an exec whose blkid exits 2 having printed nothing,
// which is what util-linux reports both for a device with no filesystem and for
// a device it could not read. Every command it runs is appended to ran, so a
// test can assert on what was and was not executed.
func newBlindBlkidExec(ran *[][]string) *fakeexec.FakeExec {
	action := func(cmd string, args ...string) exec.Cmd {
		*ran = append(*ran, append([]string{cmd}, args...))

		output := func() ([]byte, []byte, error) { return nil, nil, nil }
		if cmd == "blkid" {
			output = func() ([]byte, []byte, error) { return nil, nil, fakeexec.FakeExitError{Status: 2} }
		}

		fake := &fakeexec.FakeCmd{CombinedOutputScript: []fakeexec.FakeAction{output}}
		return fakeexec.InitFakeCmd(fake, cmd, args...)
	}

	scripts := make([]fakeexec.FakeCommandAction, 8)
	for i := range scripts {
		scripts[i] = action
	}
	return &fakeexec.FakeExec{CommandScript: scripts}
}

// ext4StageRequest builds the filesystem stage request the tests below drive
// through stageVolume. ext4 throughout: it is what the production incident ran,
// and what a device holding xfs is deliberately mismatched against.
func ext4StageRequest() *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId: "test-cluster:test-pool:test-lvol",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
}

// formatCommands returns the mkfs invocations found in ran.
func formatCommands(ran [][]string) [][]string {
	var formats [][]string
	for _, argv := range ran {
		if strings.HasPrefix(argv[0], "mkfs") {
			formats = append(formats, argv)
		}
	}
	return formats
}

// Regression: 2026-09-03-csi-blkid-reformat — a volume that already held a
// filesystem was reformatted whenever blkid could not read it. blkid exits 2
// both for "no signature found" and for "could not read the device," and
// mount-utils maps that exit code straight to "unformatted" and runs
// mkfs.ext4 -F, so a single degraded NVMe-oF path during a cold NodeStageVolume
// destroyed the volume's data. Verified on a live host: reads failing under a
// dm-flakey error_reads table make blkid exit 2 with empty output on a device
// that unquestionably holds ext4.
func TestStageVolumeNeverFormatsADeviceHoldingAFilesystem(t *testing.T) {
	device := writeExt4Device(t)
	staging := t.TempDir()

	var ran [][]string
	mounter := mount.NewFakeMounter(nil)
	ns := &nodeServer{mounter: mounter, exec: newBlindBlkidExec(&ran)}

	err := ns.stageVolume(context.Background(), device, staging, ext4StageRequest(), map[string]string{})
	if err != nil {
		t.Fatalf("stageVolume: %v", err)
	}

	if formats := formatCommands(ran); len(formats) > 0 {
		t.Errorf("formatted a device that holds a filesystem: ran %v", formats)
	}

	// The fake mounter records the resolved staging path, and on some hosts the
	// temporary directory sits behind a symlink.
	resolved, err := filepath.EvalSymlinks(staging)
	if err != nil {
		t.Fatalf("resolve staging path: %v", err)
	}
	var staged bool
	for _, mp := range mounter.MountPoints {
		if mp.Device == device && (mp.Path == staging || mp.Path == resolved) {
			staged = true
		}
	}
	if !staged {
		t.Errorf("device was not mounted at the staging path; mount points: %+v", mounter.MountPoints)
	}
}

// Regression: 2026-09-03-csi-blkid-reformat — a device the plugin cannot read at
// all was treated as an empty one and formatted. Nothing can be concluded about
// a device that will not answer a read, so staging has to fail and let kubelet
// retry rather than decide the volume is blank.
func TestStageVolumeRefusesADeviceItCannotRead(t *testing.T) {
	// Never created: standing in for a device whose reads fail, which is what a
	// volume behind a lost or timed-out NVMe-oF path does.
	device := filepath.Join(t.TempDir(), "nvme0n1")
	staging := t.TempDir()

	var ran [][]string
	ns := &nodeServer{mounter: mount.NewFakeMounter(nil), exec: newBlindBlkidExec(&ran)}

	err := ns.stageVolume(context.Background(), device, staging, ext4StageRequest(), map[string]string{})
	if err == nil {
		t.Error("stageVolume succeeded on a device it could not read; expected it to refuse")
	}

	if formats := formatCommands(ran); len(formats) > 0 {
		t.Errorf("formatted a device it could not read: ran %v", formats)
	}
}

// deviceWithSignature writes a device whose leading bytes carry magic at offset.
func deviceWithSignature(t *testing.T, offset int, magic []byte) string {
	t.Helper()

	device := filepath.Join(t.TempDir(), "nvme0n1")
	content := make([]byte, 256<<10)
	copy(content[offset:], magic)
	if err := os.WriteFile(device, content, 0o600); err != nil {
		t.Fatalf("write fake device: %v", err)
	}
	return device
}

// TestStageVolumeFormatsABlankDevice is the availability half of the format
// decision: refusing to format is only safe as long as a volume that genuinely
// needs a filesystem still gets one, or no PVC would ever come up.
func TestStageVolumeFormatsABlankDevice(t *testing.T) {
	// All zeros, which is what a freshly provisioned lvol reads as.
	device := deviceWithSignature(t, 0, nil)
	staging := t.TempDir()

	var ran [][]string
	mounter := mount.NewFakeMounter(nil)
	ns := &nodeServer{mounter: mounter, exec: newBlindBlkidExec(&ran)}

	if err := ns.stageVolume(
		context.Background(), device, staging, ext4StageRequest(), map[string]string{},
	); err != nil {
		t.Fatalf("stageVolume refused a blank device: %v", err)
	}

	if len(mounter.MountPoints) == 0 {
		t.Error("blank device was not mounted")
	}

	// mount-utils implements the format decision only on Linux; elsewhere it
	// mounts without probing, so there is no mkfs to find.
	if runtime.GOOS != "linux" {
		return
	}
	formats := formatCommands(ran)
	if len(formats) != 1 || formats[0][0] != "mkfs.ext4" {
		t.Errorf("expected one mkfs.ext4 for a blank device, got %v", formats)
	}
}

// TestStageVolumeRefusesADeviceHoldingForeignData covers content that is
// somebody's data without being a filesystem this driver can mount. Formatting
// it would destroy it just as surely.
func TestStageVolumeRefusesADeviceHoldingForeignData(t *testing.T) {
	// An LVM2 physical-volume label in the second sector.
	device := deviceWithSignature(t, 512, []byte("LABELONE"))

	var ran [][]string
	ns := &nodeServer{mounter: mount.NewFakeMounter(nil), exec: newBlindBlkidExec(&ran)}

	err := ns.stageVolume(
		context.Background(), device, t.TempDir(), ext4StageRequest(), map[string]string{},
	)
	if err == nil {
		t.Error("stageVolume accepted a device holding an LVM2 physical volume")
	}

	if formats := formatCommands(ran); len(formats) > 0 {
		t.Errorf("formatted a device holding foreign data: ran %v", formats)
	}
}

// TestStageVolumeMountsAMismatchedFilesystemRatherThanReformatting pins the
// direction a disagreement resolves in. A volume whose StorageClass says ext4
// over a device holding XFS is a misconfiguration, and the mount will fail and
// say so — but the data is not this driver's to overwrite on the way there.
func TestStageVolumeMountsAMismatchedFilesystemRatherThanReformatting(t *testing.T) {
	device := deviceWithSignature(t, 0, []byte("XFSB"))
	staging := t.TempDir()

	var ran [][]string
	mounter := mount.NewFakeMounter(nil)
	ns := &nodeServer{mounter: mounter, exec: newBlindBlkidExec(&ran)}

	if err := ns.stageVolume(
		context.Background(), device, staging, ext4StageRequest(), map[string]string{},
	); err != nil {
		t.Fatalf("stageVolume: %v", err)
	}

	if formats := formatCommands(ran); len(formats) > 0 {
		t.Errorf("reformatted a device whose filesystem did not match the requested type: ran %v", formats)
	}
	if len(mounter.MountPoints) == 0 {
		t.Error("device was not mounted")
	}
}

// TestStageVolumeSkipsRawBlockVolumes keeps the decision out of the path of a
// volume that has no filesystem by design.
func TestStageVolumeSkipsRawBlockVolumes(t *testing.T) {
	device := writeExt4Device(t)

	var ran [][]string
	mounter := mount.NewFakeMounter(nil)
	ns := &nodeServer{mounter: mounter, exec: newBlindBlkidExec(&ran)}

	req := &csi.NodeStageVolumeRequest{
		VolumeId: "test-cluster:test-pool:test-lvol",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	if err := ns.stageVolume(context.Background(), device, t.TempDir(), req, map[string]string{}); err != nil {
		t.Fatalf("stageVolume: %v", err)
	}

	if len(ran) > 0 {
		t.Errorf("ran commands for a raw block volume: %v", ran)
	}
	if len(mounter.MountPoints) > 0 {
		t.Errorf("mounted a raw block volume: %+v", mounter.MountPoints)
	}
}
