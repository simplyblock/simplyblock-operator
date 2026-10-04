// Waiting for the kernel to finish taking a controller back.
//
// This is here because a bind returns before the disks exist. Writing a PCI
// address to drivers_probe hands the controller over and returns; the NVMe
// driver then resets the device, reads its identify data, and creates a block
// device per namespace, and only then is there a disk for a reading to find.
// A collection that reclaimed a controller and read the disks in that window
// reports exactly the absence it reclaimed the controller to cure.

package inventory

import (
	"path/filepath"
	"time"

	"github.com/simplyblock/atlas/pci"
)

// DefaultReclaimSettle is how long a collection waits for the disks behind a
// reclaimed controller, when Config.ReclaimSettle says nothing.
//
// It is a ceiling and not a delay: the wait ends as soon as every reclaimed
// controller has a namespace, which on a healthy controller is well under a
// second. The ceiling is what a controller that never comes back costs, and it
// is per collection rather than per controller.
const DefaultReclaimSettle = 5 * time.Second

// reclaimPoll is how often the wait looks. Short enough that the common case is
// not rounded up to something a reader would notice.
const reclaimPoll = 50 * time.Millisecond

// reclaimSettle is the configured ceiling, or the default when none was given.
func (c Config) reclaimSettle() time.Duration {
	if c.ReclaimSettle == 0 {
		return DefaultReclaimSettle
	}
	return c.ReclaimSettle
}

// waitForReclaimed waits until every reclaimed controller presents a namespace,
// or until the ceiling passes.
//
// Running out of time is not an error and is not reported here. The disks the
// kernel did produce are read either way, and a controller that produced none
// is already visible as what it is: a controller this collection handed back
// that no device names.
func waitForReclaimed(sysfsRoot string, reclaimed []pci.Device, settle time.Duration) {
	if settle <= 0 || len(reclaimed) == 0 {
		return
	}

	deadline := time.Now().Add(settle)
	for {
		if enumerated(sysfsRoot, reclaimed) {
			return
		}
		if !time.Now().Add(reclaimPoll).Before(deadline) {
			return
		}
		time.Sleep(reclaimPoll)
	}
}

// enumerated reports whether every controller has at least one namespace.
//
// The namespace directory is the signal rather than the driver link, because
// the link appears when the driver is bound and the disk appears when the
// driver has finished with the device, and it is the disk a reading needs.
func enumerated(sysfsRoot string, reclaimed []pci.Device) bool {
	for _, device := range reclaimed {
		matches, err := filepath.Glob(
			filepath.Join(sysfsRoot, "bus", "pci", "devices", device.Address, "nvme", "nvme*", "nvme*n*"))
		if err != nil || len(matches) == 0 {
			return false
		}
	}
	return true
}
