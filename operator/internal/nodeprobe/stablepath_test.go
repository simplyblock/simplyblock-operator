// That a device's persistent name survives the report.
//
// The report is the only thing the operator sees of a worker: the probe runs on
// the machine, writes what it found, and is gone by the time a draft is
// assembled. A persistent name the probe read and did not write down is one
// nothing downstream can recover, because the /dev/disk tree it came from is on
// a machine the operator never looks at.

package nodeprobe

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
)

func TestTheReportCarriesADevicesPersistentName(t *testing.T) {
	const stable = "/dev/disk/by-id/nvme-SAMSUNG_MZQL23T8HCLS-00A07_S6CVNE0T500123"

	candidate := oneFreeDisk()
	candidate.StablePath = stable

	report := FromInventory(testNode, probedAt, inventory.Inventory{
		Devices: []blockdev.Candidate{candidate},
	}, nil)

	if len(report.Devices) != 1 {
		t.Fatalf("rendered %d devices, want 1", len(report.Devices))
	}
	if got := report.Devices[0].StablePath; got != stable {
		t.Errorf("reported the persistent name %q, want %q", got, stable)
	}

	// The wire form is what actually reaches the operator, and a field the
	// translation kept but the encoding dropped would look identical here.
	encoded, err := Encode(report)
	if err != nil {
		t.Fatalf("encode the report: %v", err)
	}
	if !strings.Contains(string(encoded), stable) {
		t.Error("the encoded report does not carry the persistent name")
	}
}

// A device udev published no link for reports none, rather than its kernel
// path. The two are different findings: one is a name that outlives a reboot,
// and the other is this boot's enumeration order, and a reader that cannot tell
// them apart would treat every device as persistently named.
func TestADeviceWithNoPersistentNameReportsNone(t *testing.T) {
	report := FromInventory(testNode, probedAt, inventory.Inventory{
		Devices: []blockdev.Candidate{oneFreeDisk()},
	}, nil)

	if got := report.Devices[0].StablePath; got != "" {
		t.Errorf("reported the persistent name %q for a device that has none", got)
	}
}
