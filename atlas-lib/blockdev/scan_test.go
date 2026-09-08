// What the scan reports for each shape of entry class/block holds.
//
// The classification is the subject. A discovery run has to tell an unclaimed
// NVMe SSD from the loop device beside it in the same directory, and the only
// evidence for that is where sysfs puts the device and which subdirectories it
// carries. Every test below names the device it is about, so a broken branch
// fails with the device's name rather than with a count that moved.

package blockdev

import (
	"path/filepath"
	"testing"
)

// scanned finds one device in a scan by kernel name, failing when it is absent.
func scanned(t *testing.T, disks []Disk, name string) Disk {
	t.Helper()
	for _, d := range disks {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("%s is missing from the scan", name)
	return Disk{}
}

func TestScanReadsAnNVMeDiskWhole(t *testing.T) {
	root := storageHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root, DevRoot: "/dev"})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	got := scanned(t, disks, "nvme0n1")
	if got.Path != "/dev/nvme0n1" {
		t.Errorf("read the path %q, want /dev/nvme0n1", got.Path)
	}
	if got.Major != 259 || got.Minor != 0 {
		t.Errorf("read device numbers %d:%d, want 259:0", got.Major, got.Minor)
	}
	// size is in 512-byte units whatever the device's own block size is, which
	// is the kernel's convention and the one arithmetic error this reading can
	// make.
	if got.SizeBytes != 6251233968*512 {
		t.Errorf("read %d bytes, want %d", got.SizeBytes, uint64(6251233968)*512)
	}
	if got.LogicalBlockSize != 512 || got.PhysicalBlockSize != 4096 {
		t.Errorf("read block sizes %d and %d, want 512 and 4096",
			got.LogicalBlockSize, got.PhysicalBlockSize)
	}
	if got.Kind != KindDisk {
		t.Errorf("classified it as %q, want %q", got.Kind, KindDisk)
	}
	if got.Transport != TransportNVMe {
		t.Errorf("read the transport %q, want %q", got.Transport, TransportNVMe)
	}
	if got.PCIAddress != "0000:5e:00.0" {
		t.Errorf("read the slot %q, want 0000:5e:00.0", got.PCIAddress)
	}
	// The trailing spaces sysfs pads the identity strings with are not part of
	// the model, and a deployment config that carried them would not match.
	if got.Model != "SAMSUNG MZQL23T8HCLS-00A07" || got.Serial != "S6CVNE0T500123" {
		t.Errorf("read the identity %q / %q, want the padding trimmed", got.Model, got.Serial)
	}
	if got.Rotational {
		t.Error("reported an NVMe SSD as rotational")
	}
	if got.NUMANode != 0 {
		t.Errorf("read NUMA node %d, want 0", got.NUMANode)
	}
	if len(got.Partitions) != 0 || len(got.Holders) != 0 {
		t.Errorf("read partitions %v and holders %v on a free disk, want neither",
			got.Partitions, got.Holders)
	}
}

func TestScanReportsPartitionsOnTheDiskThatCarriesThem(t *testing.T) {
	root := storageHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	boot := scanned(t, disks, "nvme1n1")
	if boot.Kind != KindDisk {
		t.Errorf("classified the boot disk as %q, want %q", boot.Kind, KindDisk)
	}
	want := []string{"nvme1n1p1", "nvme1n1p2"}
	if len(boot.Partitions) != len(want) {
		t.Fatalf("read partitions %v, want %v", boot.Partitions, want)
	}
	for i, name := range want {
		if boot.Partitions[i] != name {
			t.Errorf("read partitions %v, want %v ordered", boot.Partitions, want)
			break
		}
	}

	part := scanned(t, disks, "nvme1n1p1")
	if part.Kind != KindPartition {
		t.Errorf("classified %s as %q, want %q", part.Name, part.Kind, KindPartition)
	}
	if part.Transport != TransportNVMe {
		t.Errorf("read the transport of a partition as %q, want %q: a partition "+
			"sits on the same bus as its disk", part.Transport, TransportNVMe)
	}
	// A partition has no device link of its own — the kernel nests it inside
	// its disk's directory — so a reading that looked there found nothing and
	// put every partition on no node.
	if part.NUMANode != boot.NUMANode {
		t.Errorf("read the memory node of %s as %d and of the disk carrying it as %d; "+
			"a partition is in the same slot as its disk", part.Name, part.NUMANode, boot.NUMANode)
	}
	if part.PCIAddress != boot.PCIAddress {
		t.Errorf("read the slot of %s as %q and of the disk carrying it as %q",
			part.Name, part.PCIAddress, boot.PCIAddress)
	}
}

func TestScanReportsHoldersOnAClaimedDisk(t *testing.T) {
	root := storageHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	sda := scanned(t, disks, "sda")
	if len(sda.Holders) != 1 || sda.Holders[0] != "dm-0" {
		t.Errorf("read holders %v for a disk an LVM group has taken, want [dm-0]", sda.Holders)
	}
	if !sda.Rotational {
		t.Error("reported a rotational SATA disk as solid state")
	}
	if sda.NUMANode != NUMANodeUnknown {
		t.Errorf("read NUMA node %d for a disk whose bus does not say, want %d",
			sda.NUMANode, NUMANodeUnknown)
	}
	if sda.Vendor != "ATA" || sda.Model != "ST2000DM008-2FR1" {
		t.Errorf("read the identity %q / %q for the SATA disk", sda.Vendor, sda.Model)
	}
}

func TestScanNamesTheTransportOfEachBus(t *testing.T) {
	root := storageHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	for _, tc := range []struct {
		name string
		want Transport
	}{
		{"nvme0n1", TransportNVMe},
		{"sda", TransportSATA},
		{"sdb", TransportSAS},
		{"vda", TransportVirtio},
		{"loop0", TransportUnknown},
		{"dm-0", TransportUnknown},
	} {
		if got := scanned(t, disks, tc.name).Transport; got != tc.want {
			t.Errorf("read the transport of %s as %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestScanClassifiesTheDevicesThatAreNotDisks(t *testing.T) {
	root := storageHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	if got := scanned(t, disks, "loop0"); got.Kind != KindLoop {
		t.Errorf("classified loop0 as %q, want %q", got.Kind, KindLoop)
	}
	mapper := scanned(t, disks, "dm-0")
	if mapper.Kind != KindDeviceMapper {
		t.Errorf("classified dm-0 as %q, want %q", mapper.Kind, KindDeviceMapper)
	}
	if !mapper.Virtual {
		t.Error("a device-mapper node sits under devices/virtual and was not marked virtual")
	}
	if nvme := scanned(t, disks, "nvme0n1"); nvme.Virtual {
		t.Error("marked an NVMe namespace virtual")
	}
}

func TestScanSeparatesAFabricNamespaceFromALocalMultipathDisk(t *testing.T) {
	// The kernel puts the head of a multipath namespace under devices/virtual
	// whether the paths run over TCP or over two PCIe ports, so the path says
	// virtual for both and the name says nvmeXnY for both. The controllers'
	// transport is the only evidence, and getting this wrong offers an attached
	// volume to a cluster as free space.
	root := multipathHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	fabric := scanned(t, disks, "nvme3n1")
	if fabric.Transport != TransportNVMeFabric {
		t.Errorf("read the transport of a namespace whose controller says tcp as %q, want %q",
			fabric.Transport, TransportNVMeFabric)
	}

	local := scanned(t, disks, "nvme4n1")
	if local.Transport != TransportNVMe {
		t.Errorf("read the transport of a dual-ported PCIe namespace as %q, want %q",
			local.Transport, TransportNVMe)
	}
	if !local.Virtual {
		t.Error("a multipath head sits under devices/virtual, whatever its controllers are")
	}
}

func TestScanTreatsASubsystemItCannotReadAsAFabric(t *testing.T) {
	// A namespace whose origin could not be established is not one to hand to a
	// cluster, so the unreadable case answers with the transport that refuses
	// it rather than the one that permits it.
	h := multipathHost()
	delete(h.files, "devices/virtual/nvme-subsystem/nvme-subsys1/nvme4/transport")
	delete(h.files, "devices/virtual/nvme-subsystem/nvme-subsys1/nvme5/transport")

	disks, err := Scan(ScanConfig{SysfsRoot: h.write(t)})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}
	if got := scanned(t, disks, "nvme4n1").Transport; got != TransportNVMeFabric {
		t.Errorf("read the transport of a subsystem with no readable controller as %q, want %q",
			got, TransportNVMeFabric)
	}
}

func TestScanIsOrderedByName(t *testing.T) {
	root := storageHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	want := []string{"dm-0", "loop0", "nvme0n1", "nvme1n1", "nvme1n1p1", "nvme1n1p2", "sda", "sdb", "vda"}
	if len(disks) != len(want) {
		t.Fatalf("scanned %d devices, want %d", len(disks), len(want))
	}
	for i, name := range want {
		if disks[i].Name != name {
			t.Fatalf("device %d is %s, want %s: the scan is ordered so that two "+
				"runs against one host agree", i, disks[i].Name, name)
		}
	}
}

func TestScanReportsNoneRatherThanFailingWithoutTheClassDirectory(t *testing.T) {
	root := tree{files: map[string]string{"unrelated": "x"}}.write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan a tree without class/block: %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("scanned %d devices from a tree with no class/block", len(disks))
	}
}

// The next two are regressions from a run against a real K3s worker, where the
// probe pod reads the host's sysfs at /host/sys. Neither could have shown up
// against a fixture rooted at a bare temporary directory.

func TestScanIgnoresTheCallersMountPointWhenNamingATransport(t *testing.T) {
	// The transport is read from the segments of the resolved device path, and
	// that path begins with wherever the caller mounted sysfs. A probe pod
	// mounts the host's at /host/sys, which puts a segment called "host" in
	// front of every device on the machine; it matched the SCSI host directory
	// pattern, and every device-mapper node, loop device, and network block
	// device on a real worker came back as SCSI.
	root := reRooted(t, storageHost(), filepath.Join("host", "sys"))

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	for _, name := range []string{"dm-0", "loop0"} {
		if got := scanned(t, disks, name).Transport; got != TransportUnknown {
			t.Errorf("read the transport of %s as %q under a /host/sys mount, want %q: "+
				"the mount point is the caller's and says nothing about the bus",
				name, got, TransportUnknown)
		}
	}
	// The real buses still resolve, so the fix is not to report nothing.
	for _, tc := range []struct {
		name string
		want Transport
	}{{"nvme0n1", TransportNVMe}, {"sda", TransportSATA}, {"sdb", TransportSAS}, {"vda", TransportVirtio}} {
		if got := scanned(t, disks, tc.name).Transport; got != tc.want {
			t.Errorf("read the transport of %s as %q, want %q", tc.name, got, tc.want)
		}
	}
}

// reRooted writes a fixture under a nested mount point and returns the sysfs
// root inside it, which is the shape a probe pod sees the host's tree in.
func reRooted(t *testing.T, h tree, mountPoint string) string {
	t.Helper()
	nested := tree{files: map[string]string{}, links: map[string]string{}}
	for path, content := range h.files {
		nested.files[filepath.Join(mountPoint, path)] = content
	}
	for path, target := range h.links {
		nested.links[filepath.Join(mountPoint, path)] = target
	}
	for _, dir := range h.dirs {
		nested.dirs = append(nested.dirs, filepath.Join(mountPoint, dir))
	}
	return filepath.Join(nested.write(t), mountPoint)
}

func TestScanClassifiesANetworkBlockDevice(t *testing.T) {
	// A Rocky worker carries sixteen nbd devices whether or not any is
	// configured. They are not disks in this machine, and calling them disks
	// put sixteen entries claiming to be local storage into every report.
	h := storageHost()
	const nbd = "devices/virtual/block/nbd0"
	h.blockAttrs(nbd, "43:0", 0, false, false, false)
	h.links["class/block/nbd0"] = "../../" + nbd

	disks, err := Scan(ScanConfig{SysfsRoot: h.write(t)})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	if got := scanned(t, disks, "nbd0").Kind; got != KindNetwork {
		t.Errorf("classified nbd0 as %q, want %q: a network block device is not a "+
			"disk in this machine", got, KindNetwork)
	}
}

func TestScanConfigDefaultsToTheLiveHostsRoots(t *testing.T) {
	var cfg ScanConfig
	if cfg.sysfs() != DefaultSysfsRoot {
		t.Errorf("an unset SysfsRoot resolves to %q, want %q", cfg.sysfs(), DefaultSysfsRoot)
	}
	if cfg.dev() != DefaultDevRoot {
		t.Errorf("an unset DevRoot resolves to %q, want %q", cfg.dev(), DefaultDevRoot)
	}
	if cfg.proc() != DefaultProcRoot {
		t.Errorf("an unset ProcRoot resolves to %q, want %q", cfg.proc(), DefaultProcRoot)
	}
}
