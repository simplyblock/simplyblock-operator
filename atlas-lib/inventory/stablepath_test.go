// Which persistent name a real machine's devices are read under.
//
// The preference order is checked in blockdev against trees written by hand,
// which proves the ranking agrees with itself. This checks it against machines:
// the links below are what udev actually created on two workers, with every
// alternative it created beside them, and the assertions name the link that is
// the device's own identity rather than somebody's account of it.
//
// That distinction is the whole reason the file exists. A hand-written tree
// contains the links its author thought of, and the author of the ranking is
// the same person. Neither of the hosts below has a single wwn- link, which was
// the first thing the ranking preferred; both offer three or four names per
// device instead, and only one of them is read off the device.

package inventory

import (
	"path/filepath"
	"testing"
)

const (
	// passthroughWorker has two Samsung SSDs passed through to the guest, so
	// its namespaces carry an EUI, and a QEMU SCSI boot disk beside them.
	passthroughWorker = "okd-worker-passthrough-nvme"

	// scsiWorker is a worker of the running lab: four QEMU SCSI disks, one of
	// them the partitioned boot disk, and two namespaces this product exported
	// and the node has attached.
	//
	// It is the fixture that makes the case for the whole change. The kernel
	// calls its boot disk sdb and the hypervisor calls it drive-scsi0; sda is
	// drive-scsi2. Nothing about a name in one of those two orders predicts the
	// name in the other, and only one of them survives a reboot.
	scsiWorker = "okd-worker-scsi-disks"
)

// devicesByName reads the capture's block devices and indexes their persistent
// names by kernel name.
//
// The disk reading is taken rather than a whole Collect, because the rest of a
// collection would drag in what a capture cannot always answer -- the
// passthrough worker's predates the os-release section, so the OS reading fails
// on it -- and a test of the device names would report that instead.
func devicesByName(t *testing.T, host string) map[string]string {
	t.Helper()

	root := hostFixture(t, host)
	cfg := syntheticHost(root)
	cfg.DevRoot = devRootOf(root)

	devices, err := cfg.inspector().Candidates(t.Context())
	if err != nil {
		t.Fatalf("read the block devices: %v", err)
	}

	paths := map[string]string{}
	for _, device := range devices {
		paths[device.Name] = device.StablePath
	}
	return paths
}

// linksAre checks a host's devices against the by-id or by-partuuid link each
// one is expected to be read under.
func linksAre(t *testing.T, host string, want map[string]string) {
	t.Helper()

	paths := devicesByName(t, host)
	dev := devRootOf(hostFixture(t, host))
	for name, link := range want {
		if got := paths[name]; got != filepath.Join(dev, "disk", link) {
			t.Errorf("%s reads under %q, want %s", name, got, link)
		}
	}
}

// An NVMe namespace is read under the identifier it reports for itself: the EUI
// where the drive is real hardware, and the namespace UUID where this product
// exported it.
//
// The alternatives udev created for the same namespace are built from the
// controller's model and serial, and they are the controller's: the passthrough
// worker's drives each publish a bare model-and-serial link and a second one
// with an index appended, and the attached namespaces publish three names
// between two devices, two of which differ only by that index. An index
// counting the namespaces in the order they were found is the same kind of
// ordering the kernel name already is.
func TestAnNVMeNamespaceIsReadUnderTheIdentifierItReports(t *testing.T) {
	linksAre(t, passthroughWorker, map[string]string{
		"nvme0n1": "by-id/nvme-eui.34333930547014240025384300000001",
		"nvme1n1": "by-id/nvme-eui.34333930547013130025384300000001",
	})
	linksAre(t, scsiWorker, map[string]string{
		"nvme8n1": "by-id/nvme-uuid.8a695f15-8227-4b90-a0d5-66b9e72b496f",
		"nvme8n2": "by-id/nvme-uuid.1b68906b-c6f1-4c29-b2eb-543b2cbf95ea",
	})
}

// A partition is read under the identifier in its own partition table, which is
// the one name it has that does not contain its parent disk's.
//
// Both by-id alternatives do contain it: they are the disk's link with a -partN
// suffix, so a hypervisor re-exporting the volume under another drive id
// renames every one of them at once. The partition table is on the device.
func TestAPartitionIsReadUnderItsPartitionTableIdentifier(t *testing.T) {
	linksAre(t, scsiWorker, map[string]string{
		"sdb1": "by-partuuid/5a64da5d-03c1-4b8d-bad9-e1a137142ed6",
		"sdb2": "by-partuuid/1cdde0bf-9102-42ed-b091-392f047424e8",
		"sdb3": "by-partuuid/c76a09ee-afa6-48b4-a265-87f4d483fe3d",
		"sdb4": "by-partuuid/28427de0-1916-4c05-895f-0829cd8790ba",
	})
}

// A whole disk with no identifier of its own is read under the name the
// hypervisor gave it, which is all there is.
//
// This is the case the change exists for. The kernel calls these four disks
// sda, sdb, sdc, and sdd in the order it found them, and the hypervisor calls
// them drive-scsi2, drive-scsi0, drive-scsi1, and drive-scsi3. A deployment
// that recorded the first spelling would, after a reboot that probed the
// controllers in another order, hand a storage node a different disk than the
// one it was given -- and nothing would say so, because both names exist and
// both resolve.
func TestAWholeDiskIsReadUnderTheNameItsHypervisorGaveIt(t *testing.T) {
	linksAre(t, scsiWorker, map[string]string{
		"sda": "by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2",
		"sdb": "by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0",
		"sdc": "by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1",
		"sdd": "by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi3",
	})
}

// No device is read under a by-path link. It names the slot the device is
// plugged into, so it survives a reboot and moves to the replacement when a
// disk is swapped -- which is the one failure a persistent name exists to
// prevent, arriving under a name that looks like it prevents it.
//
// Both hosts publish one for every disk, and the lab worker publishes one per
// partition too, so a reading that took whatever link it found first would have
// landed on these.
func TestNoDeviceIsReadUnderItsSlot(t *testing.T) {
	for _, host := range []string{passthroughWorker, scsiWorker} {
		for name, path := range devicesByName(t, host) {
			if path == "" {
				continue
			}
			if filepath.Base(filepath.Dir(path)) == "by-path" {
				t.Errorf("%s of %s reads under the slot %q", name, host, path)
			}
		}
	}
}

// A device udev published no link for reads as having no persistent name,
// rather than as having its kernel path. Both hosts present sixteen network
// block devices the kernel made and nothing named.
func TestADeviceWithNoLinkHasNoPersistentName(t *testing.T) {
	paths := devicesByName(t, scsiWorker)
	if got, ok := paths["nbd0"]; !ok {
		t.Fatal("nbd0 is missing from the reading")
	} else if got != "" {
		t.Errorf("nbd0 reads under %q, and udev named it nothing", got)
	}
}
