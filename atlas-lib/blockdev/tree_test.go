// The sysfs and procfs trees the scan, usage, and candidacy tests read.
//
// The tree is built with real directories and real symlinks because that is
// what the scan actually depends on: an interface is classified by where
// class/block points into the device tree, and a fixture that put the
// attributes flat under class/block would exercise none of the path walking
// that decides a device's transport, its slot, and whether it is virtual.
//
// The host below carries one device of each shape the classifier distinguishes,
// so a change that breaks one branch fails a named test rather than shifting a
// count.

package blockdev

import (
	"os"
	"path/filepath"
	"testing"
)

// tree is a set of files, symlinks, and bare directories to materialize under a
// temporary root.
type tree struct {
	files map[string]string
	links map[string]string
	dirs  []string
}

// write materializes the tree and returns its root.
func (t2 tree) write(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range t2.dirs {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range t2.files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, target := range t2.links {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// blockAttrs writes the attributes every block device carries, whatever it is.
func (t2 tree) blockAttrs(dir, devNo string, sectors uint64, rotational, readOnly, removable bool) {
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	t2.files[dir+"/dev"] = devNo
	t2.files[dir+"/size"] = itoa(int(sectors))
	t2.files[dir+"/ro"] = bit(readOnly)
	t2.files[dir+"/removable"] = bit(removable)
	t2.files[dir+"/queue/logical_block_size"] = "512"
	t2.files[dir+"/queue/physical_block_size"] = "4096"
	t2.files[dir+"/queue/rotational"] = bit(rotational)
}

// storageHost is a worker with one device of each shape: a free NVMe SSD, a
// boot NVMe carrying a partitioned root, a SATA disk an LVM volume group has
// taken, a free SAS disk, a virtio disk, a loop device, and the device-mapper
// node the SATA disk feeds.
func storageHost() tree {
	const (
		freeNVMe = "devices/pci0000:00/0000:5e:00.0/nvme/nvme0/nvme0n1"
		bootNVMe = "devices/pci0000:00/0000:5f:00.0/nvme/nvme1/nvme1n1"
		sata     = "devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda"
		sas      = "devices/pci0000:00/0000:af:00.0/host1/port-1:0/end_device-1:0/target1:0:0/1:0:0:0/block/sdb"
		virtio   = "devices/pci0000:00/0000:00:05.0/virtio2/block/vda"
		loop     = "devices/virtual/block/loop0"
		mapper   = "devices/virtual/block/dm-0"
	)
	t2 := tree{files: map[string]string{}, links: map[string]string{}}

	// A 3.2 TB NVMe SSD nothing has taken: the shape a discovery run is for.
	t2.blockAttrs(freeNVMe, "259:0", 6251233968, false, false, false)
	t2.files["devices/pci0000:00/0000:5e:00.0/nvme/nvme0/model"] = "SAMSUNG MZQL23T8HCLS-00A07  "
	t2.files["devices/pci0000:00/0000:5e:00.0/nvme/nvme0/serial"] = "S6CVNE0T500123      "
	t2.files["devices/pci0000:00/0000:5e:00.0/numa_node"] = "0"
	t2.links["class/block/nvme0n1"] = "../../" + freeNVMe
	t2.links[freeNVMe+"/device"] = ".."

	// The boot NVMe, with an EFI partition and a root partition.
	t2.blockAttrs(bootNVMe, "259:2", 976773168, false, false, false)
	t2.blockAttrs(bootNVMe+"/nvme1n1p1", "259:3", 1048576, false, false, false)
	t2.blockAttrs(bootNVMe+"/nvme1n1p2", "259:4", 975724592, false, false, false)
	t2.files[bootNVMe+"/nvme1n1p1/partition"] = "1"
	t2.files[bootNVMe+"/nvme1n1p2/partition"] = "2"
	t2.files["devices/pci0000:00/0000:5f:00.0/nvme/nvme1/model"] = "INTEL SSDPEKNU512GZ"
	t2.files["devices/pci0000:00/0000:5f:00.0/nvme/nvme1/serial"] = "PHKA1234567890A"
	t2.files["devices/pci0000:00/0000:5f:00.0/numa_node"] = "1"
	t2.links["class/block/nvme1n1"] = "../../" + bootNVMe
	t2.links["class/block/nvme1n1p1"] = "../../" + bootNVMe + "/nvme1n1p1"
	t2.links["class/block/nvme1n1p2"] = "../../" + bootNVMe + "/nvme1n1p2"
	t2.links[bootNVMe+"/device"] = ".."

	// A rotational SATA disk that an LVM volume group already holds.
	t2.blockAttrs(sata, "8:0", 3907029168, true, false, false)
	t2.files["devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/vendor"] = "ATA     "
	t2.files["devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/model"] = "ST2000DM008-2FR1"
	t2.links["class/block/sda"] = "../../" + sata
	t2.links[sata+"/device"] = "../.."
	t2.dirs = append(t2.dirs, sata+"/holders/dm-0")

	// A free SAS disk behind an expander.
	t2.blockAttrs(sas, "8:16", 3750748848, true, false, false)
	t2.files["devices/pci0000:00/0000:af:00.0/host1/port-1:0/end_device-1:0/target1:0:0/1:0:0:0/vendor"] = "SEAGATE "
	t2.files["devices/pci0000:00/0000:af:00.0/host1/port-1:0/end_device-1:0/target1:0:0/1:0:0:0/model"] = "ST2000NM0045    "
	t2.files["devices/pci0000:00/0000:af:00.0/numa_node"] = "1"
	t2.links["class/block/sdb"] = "../../" + sas
	t2.links[sas+"/device"] = "../.."

	// A virtio disk, which is what a discovery run finds on a virtual worker.
	t2.blockAttrs(virtio, "253:0", 209715200, false, false, false)
	t2.links["class/block/vda"] = "../../" + virtio
	t2.links[virtio+"/device"] = "../.."

	// A loop device and the device-mapper node the SATA disk feeds. Neither is
	// backend storage, and both are in class/block beside the disks that are.
	t2.blockAttrs(loop, "7:0", 0, false, false, false)
	t2.links["class/block/loop0"] = "../../" + loop
	t2.dirs = append(t2.dirs, loop+"/loop")

	t2.blockAttrs(mapper, "252:0", 3907022848, false, false, false)
	t2.files[mapper+"/dm/name"] = "vg0-root"
	t2.links["class/block/dm-0"] = "../../" + mapper
	t2.dirs = append(t2.dirs, mapper+"/slaves/sda")

	return t2
}

// multipathHost is the case the path alone cannot decide: two NVMe namespaces
// the kernel presents through a subsystem rather than through a controller, one
// reached over TCP and one a dual-ported disk in a slot in this machine.
//
// Both sit under devices/virtual, both are called nvmeXnY, and neither carries
// a partition or a holder. The only thing separating a volume this node has
// attached from a disk it owns is the transport its controllers report.
func multipathHost() tree {
	const (
		fabric = "devices/virtual/nvme-subsystem/nvme-subsys0/nvme3n1"
		local  = "devices/virtual/nvme-subsystem/nvme-subsys1/nvme4n1"
	)
	t2 := tree{files: map[string]string{}, links: map[string]string{}}

	t2.blockAttrs(fabric, "259:8", 209715200, false, false, false)
	t2.files["devices/virtual/nvme-subsystem/nvme-subsys0/nvme3/transport"] = "tcp"
	t2.files["devices/virtual/nvme-subsystem/nvme-subsys0/nvme3/state"] = "live"
	t2.links["class/block/nvme3n1"] = "../../" + fabric

	t2.blockAttrs(local, "259:9", 3125627568, false, false, false)
	t2.files["devices/virtual/nvme-subsystem/nvme-subsys1/nvme4/transport"] = "pcie"
	t2.files["devices/virtual/nvme-subsystem/nvme-subsys1/nvme5/transport"] = "pcie"
	t2.links["class/block/nvme4n1"] = "../../" + local

	t2.files["self/mountinfo"] = "25 1 0:24 / / rw - overlay overlay rw"
	t2.files["swaps"] = swapOnVirtio
	return t2
}
