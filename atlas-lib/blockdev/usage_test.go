// What the usage reading concludes about who is already using a device.
//
// The four sources disagree on purpose, and the tests keep them apart. A mount
// is recorded against a partition and the disk that carries it is what a
// discovery run must not hand over, so the mount reading has to climb; a swap
// area names a path and nothing else; a holder is a device stacked on top; and
// the exclusive open is the kernel's own answer, which catches all three plus
// whatever none of them saw.

package blockdev

import (
	"errors"
	"path/filepath"
	"testing"
)

// mountedBootDisk is the mountinfo of the fixture host: the boot NVMe's two
// partitions and the device-mapper volume that the SATA disk feeds.
const mountedBootDisk = `25 1 259:4 / / rw,relatime shared:1 - ext4 /dev/nvme1n1p2 rw
26 25 259:3 / /boot/efi rw,relatime shared:2 - vfat /dev/nvme1n1p1 rw,fmask=0077
27 25 252:0 / /var rw,relatime shared:3 - xfs /dev/mapper/vg0-root rw
28 25 0:24 / /sys/fs/cgroup rw,nosuid - cgroup2 cgroup2 rw`

// swapOnVirtio is the /proc/swaps of the same host, with the virtio disk given
// over to swap whole.
const swapOnVirtio = `Filename				Type		Size		Used		Priority
/dev/vda                                partition	8388604		0		-2`

// usageHost writes the fixture host plus the procfs files the usage reading
// needs, and returns the scan the reading is taken over.
func usageHost(t *testing.T) (ScanConfig, []Disk) {
	t.Helper()
	h := storageHost()
	h.files["self/mountinfo"] = mountedBootDisk
	h.files["swaps"] = swapOnVirtio

	root := h.write(t)
	cfg := ScanConfig{SysfsRoot: root, ProcRoot: root}
	disks, err := Scan(cfg)
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}
	return cfg, disks
}

// free is an exclusive opener that hands over every device.
func free(string) error { return nil }

func TestReadUsageClimbsFromAPartitionToItsDisk(t *testing.T) {
	cfg, disks := usageHost(t)

	usage, err := ReadUsage(cfg, disks, free)
	if err != nil {
		t.Fatalf("read the usage: %v", err)
	}

	// Nothing is mounted at the whole boot disk. What matters is that its
	// partitions are, because handing the disk over would take the root
	// filesystem with it.
	boot := usage["nvme1n1"]
	want := []string{"/", "/boot/efi"}
	if len(boot.Mountpoints) != len(want) {
		t.Fatalf("read mountpoints %v for the boot disk, want %v", boot.Mountpoints, want)
	}
	for i, mount := range want {
		if boot.Mountpoints[i] != mount {
			t.Errorf("read mountpoints %v, want %v ordered", boot.Mountpoints, want)
			break
		}
	}
	if part := usage["nvme1n1p2"]; len(part.Mountpoints) != 1 || part.Mountpoints[0] != "/" {
		t.Errorf("read mountpoints %v for the root partition, want [/]", part.Mountpoints)
	}
}

func TestReadUsageLeavesAFreeDiskWithNothingAgainstIt(t *testing.T) {
	cfg, disks := usageHost(t)

	usage, err := ReadUsage(cfg, disks, free)
	if err != nil {
		t.Fatalf("read the usage: %v", err)
	}

	got := usage["nvme0n1"]
	if len(got.Mountpoints) != 0 || got.Swap || len(got.Holders) != 0 || got.Busy || got.ProbeErr != nil {
		t.Errorf("read %+v for a free disk, want nothing against it", got)
	}
}

func TestReadUsageRecordsAHolderAndASwapArea(t *testing.T) {
	cfg, disks := usageHost(t)

	usage, err := ReadUsage(cfg, disks, free)
	if err != nil {
		t.Fatalf("read the usage: %v", err)
	}

	sda := usage["sda"]
	if len(sda.Holders) != 1 || sda.Holders[0] != "dm-0" {
		t.Errorf("read holders %v for the disk an LVM group holds, want [dm-0]", sda.Holders)
	}
	if len(sda.Mountpoints) != 0 {
		t.Errorf("read mountpoints %v for a disk that is not itself mounted; the "+
			"filesystem is on the mapper device above it, and the holder is what says so",
			sda.Mountpoints)
	}

	if vda := usage["vda"]; !vda.Swap {
		t.Error("a disk /proc/swaps names was not reported as a swap area")
	}
	if nvme := usage["nvme0n1"]; nvme.Swap {
		t.Error("reported a disk /proc/swaps does not name as a swap area")
	}
}

func TestReadUsageReportsWhatTheKernelRefusesToHandOver(t *testing.T) {
	cfg, disks := usageHost(t)

	// The kernel holds sdb for a reason none of the readable sources named,
	// which is the case the exclusive open exists for.
	held := func(path string) error {
		if path == "/dev/sdb" {
			return ErrDeviceBusy
		}
		return nil
	}

	usage, err := ReadUsage(cfg, disks, held)
	if err != nil {
		t.Fatalf("read the usage: %v", err)
	}

	sdb := usage["sdb"]
	if !sdb.Busy {
		t.Error("a device the kernel refused to open exclusively was not reported busy")
	}
	if sdb.ProbeErr != nil {
		t.Errorf("a refusal is an answer rather than a failed probe, and this one "+
			"was recorded as %v", sdb.ProbeErr)
	}
	if usage["nvme0n1"].Busy {
		t.Error("reported a device the kernel handed over as busy")
	}
}

func TestReadUsageKeepsAProbeItCouldNotMakeApartFromABusyDevice(t *testing.T) {
	cfg, disks := usageHost(t)

	// A probe that could not run says nothing about the device. Recording it as
	// free would hand over a disk nobody checked, and recording it as busy
	// would report a reason that is not the truth.
	unreadable := errors.New("no permission to open the device")
	broken := func(path string) error {
		if path == "/dev/nvme0n1" {
			return unreadable
		}
		return nil
	}

	usage, err := ReadUsage(cfg, disks, broken)
	if err != nil {
		t.Fatalf("read the usage: %v", err)
	}

	got := usage["nvme0n1"]
	if got.Busy {
		t.Error("a probe that could not run was reported as the kernel refusing the device")
	}
	if !errors.Is(got.ProbeErr, unreadable) {
		t.Errorf("read the probe error %v, want %v", got.ProbeErr, unreadable)
	}
}

func TestReadUsageDoesNotProbeADeviceOfZeroSize(t *testing.T) {
	cfg, disks := usageHost(t)

	// An unbacked loop device has no size and no device node worth opening, and
	// opening one by path would be a probe against whatever /dev happens to
	// hold at that name.
	probed := map[string]bool{}
	record := func(path string) error {
		probed[path] = true
		return nil
	}

	if _, err := ReadUsage(cfg, disks, record); err != nil {
		t.Fatalf("read the usage: %v", err)
	}
	if probed["/dev/loop0"] {
		t.Error("probed a device that reports a size of zero")
	}
	if !probed["/dev/nvme0n1"] {
		t.Error("did not probe a disk with a size")
	}
}

func TestReadUsageReadsTheMountTableItWasPointedAt(t *testing.T) {
	// A process in a container has its own mount namespace, so its own
	// mountinfo does not list the host's mounts. A scan running there and
	// reading self/mountinfo sees an empty table and reports the disk carrying
	// the root filesystem as free, which is how a mounted disk gets handed to a
	// storage cluster. The host's table is PID 1's, and the caller has to be
	// able to say so.
	h := storageHost()
	h.files["self/mountinfo"] = "25 1 0:24 / / rw - overlay overlay rw"
	h.files["1/mountinfo"] = mountedBootDisk
	h.files["swaps"] = swapOnVirtio

	root := h.write(t)
	container := ScanConfig{SysfsRoot: root, ProcRoot: root}
	host := container
	host.MountinfoPath = filepath.Join(root, "1", "mountinfo")

	disks, err := Scan(container)
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	asContainer, err := ReadUsage(container, disks, free)
	if err != nil {
		t.Fatalf("read the usage through the container's own table: %v", err)
	}
	if mounts := asContainer["nvme1n1"].Mountpoints; len(mounts) != 0 {
		t.Errorf("the container's own table lists %v for the boot disk, and the "+
			"fixture put nothing of the host's in it", mounts)
	}

	asHost, err := ReadUsage(host, disks, free)
	if err != nil {
		t.Fatalf("read the usage through the host's table: %v", err)
	}
	if mounts := asHost["nvme1n1"].Mountpoints; len(mounts) != 2 {
		t.Errorf("read %v for the boot disk through the host's table, want its "+
			"two mounts: pointing the reading at the host's namespace is the "+
			"only thing that keeps a probe in a pod from handing over the root disk",
			mounts)
	}
}

func TestReadUsageWithoutMountinfoSaysSoRatherThanReportingNothingMounted(t *testing.T) {
	h := storageHost()
	h.files["swaps"] = swapOnVirtio
	root := h.write(t)
	cfg := ScanConfig{SysfsRoot: root, ProcRoot: root}

	disks, err := Scan(cfg)
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}
	if _, err := ReadUsage(cfg, disks, free); err == nil {
		t.Error("read the usage without mountinfo; an unreadable mount table is " +
			"not a host with nothing mounted, and reporting it as one is how a " +
			"mounted disk gets handed over")
	}
}
