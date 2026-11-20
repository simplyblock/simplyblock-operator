// A controller nothing owns is still a disk.
//
// A controller has four states and only two of them present a block device:
// the kernel's driver has it, a userspace driver has it, nothing has it, or the
// probe could not say. The third is what a failed run leaves behind — binding an
// NVMe controller to a userspace driver takes its namespaces from the kernel,
// and a run that unbinds and then fails leaves it owned by nothing at all.

package discovery

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// TestAControllerNothingOwnsIsOffered covers the disks a re-run has to find
// again.
//
// Regression: 2026-09-20-unbound-controllers-vanish-from-discovery —
// claimableControllers offered a controller only when it was bound to a
// userspace driver and idle, so one bound to nothing was dropped: it presents no
// block device, so the device reading does not have it either, and the disk was
// absent from the draft entirely. Five of six workers of the lab fleet were
// reported as having nothing after the day's failed adds unbound their Microns,
// and the cluster activated on the one machine whose add had completed.
//
// Nothing can be driving a controller with no driver, which is what makes it
// offerable without asking who holds it: there is no character device to hold.
func TestAControllerNothingOwnsIsOffered(t *testing.T) {
	worker := report("worker-0")
	worker.NVMeControllers = []nodeprobe.Controller{
		// The state a failed add leaves: the capture of worker-0 has two of
		// these and one kernel-driven boot disk.
		{Address: "0000:01:00.0", NUMANode: -1},
		{Address: "0000:0b:00.0", NUMANode: -1},
		{Address: "0000:02:00.0", Driver: "nvme", NUMANode: -1},
	}

	offered := claimableControllers(worker, ClassNVMe)

	named := map[string]bool{}
	for _, device := range offered {
		named[device.PCIAddress] = true
	}
	for _, address := range []string{"0000:01:00.0", "0000:0b:00.0"} {
		if !named[address] {
			t.Errorf("the controller at %s is not offered, so a disk nothing owns is invisible", address)
		}
	}
	if named["0000:02:00.0"] {
		t.Error("the kernel-driven controller was offered a second time; the device reading already has it")
	}
}

// A userspace binding is the other reclaimable state, and it still turns on
// whether anything is driving it.
func TestAUserspaceBindingIsOfferedOnlyWhenIdle(t *testing.T) {
	idle := report("worker-1")
	idle.NVMeControllers = []nodeprobe.Controller{
		{Address: "0000:00:02.0", Driver: "uio_pci_generic", InUse: ptr.To(false), NUMANode: -1},
	}
	if len(claimableControllers(idle, ClassNVMe)) != 1 {
		t.Error("an idle userspace binding is not offered")
	}

	busy := report("worker-2")
	busy.NVMeControllers = []nodeprobe.Controller{
		{Address: "0000:00:04.0", Driver: "vfio-pci", InUse: ptr.To(true), NUMANode: -1},
	}
	if len(claimableControllers(busy, ClassNVMe)) != 0 {
		t.Error("a controller something is driving was offered; it may be a guest's disk")
	}

	unchecked := report("worker-3")
	unchecked.NVMeControllers = []nodeprobe.Controller{
		{Address: "0000:00:05.0", Driver: "vfio-pci", NUMANode: -1},
	}
	if len(claimableControllers(unchecked, ClassNVMe)) != 0 {
		t.Error("a controller the probe could not check was offered; unchecked is not idle")
	}
}

// A controller the kernel is presenting is left to the device reading, which
// knows its size, its content and whether anything is mounted on it.
func TestAPresentedControllerIsNotOfferedTwice(t *testing.T) {
	worker := report("worker-4")
	worker.NVMeControllers = []nodeprobe.Controller{{Address: "0000:01:00.0", NUMANode: -1}}
	worker.Devices = []nodeprobe.Device{{
		Name: "nvme0n1", PCIAddress: "0000:01:00.0", Kind: "Disk", Transport: "NVMe", Available: true,
	}}

	if got := claimableControllers(worker, ClassNVMe); len(got) != 0 {
		t.Errorf("offered %d controller(s) the kernel already presents as block devices", len(got))
	}
}
