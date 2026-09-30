// Taking a fleet's idle NVMe controllers back from a userspace driver.
//
// This is BindTo applied to every controller of a machine, and it exists as its
// own call because the interesting part is not the rebind but which controllers
// are left alone. A machine prepared for SPDK and a machine running SPDK look
// identical in sysfs: both have their NVMe controllers on uio_pci_generic or
// vfio-pci, and the only thing separating them is whether a process holds the
// character devices open. Reclaiming the first costs nothing. Reclaiming the
// second takes a storage node's disks out from under it mid-IO.
//
// So nothing here decides that question: it is answered by CheckHolders, which
// walks the host's process table, and a controller nobody asked about is
// refused rather than reclaimed. Not knowing is not permission — a caller in a
// pod without the host's PID namespace sees only its own processes, and would
// conclude that nothing holds anything.
//
// A controller the kernel already drives is not this call's business, and is
// neither reclaimed nor refused: there is no userspace driver to take it from,
// and unbinding it to bind it back again would interrupt a device that is
// working.

package pci

import (
	"errors"
	"fmt"
)

// Refusal is one controller a reclaim left alone, and why.
type Refusal struct {
	// Address is the controller's slot.
	Address string

	// Reason completes a left-alone-because sentence, and names what is driving
	// the device where something is: the next question after a refusal is
	// always which process.
	Reason string
}

// String renders a refusal for a message.
func (r Refusal) String() string { return r.Address + ": " + r.Reason }

// Reclamation is what one pass over a machine gave back, and what it did not.
type Reclamation struct {
	// Reclaimed is the addresses handed back to the kernel, ascending in the
	// order they were given.
	Reclaimed []string

	// Refused is every controller that was bound to a userspace driver and was
	// left alone anyway, with the ground for each.
	Refused []Refusal
}

// Reclaim hands back to the kernel every NVMe controller in devices that a
// userspace driver holds and that nothing is driving.
//
// The devices are a caller's scan, so the caller decides what is in scope, and
// they should have been through CheckHolders: a controller whose holders were
// never established is refused, which is what makes passing an unchecked scan
// safe rather than silently permissive.
//
// A refusal is not an error. The common machine has some controllers to give
// back and some to leave, and a call that failed on the first held one would
// leave the rest bound with no way to tell how far it got. The error return is
// for a broken sysfs, which is a fact about the machine rather than about any
// one device.
func Reclaim(cfg Config, devices []Device) (Reclamation, error) {
	var out Reclamation
	for _, device := range devices {
		if !device.IsNVMe() || !device.BoundToUserspace() {
			continue
		}

		// Unchecked is its own answer, and it is the one case this decides.
		// A device nobody asked about is not a device nothing is using: a
		// caller without the host's PID namespace sees only its own processes,
		// so treating silence as idle would reclaim a controller an
		// application is running on.
		if device.InUse == nil {
			out.Refused = append(out.Refused, Refusal{
				Address: device.Address,
				Reason: "whether anything is driving it was never established, and not " +
					"knowing is not permission to take it",
			})
			continue
		}

		// Everything else goes through BindTo, held ones included, because it
		// re-reads the holders itself and its refusal names the process. The
		// two readings are moments apart and that window is exactly when an
		// application starts, so the guard belongs at the write rather than
		// here.
		err := BindTo(cfg, device.Address, "")
		switch {
		case err == nil:
			out.Reclaimed = append(out.Reclaimed, device.Address)
		case errors.Is(err, ErrDeviceHeld):
			out.Refused = append(out.Refused, Refusal{
				Address: device.Address,
				Reason:  err.Error(),
			})
		default:
			return out, fmt.Errorf("pci: reclaim %s: %w", device.Address, err)
		}
	}
	return out, nil
}
