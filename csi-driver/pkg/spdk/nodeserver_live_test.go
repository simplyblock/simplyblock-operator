// A live check of the format decision against a real NVMe-oF device, run by
// hand on a node that has one. It is skipped unless SBTEST_DEVICE names the
// device, so it costs an ordinary test run nothing. It exists because the unit
// tests stand in a temporary file for the block device, and the decision this
// package makes is only worth as much as it is worth against the real thing.
package spdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/atlas/blockfs"
	mount "k8s.io/mount-utils"
)

// TestLiveStageVolumeDoesNotReformatARealVolume stages a real device that
// already holds a filesystem, with a blkid that reports nothing — which is what
// a degraded NVMe-oF path produces, as `blkid -p` under a dm-flakey error_reads
// table demonstrates. The volume's data has to survive, and mkfs must never be
// issued.
//
// Run it on a node with a staged simplyblock volume:
//
//	SBTEST_DEVICE=/dev/nvme0n1 SBTEST_EXPECT_FILE=precious.txt ./spdk.test \
//	    -test.run TestLiveStageVolume -test.v
func TestLiveStageVolumeDoesNotReformatARealVolume(t *testing.T) {
	device := os.Getenv("SBTEST_DEVICE")
	if device == "" {
		t.Skip("SBTEST_DEVICE is not set; this test needs a real block device")
	}
	expectFile := os.Getenv("SBTEST_EXPECT_FILE")

	staging := t.TempDir()
	var ran [][]string
	ns := &nodeServer{mounter: mount.New(""), exec: newBlindBlkidExec(&ran)}

	err := ns.stageVolume(context.Background(), device, staging, ext4StageRequest(), map[string]string{})
	if err != nil {
		t.Fatalf("stageVolume on %s: %v", device, err)
	}
	defer func() {
		if umountErr := mount.New("").Unmount(staging); umountErr != nil {
			t.Logf("unmount %s: %v", staging, umountErr)
		}
	}()

	if formats := formatCommands(ran); len(formats) > 0 {
		t.Fatalf("issued mkfs against a real volume that holds data: %v", formats)
	}
	t.Logf("commands issued while staging: %v", ran)

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("read staged filesystem: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("staged filesystem contains: %v", names)

	if expectFile == "" {
		return
	}
	content, err := os.ReadFile(filepath.Join(staging, expectFile))
	if err != nil {
		t.Fatalf("the volume's data did not survive staging: %v", err)
	}
	t.Logf("%s survived: %s", expectFile, content)
}

// TestLiveUpstreamFormatDecisionOnARealVolume is the negative control for the
// test above, and the reason this driver no longer lets mount-utils decide.
// It asks SafeFormatAndMount what it would do with the very same device — the
// call the driver used to make — while blkid reports nothing, and records the
// command it chooses. Nothing is executed: the exec is a fake, and the mounter
// is a fake, so this observes the decision without acting on it.
//
// If this ever stops finding an mkfs, upstream has fixed
// kubernetes/kubernetes#140376 and the driver's own probe could be revisited.
func TestLiveUpstreamFormatDecisionOnARealVolume(t *testing.T) {
	device := os.Getenv("SBTEST_DEVICE")
	if device == "" {
		t.Skip("SBTEST_DEVICE is not set; this test needs a real block device")
	}

	var ran [][]string
	mounter := mount.SafeFormatAndMount{
		Interface: mount.NewFakeMounter(nil),
		Exec:      newBlindBlkidExec(&ran),
	}

	err := mounter.FormatAndMountSensitiveWithFormatOptions(
		device, t.TempDir(), "ext4", nil, nil, nil,
	)
	if err != nil {
		t.Logf("FormatAndMount returned: %v", err)
	}

	formats := formatCommands(ran)
	t.Logf("commands mount-utils chose for a volume holding data: %v", ran)
	if len(formats) == 0 {
		t.Log("mount-utils did not choose to format; upstream may have been fixed")
		return
	}
	t.Logf("mount-utils would have destroyed the volume with: %v", formats)
}

// TestLiveProbeSeesAFreshVolumeAsBlank is the availability half of the
// contract: a freshly provisioned lvol has to come back blank, or the driver
// would refuse to format volumes it is supposed to format. Point
// SBTEST_BLANK_DEVICE at the device of a raw-block volume, which the driver
// never formats.
func TestLiveProbeSeesAFreshVolumeAsBlank(t *testing.T) {
	device := os.Getenv("SBTEST_BLANK_DEVICE")
	if device == "" {
		t.Skip("SBTEST_BLANK_DEVICE is not set; this test needs a fresh raw block device")
	}

	result := blockfs.NewDeviceProber().Probe(context.Background(), device)
	t.Logf("probe of %s: state=%s signature=%q err=%v", device, result.State, result.Signature, result.Err)

	if result.State != blockfs.StateBlank {
		t.Fatalf("state = %q, want %q: a fresh volume that does not read as blank would never be formatted",
			result.State, blockfs.StateBlank)
	}
}
