// That the scan names an iSCSI LUN for what it is.
//
// An iSCSI disk reaches the kernel through the SCSI stack, so every marker the
// transport reading looks for says SCSI: it hangs off a hostN, it is addressed
// as a SCSI target, and it presents an sdX. What separates it from a disk in
// the machine is one segment of its device path, the session the SCSI transport
// class creates for it, and without reading that a LUN on the other side of a
// network is indistinguishable from a disk on a cable inside the chassis.

package blockdev

import "testing"

// iscsiHost is a worker with one local SAS disk and one iSCSI LUN, which is the
// pair the reading has to tell apart.
func iscsiHost() tree {
	const (
		local = "devices/pci0000:00/0000:18:00.0/host0/end_device-0:0/target0:0:0/0:0:0:0/block/sda"
		lun   = "devices/platform/host6/session1/target6:0:0/6:0:0:0/block/sdb"
	)
	t2 := tree{files: map[string]string{}, links: map[string]string{}}

	t2.blockAttrs(local, "8:0", 3125627568, true, false, false)
	t2.links["class/block/sda"] = "../../" + local

	t2.blockAttrs(lun, "8:16", 2097152000, false, false, false)
	t2.links["class/block/sdb"] = "../../" + lun

	t2.files["self/mountinfo"] = "25 1 0:24 / / rw - overlay overlay rw"
	t2.files["swaps"] = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
	return t2
}

func TestScanNamesAnISCSILUNForWhatItIs(t *testing.T) {
	root := iscsiHost().write(t)

	disks, err := Scan(ScanConfig{SysfsRoot: root})
	if err != nil {
		t.Fatalf("scan the block devices: %v", err)
	}

	if got := scanned(t, disks, "sdb").Transport; got != TransportISCSI {
		t.Errorf("the LUN reads as %q, want %q", got, TransportISCSI)
	}
	if got := scanned(t, disks, "sda").Transport; got != TransportSAS {
		t.Errorf("the local disk reads as %q, want %q", got, TransportSAS)
	}
}
