// That the scan reports the subsystem NQN of an NVMe device.
//
// The NQN is what identifies a namespace as a volume this product exported,
// which the transport can only infer: a fabric namespace is somebody else's
// bytes, and a fabric namespace whose NQN names a simplyblock logical volume is
// this fleet's own. The two answers need different words, and only the NQN
// distinguishes them.

package blockdev

import "testing"

func TestScanReportsTheSubsystemNQNOfAMultipathNamespace(t *testing.T) {
	host := multipathHost()
	const nqn = "nqn.2023-02.io.simplyblock:c30a691a-1d2e-4f3a-9b8c-5d6e7f809a1b:lvol:792e184c-0a1b-2c3d-4e5f-60718293a4b5"
	host.files["devices/virtual/nvme-subsystem/nvme-subsys0/subsysnqn"] = nqn

	root := host.write(t)
	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	if got := scanned(t, disks, "nvme3n1").SubsystemNQN; got != nqn {
		t.Errorf("the fabric namespace reports NQN %q, want the subsystem's", got)
	}
}

func TestScanReportsTheSubsystemNQNOfAControllerNamespace(t *testing.T) {
	// A namespace the kernel presents through a controller rather than a
	// subsystem keeps its NQN one directory up, in the same place.
	host := storageHost()
	const nqn = "nqn.2019-08.org.qemu:local"
	host.files["devices/pci0000:00/0000:5e:00.0/nvme/nvme0/subsysnqn"] = nqn

	root := host.write(t)
	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	if got := scanned(t, disks, "nvme0n1").SubsystemNQN; got != nqn {
		t.Errorf("the local namespace reports NQN %q, want the controller's", got)
	}
}

func TestScanReportsNoNQNForADeviceThatIsNotNVMe(t *testing.T) {
	root := storageHost().write(t)
	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	for _, name := range []string{"sda", "sdb", "vda"} {
		if got := scanned(t, disks, name).SubsystemNQN; got != "" {
			t.Errorf("%s reports NQN %q, and is on no NVMe subsystem", name, got)
		}
	}
}
