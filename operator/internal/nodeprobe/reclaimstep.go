// The one thing the probe does that changes the worker.
//
// A logical block-device deployment reaches a disk through the kernel, so a
// controller a userspace driver holds presents no block device and is invisible
// to a block-mode run: a fleet prepared for SPDK reports no storage at all, and
// the reason is a binding rather than an absence.
//
// Reclaiming those controllers is what this does, and the whole of the care is
// in which ones it leaves. A machine prepared for SPDK and a machine running
// SPDK are identical in sysfs, and the only evidence separating them is whether
// a process holds the character devices open — so the holders are read from the
// host's process table first, and a controller nobody could ask about is left
// alone. atlas/pci carries both halves and re-reads the holders at the write,
// which is where the window between asking and acting closes.
//
// What it did is recorded on the report rather than only logged. The reclaim
// happens before the machine is read, so a report that carried only the result
// would describe a worker whose disks appeared for reasons nothing in it
// explains.

package nodeprobe

import (
	"fmt"

	"github.com/simplyblock/atlas/pci"
)

// ReclaimRefusal is one controller a reclaim left bound, and why.
type ReclaimRefusal struct {
	// Address is the controller's slot.
	Address string `json:"address"`

	// Reason names what is driving the device where something is, because the
	// next question after a refusal is always which process.
	Reason string `json:"reason"`
}

// Reclaim is what one probe's reclaim pass gave back to the kernel, and what it
// did not.
//
// A run that did not reclaim carries none of this rather than an empty one: a
// pass that found nothing to take and a run that never looked are different
// findings, and only the first says anything about the machine.
type Reclaim struct {
	// Reclaimed is the controllers handed back, by address.
	Reclaimed []string `json:"reclaimed,omitempty"`

	// Refused is every controller that was on a userspace driver and was left
	// there anyway, with the ground for each.
	Refused []ReclaimRefusal `json:"refused,omitempty"`
}

// ReclaimUserspace hands back to the kernel every NVMe controller a userspace
// driver holds and nothing is driving, and reports what it did.
//
// The holders are established before anything is written, and a controller
// whose holders could not be read is refused: a probe without the host's PID
// namespace sees only its own processes, and reading that silence as idle would
// take a running application's disks out from under it.
//
// A machine with nothing bound answers an empty pass rather than nothing at
// all, because the run did look.
func ReclaimUserspace(cfg pci.Config) (*Reclaim, error) {
	devices, err := pci.Scan(cfg)
	if err != nil {
		return nil, fmt.Errorf("read the PCI controllers to reclaim: %w", err)
	}

	// The holder check's own failures are not fatal. It reports what it could
	// not establish by leaving InUse unset, and the reclaim refuses exactly
	// those, so a machine whose process table is partly unreadable gives back
	// the controllers it could account for and says so about the rest.
	controllers, _ := pci.CheckHolders(cfg, pci.NVMeControllers(devices))

	done, err := pci.Reclaim(cfg, controllers)
	if err != nil {
		return nil, fmt.Errorf("reclaim the userspace-bound controllers: %w", err)
	}

	out := &Reclaim{Reclaimed: done.Reclaimed}
	for _, refusal := range done.Refused {
		out.Refused = append(out.Refused, ReclaimRefusal{
			Address: refusal.Address,
			Reason:  refusal.Reason,
		})
	}
	return out, nil
}
