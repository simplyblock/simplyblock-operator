// What a device says about writing 4K atomically.
//
// The question is whether a write of that size survives a power failure whole,
// which is what lets a cluster run checksum validation on a device whose
// logical block size is smaller. The kernel answers it in
// queue/atomic_write_unit_max_bytes, and only since 6.11: before that the
// attribute does not exist, and its absence says nothing about the hardware.
//
// So the reading distinguishes three answers and not two. A device that reports
// a maximum is believed; a device that reports zero has told us it guarantees
// nothing; a kernel that reports no attribute at all has not been asked.

package blockdev

import "testing"

// diskNamed scans a fixture host and returns the one disk asked for, which is
// the two lines every case here would otherwise open with.
func diskNamed(t *testing.T, host tree, name string) Disk {
	t.Helper()
	disks, err := Scan(ScanConfig{SysfsRoot: host.write(t), DevRoot: "/dev"})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}
	return scanned(t, disks, name)
}

func TestADeviceReportingAnAtomicUnitIsBelieved(t *testing.T) {
	host := storageHost()
	dir := "devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1"
	host.files[dir+"/queue/atomic_write_unit_max_bytes"] = "4096"
	host.files[dir+"/queue/atomic_write_unit_min_bytes"] = "512"

	disk := diskNamed(t, host, "nvme0n1")

	if got := disk.AtomicWriteUnitMaxBytes; got == nil || *got != 4096 {
		t.Fatalf("AtomicWriteUnitMaxBytes = %v, want 4096", got)
	}
	if got := disk.AtomicWriteUnitMinBytes; got == nil || *got != 512 {
		t.Fatalf("AtomicWriteUnitMinBytes = %v, want 512", got)
	}
	if !disk.AtomicAt(4096) {
		t.Error("a device whose atomic unit is 4096 is not atomic at 4096")
	}
}

// Zero is an answer: the device was asked and guarantees nothing.
func TestADeviceReportingZeroIsNotAtomic(t *testing.T) {
	host := storageHost()
	dir := "devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1"
	host.files[dir+"/queue/atomic_write_unit_max_bytes"] = "0"

	disk := diskNamed(t, host, "nvme0n1")

	if got := disk.AtomicWriteUnitMaxBytes; got == nil || *got != 0 {
		t.Fatalf("AtomicWriteUnitMaxBytes = %v, want a stated 0", got)
	}
	if disk.AtomicAt(4096) {
		t.Error("a device guaranteeing nothing was read as atomic")
	}
}

// A kernel older than 6.11 publishes no such attribute, and that is not the
// same as a device that guarantees nothing: reading it as false would turn
// every host of that vintage into hardware nobody can run checksums on.
func TestAKernelThatDoesNotPublishTheAttributeSaysNothing(t *testing.T) {
	disk := diskNamed(t, storageHost(), "nvme0n1")

	if got := disk.AtomicWriteUnitMaxBytes; got != nil {
		t.Fatalf("AtomicWriteUnitMaxBytes = %v, want nothing stated", *got)
	}
	if disk.AtomicAt(4096) {
		t.Error("an unanswered question was read as a yes")
	}
	if disk.AtomicityKnown() {
		t.Error("an unanswered question was reported as known")
	}
}

// A device whose maximum is below the size asked about is not atomic at it.
func TestADeviceAtomicOnlyBelowTheSizeAskedAbout(t *testing.T) {
	host := storageHost()
	dir := "devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1"
	host.files[dir+"/queue/atomic_write_unit_max_bytes"] = "512"

	disk := diskNamed(t, host, "nvme0n1")

	if disk.AtomicAt(4096) {
		t.Error("a device atomic to 512 was read as atomic at 4096")
	}
	if !disk.AtomicAt(512) {
		t.Error("a device atomic to 512 was not atomic at 512")
	}
}
