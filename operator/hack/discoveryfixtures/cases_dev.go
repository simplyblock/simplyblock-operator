// The device class and inventory shape cases, §2 of the document.
//
// Each is a worker whose disks differ in one way from the four free NVMe disks
// a draft is ordinarily built from: another class, another bus, another kind of
// entry in the block layer, or a count at the edge of what the schema holds.

package main

import (
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// blockClass is the filter that makes a run scan logical block devices, on a
// run that has already decided about the journal.
//
// forceJournalDevice is set because a fleet of identical disks is the ordinary
// fleet, and identical disks say nothing about which one carries the journal: a
// block run over one is refused rather than guessed at, and every case using
// this helper is about something else — which devices reach the draft, how they
// are grouped, what a held device does. Without it they would all record the
// same refusal and none of them would test its own subject.
//
// blockClassUndecided is the run that has not decided, and the case that is
// about the refusal itself uses it.
func blockClass() *simplyblockv1alpha2.DiscoverSpec {
	spec := blockClassUndecided()
	spec.ForceJournalDevice = ptr.To(true)
	return spec
}

// blockClassUndecided scans logical block devices and says nothing about the
// journal, which is what an administrator writes the first time.
func blockClassUndecided() *simplyblockv1alpha2.DiscoverSpec {
	return &simplyblockv1alpha2.DiscoverSpec{EnableLogicalBlockDevices: ptr.To(true)}
}

// oneNodeFourNVMe is the same four disks with every one of them on memory node
// 0, for a case that is about the disks rather than about the placement.
func oneNodeFourNVMe() []nodeprobe.Device {
	return []nodeprobe.Device{
		nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
		nvme("nvme1n1", "0000:5f:00.0", 0, 3*tb),
		nvme("nvme2n1", "0000:60:00.0", 0, 3*tb),
		nvme("nvme3n1", "0000:61:00.0", 0, 3*tb),
	}
}

// virtioDisks is n virtio disks, which have a path and no PCI address.
func virtioDisks(n int) []nodeprobe.Device {
	out := make([]nodeprobe.Device, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, blk(fmt.Sprintf("vd%c", 'b'+rune(i)), 2*tb))
	}
	return out
}

// manyNVMe is n disks in consecutive slots on one memory node.
func manyNVMe(n int, size uint64) []nodeprobe.Device {
	out := make([]nodeprobe.Device, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, nvme(
			fmt.Sprintf("nvme%dn1", i), fmt.Sprintf("0000:%02x:00.0", 0x5e+i), 0, size))
	}
	return out
}

func devCases() map[string]Case {
	single := cpu(1, 16, 2)

	cases := map[string]Case{
		"DEV-01": {
			Family: "dev", Slug: "four-nvme-disks",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(oneNodeFourNVMe()...))},
		},
		"DEV-02": {
			Family: "dev", Slug: "four-block-disks",
			Discover: blockClass(),
			Reports:  []nodeprobe.Report{host("worker-01", single, disks(virtioDisks(4)...))},
		},
		"DEV-03": {
			Family: "dev", Slug: "block-disks-on-an-nvme-run",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(virtioDisks(4)...))},
		},
		"DEV-04": {
			Family: "dev", Slug: "mixed-classes-on-an-nvme-run",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
				nvme("nvme1n1", "0000:5f:00.0", 0, 3*tb),
				blk("vdb", 2*tb),
				blk("vdc", 2*tb),
			))},
		},
		"DEV-05": {
			Family: "dev", Slug: "nvme-and-virtio-on-a-block-run", Gap: "G-1",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
				nvme("nvme1n1", "0000:5f:00.0", 0, 3*tb),
				blk("vdb", 2*tb),
				blk("vdc", 2*tb),
			))},
		},
		"DEV-06": {
			Family: "dev", Slug: "one-controller-two-namespaces",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, tb),
				nvme("nvme0n2", "0000:5e:00.0", 0, tb),
				nvme("nvme1n1", "0000:5f:00.0", 0, tb),
			))},
		},
		"DEV-07": {
			Family: "dev", Slug: "a-sata-disk-beside-an-nvme-one",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
				blk("sda", 4*tb, transported(blockdev.TransportSATA)),
			))},
		},
		"DEV-08": {
			Family: "dev", Slug: "a-spinning-disk-beside-an-ssd", Gap: "G-2",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
				nvme("nvme1n1", "0000:5f:00.0", 0, 16*tb, spinning()),
			))},
		},
		"DEV-09": {
			Family: "dev", Slug: "ten-nvme-disks",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(manyNVMe(10, 3*tb)...))},
		},
		"DEV-10": {
			Family: "dev", Slug: "partitions-and-loopbacks-beside-disks",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(append(
				oneNodeFourNVMe(),
				nvme("nvme0n1p1", "0000:5e:00.0", 0, 512*gb, partOf()),
				blk("loop0", 64*gb, looped()),
				blk("loop1", 64*gb, looped()),
			)...))},
		},
		"DEV-11": {
			Family: "dev", Slug: "a-worker-at-the-selection-ceiling",
			Note:    "128 is the MaxItems of a group's device selection, so this is the largest worker the schema can describe.",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(manyNVMe(128, 512*gb)...))},
		},
		"DEV-12": {
			Family: "dev", Slug: "a-disk-that-reports-no-size",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 0),
				nvme("nvme1n1", "0000:5f:00.0", 0, 0),
			))},
		},
		"DEV-13": {
			Family: "dev", Slug: "an-attached-simplyblock-volume",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				attachedVolume("nvme3n1", "792e184c-0a1b-2c3d-4e5f-60718293a4b5"),
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
			))},
		},
		"DEV-14": {
			Family: "dev", Slug: "an-attached-volume-on-a-block-run",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				attachedVolume("nvme3n1", "792e184c-0a1b-2c3d-4e5f-60718293a4b5"),
				blk("vdb", 2*tb),
				blk("vdc", 4*tb),
			))},
		},
		"DEV-15": {
			Family: "dev", Slug: "every-disk-is-an-attached-volume",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				attachedVolume("nvme3n1", "792e184c-0a1b-2c3d-4e5f-60718293a4b5"),
				attachedVolume("nvme4n1", "8a3f0b12-3c4d-5e6f-7081-92a3b4c5d6e7"),
				attachedVolume("nvme5n1", "b1c2d3e4-f506-1728-394a-5b6c7d8e9f01"),
			))},
		},
		"DEV-17": {
			Family: "dev", Slug: "an-iscsi-lun-nobody-named",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				iscsiLUN("sdb", 2*tb),
				blk("vdb", 2*tb),
				blk("vdc", 4*tb),
			))},
		},
		"DEV-18": {
			Family: "dev", Slug: "an-iscsi-lun-beside-local-disks-taken-as-well",
			Discover: &simplyblockv1alpha2.DiscoverSpec{
				EnableLogicalBlockDevices: ptr.To(true),
				PreferLocalDisks:          ptr.To(false),
				ForceJournalDevice:        ptr.To(true),
			},
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				iscsiLUN("sdb", 2*tb),
				blk("vdb", 2*tb),
			))},
		},
		"DEV-22": {
			Family: "dev", Slug: "a-raid-array-over-the-local-disks",
			// Undecided about the journal, because a worker whose storage is
			// one array has nothing to dedicate: the journal is carved from
			// the array, as it is from a single disk.
			Discover: blockClassUndecided(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				mdArray("md0", "raid1", 2*tb, "sdb", "sdc"),
				blk("sdb", 2*tb, transported(blockdev.TransportSATA), heldBy("md0")),
				blk("sdc", 2*tb, transported(blockdev.TransportSATA), heldBy("md0")),
			))},
		},
		"DEV-23": {
			Family: "dev", Slug: "a-multipath-lun-over-two-iscsi-paths",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				multipathMap("dm-0", 4*tb, "sdb", "sdc"),
				blk("sdb", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
				blk("sdc", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
				multipathMap("dm-1", 2*tb, "sdd", "sde"),
				blk("sdd", 2*tb, transported(blockdev.TransportISCSI), heldBy("dm-1")),
				blk("sde", 2*tb, transported(blockdev.TransportISCSI), heldBy("dm-1")),
			))},
		},
		"DEV-24": {
			Family: "dev", Slug: "an-array-and-a-map-beside-local-disks",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				blk("vdb", 2*tb),
				blk("vdc", 2*tb),
				blk("vdd", 1*tb),
				mdArray("md0", "raid0", 4*tb, "sdb", "sdc"),
				blk("sdb", 2*tb, transported(blockdev.TransportSATA), heldBy("md0")),
				blk("sdc", 2*tb, transported(blockdev.TransportSATA), heldBy("md0")),
				multipathMap("dm-0", 4*tb, "sdd", "sde"),
				blk("sdd", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
				blk("sde", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
			))},
		},
		"DEV-25": {
			Family: "dev", Slug: "the-same-worker-with-local-disks-not-preferred",
			Discover: &simplyblockv1alpha2.DiscoverSpec{
				EnableLogicalBlockDevices: ptr.To(true),
				PreferLocalDisks:          ptr.To(false),
				ForceJournalDevice:        ptr.To(true),
			},
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				blk("vdb", 2*tb),
				blk("vdc", 2*tb),
				blk("vdd", 1*tb),
				mdArray("md0", "raid0", 4*tb, "sdb", "sdc"),
				blk("sdb", 2*tb, transported(blockdev.TransportSATA), heldBy("md0")),
				blk("sdc", 2*tb, transported(blockdev.TransportSATA), heldBy("md0")),
				multipathMap("dm-0", 4*tb, "sdd", "sde"),
				blk("sdd", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
				blk("sde", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
			))},
		},
		"DEV-26": {
			Family: "dev", Slug: "an-array-on-an-nvme-run",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				// md127 is the number the kernel hands an array it assembled
				// itself from the superblocks, as it does at boot.
				mdArray("md127", "raid1", 2*tb, "nvme0n1", "nvme1n1"),
				nvme("nvme0n1", "0000:5e:00.0", 0, 2*tb, heldBy("md127")),
				nvme("nvme1n1", "0000:5f:00.0", 0, 2*tb, heldBy("md127")),
			))},
		},
		"DEV-27": {
			Family: "dev", Slug: "a-logical-volume-beside-a-multipath-map",
			Discover: blockClass(),
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				multipathMap("dm-0", 4*tb, "sdb", "sdc"),
				blk("sdb", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
				blk("sdc", 4*tb, transported(blockdev.TransportISCSI), heldBy("dm-0")),
				logicalVolume("dm-1", 2*tb, "sdd"),
				blk("sdd", 2*tb, transported(blockdev.TransportSATA), heldBy("dm-1")),
				blk("vdb", 2*tb),
			))},
		},
		"DEV-19": {
			Family: "dev", Slug: "an-iscsi-lun-on-an-nvme-run",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				iscsiLUN("sdb", 2*tb),
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
			))},
		},
		"DEV-21": {
			Family: "dev", Slug: "blank-formatted-and-unread-partitions-block-run",
			Note: "Driven in the discovery package by TestPlanProposesABlankPartitionInABlockRun, " +
				"which builds the partitions' probe readings directly.",
		},
		"DEV-20": {
			Family: "dev", Slug: "equal-block-disks-with-no-journal-decision",
			// The one block case that does not force, which is what makes it
			// about the journal split. DEV-02 is the same four disks and
			// dedicates one, because its run decided.
			Discover: blockClassUndecided(),
			Reports:  []nodeprobe.Report{host("worker-01", single, disks(virtioDisks(4)...))},
			Note: "The ordinary fleet: four disks of one size, which is how machines are " +
				"bought. Nothing about them says which carries the journal, and taking " +
				"one spends a disk the fleet did not set aside. The run leaves " +
				"enableJournalDevice unset, and the node's configure carves the journal " +
				"partition out of the smallest disk, so every disk keeps carrying data. " +
				"The note names forceJournalDevice for a run that wants a disk dedicated.",
		},
		"DEV-16": {
			Family: "dev", Slug: "a-fabric-namespace-of-another-product",
			Reports: []nodeprobe.Report{host("worker-01", single, disks(
				foreignVolume("nvme3n1"),
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
			))},
		},
	}

	// Every case here is about disks, so none of them says anything about the
	// node objects, and all of them need one: a document naming a worker the
	// cluster does not have is a document its own validation rejects.
	for id, c := range cases {
		c.Nodes = kubeFleet(c.Reports)
		cases[id] = c
	}
	return cases
}

// ourCluster is the cluster the attached volumes below belong to, which is what
// a refusal names.
const ourCluster = "c30a691a-1d2e-4f3a-9b8c-5d6e7f809a1b"

// attachedVolume is a simplyblock volume as the worker that connected it sees
// one: a namespace on a fabric, carrying the NQN this product builds.
//
// The probe refuses it for being on a fabric, which is what a real report
// carries, and the rule that names it as this fleet's own reads the NQN rather
// than the refusal.
func attachedVolume(name, volumeID string) nodeprobe.Device {
	device := blk(name, tb, refused(blockdev.ReasonFabricNamespace))
	device.Transport = string(blockdev.TransportNVMeFabric)
	device.SubsystemNQN = nqn.Make(ourCluster, volumeID)
	return device
}

// iscsiLUN is a disk on the other side of a network, which the kernel presents
// through the SCSI stack like any local disk.
func iscsiLUN(name string, size uint64) nodeprobe.Device {
	device := blk(name, size)
	device.Transport = string(blockdev.TransportISCSI)
	return device
}

// logicalVolume is the device-mapper node of an LVM logical volume, which
// looks like a multipath map in class/block and is told apart by what built it.
func logicalVolume(name string, size uint64, members ...string) nodeprobe.Device {
	device := multipathMap(name, size, members...)
	device.DMSubsystem = blockdev.DMSubsystemLVM
	return device
}

// foreignVolume is a namespace something else exported, which is on a fabric
// and is nobody's simplyblock volume.
func foreignVolume(name string) nodeprobe.Device {
	device := blk(name, tb, refused(blockdev.ReasonFabricNamespace))
	device.Transport = string(blockdev.TransportNVMeFabric)
	device.SubsystemNQN = "nqn.2019-08.org.ceph:rbd.pool.image"
	return device
}
