// Package pci is what PCI devices a machine has and which driver owns each.
//
// It exists because of one blind spot. A discovery run looks for NVMe disks by
// enumerating block devices, and an NVMe controller bound to a userspace-IO
// driver has no block device at all: SPDK takes a controller by rebinding it
// from the kernel's NVMe driver to uio_pci_generic or vfio-pci, and from that
// moment the kernel presents nothing under class/block for it. A scan of the
// block devices reports "this worker has no NVMe disks" about a machine with
// four of them.
//
// That is not a corner case. It is the state of every worker simplyblock is
// already running on, and of every worker a previous deployment left behind. On
// the fleet this was written against, three of four workers had four NVMe
// controllers each on uio_pci_generic and not one NVMe block device between
// them.
//
// So the PCI view is the second half of the answer, and the two are read
// together:
//
//	devices, err := pci.Scan(pci.Config{})
//	for _, d := range devices {
//	    if !d.IsNVMe() {
//	        continue
//	    }
//	    switch {
//	    case d.Driver == pci.DriverNVMe:
//	        // The kernel presents a namespace; the blockdev scan covers it.
//	    case d.BoundToUserspace():
//	        // A disk something took for userspace IO. Not a candidate, but
//	        // visible, with a reason a person can act on.
//	    case d.Driver == "":
//	        // A disk no driver owns. It has no block device, so its content
//	        // cannot be read, and it must not be handed over unread.
//	    }
//	}
//
// # Reading is separate from changing
//
// [Scan] and [HeldBy] read. [BindTo] and [Unbind] change, and they live in
// rebind.go behind a guard, because giving a controller back to the kernel while
// SPDK is driving it would take the storage node's disks out from under it.
// Nothing in a discovery run calls them: a run inspects what is there and writes
// what it found, and reclaiming a controller is a decision somebody makes
// afterward.
//
// # There is no busy flag to read
//
// Whether a userspace driver is actually driving a device is not in sysfs. The
// uio driver exports name, version, and event, and none of them changes while a
// process holds the character device — measured, not assumed: opening /dev/uio0
// and diffing the directory across the open showed no difference. The only
// evidence is a process holding the device open, which [HeldBy] finds by walking
// /proc, and which therefore needs the host's PID namespace to see anything but
// its own.
package pci
