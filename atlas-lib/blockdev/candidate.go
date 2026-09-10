// Whether a device may be handed to a storage cluster, and the reason for every
// device that may not.
//
// This is where the scan, the usage reading, and the content reading meet, and
// it is the only file that says no. The three of them separately answer what a
// device is, who is using it, and what it carries; a candidate is a device with
// all three answers attached and a list of the grounds on which it was refused.
//
// The reasons are a list rather than the first one found, because a disk that is
// mounted and also carries a partition table is refused on both grounds, and a
// reviewer told only about the partition table would reach for the flag that
// overrides it. That flag is the reason the reasons are named at all: a stale
// partition table is a condition an administrator can decide to ignore, and
// nothing else on the list is.
//
// A device already refused is not opened. The content reading is the one step
// that touches bytes, and a device something else is writing to is a device
// nothing here has any business reading.

package blockdev

import (
	"context"
	"fmt"
	"slices"
)

// Reason is a ground on which a device was refused.
type Reason string

const (
	// ReasonNotAWholeDisk is a partition, a device-mapper node, a software-RAID
	// array, a loop device, or a RAM disk. A cluster is built out of whole
	// disks, and each of these is either a slice of one or not one at all.
	ReasonNotAWholeDisk Reason = "NotAWholeDisk"

	// ReasonFabricNamespace is an NVMe namespace reached over a fabric, which
	// on this product's hosts is a simplyblock volume that is attached to the
	// node. It looks exactly like an unclaimed local disk and it is the
	// opposite of one.
	ReasonFabricNamespace Reason = "FabricNamespace"

	// ReasonUnknownTransport is a whole disk on no bus the scan could name: a
	// virtual block device it does not recognize, or hardware behind a driver it
	// has not been taught. Either way nothing established where the device's
	// bytes come from, and a discovery run refuses what it cannot place.
	//
	// It is not a statement that the device is virtual. The head device of a multipath
	// namespace is virtual and is a disk in a slot in this machine, which is
	// why the transport and not the tree is what this rests on.
	ReasonUnknownTransport Reason = "UnknownTransport"

	// ReasonMounted is a device, or a partition of it, that carries a mounted
	// filesystem.
	ReasonMounted Reason = "Mounted"

	// ReasonSwapArea is a device, or a partition of it, that is active swap.
	ReasonSwapArea Reason = "SwapArea"

	// ReasonStacked is a device another device is built on: the device-mapper
	// node of a volume group that took it, or a RAID array it is a member of.
	ReasonStacked Reason = "Stacked"

	// ReasonBusy is a device the kernel refused to hand over for a reason none
	// of the readable sources named. It is the backstop, and a device refused
	// on this ground alone is one to look at by hand.
	ReasonBusy Reason = "Busy"

	// ReasonUnreadable is a device whose usage or whose content could not be
	// established. It is not a device that was checked and found free, and the
	// difference is the whole reason this package exists.
	ReasonUnreadable Reason = "Unreadable"

	// ReasonReadOnly is a device the kernel presents read-only, which a cluster
	// cannot write to.
	ReasonReadOnly Reason = "ReadOnly"

	// ReasonNoCapacity is a device reporting a size of zero: an unbacked loop
	// device, a card reader with no card in it.
	ReasonNoCapacity Reason = "NoCapacity"

	// ReasonPartitioned is a device carrying a partition table.
	//
	// It is the one reason an administrator can override, for the case where the
	// table is stale and the disk is meant to be handed over anyway, so it is
	// never folded into ReasonNotBlank: a caller allowing partitioned devices
	// needs to distinguish a disk whose only problem is its table from one that
	// also carries a filesystem.
	ReasonPartitioned Reason = "Partitioned"

	// ReasonNotBlank is a device carrying anything else: a filesystem, an LVM
	// physical-volume label, a RAID superblock, or bytes matching no signature
	// this package knows. Candidate.Reading names what was found.
	ReasonNotBlank Reason = "NotBlank"
)

// Rejection is one ground with the evidence for it, so that an event or a log
// line can say what was found and not only that something was.
type Rejection struct {
	// Reason is the ground.
	Reason Reason

	// Detail is the finding in words: which mountpoint, which holder, which
	// signature at which offset.
	Detail string
}

// Candidate is one device with everything known about it, and the grounds on
// which it was refused.
type Candidate struct {
	Disk

	// Usage is what was found to be using the device.
	Usage Usage

	// Reading is what the device was found to carry. It is the zero Reading —
	// ContentUnknown — for a device that was refused before anything was
	// opened, which is deliberate: ContentUnknown authorizes nothing, so a
	// caller cannot mistake a device that was never read for an empty one.
	Reading Reading

	// Rejections is every ground the device was refused on, in the order they
	// were established, and is empty for a device that may be handed over.
	Rejections []Rejection
}

// Available reports whether the device may be handed to a storage cluster.
func (c Candidate) Available() bool {
	return len(c.Rejections) == 0
}

// RejectedFor reports whether reason is among the grounds.
func (c Candidate) RejectedFor(reason Reason) bool {
	return slices.ContainsFunc(c.Rejections, func(r Rejection) bool { return r.Reason == reason })
}

// OnlyRejectedFor reports whether the device was refused, and every ground it
// was refused on is among those given.
//
// This is how an override is applied. A caller that accepts a partitioned disk
// asks OnlyRejectedFor(ReasonPartitioned), and a disk that is also mounted
// answers false, so the override cannot widen into one that hands over a disk
// in use. It is false for a device with no rejections at all, because such a
// device needs no override.
func (c Candidate) OnlyRejectedFor(reasons ...Reason) bool {
	if c.Available() {
		return false
	}
	for _, rejection := range c.Rejections {
		if !slices.Contains(reasons, rejection.Reason) {
			return false
		}
	}
	return true
}

// Inspector collects the candidates of one host.
//
// The zero value works on a Linux host: it scans /sys, reads /proc, builds
// device paths under /dev, and reads content off the devices themselves. The
// fields are there to be replaced, which is how the package is tested and how a
// caller reading a host's tree from somewhere else points it at that tree.
type Inspector struct {
	// Config names the trees the scan and the usage reading are taken from.
	Config ScanConfig

	// Prober reads what each candidate device carries. A nil Prober is the
	// local one, reading devices on this host with the page cache bypassed.
	Prober *Prober

	// Exclusive asks the kernel whether it will hand a device over. A nil
	// Exclusive is OpenExclusive, the real question against the real kernel.
	Exclusive ExclusiveOpener
}

// Candidates reports every block device the host has, with the grounds on which
// each one was refused.
//
// Every device is returned, refused ones included. A discovery run has to be
// able to tell an administrator why the disk they expected is not on the list,
// and a filter that dropped it would leave them with nothing to read.
func (in Inspector) Candidates(ctx context.Context) ([]Candidate, error) {
	disks, err := Scan(in.Config)
	if err != nil {
		return nil, err
	}

	usage, err := ReadUsage(in.Config, disks, in.Exclusive)
	if err != nil {
		return nil, err
	}

	prober := in.Prober
	if prober == nil {
		prober = NewProber()
	}

	candidates := make([]Candidate, 0, len(disks))
	for _, disk := range disks {
		candidates = append(candidates, judge(ctx, prober, disk, usage[disk.Name]))
	}
	return candidates, nil
}

// judge decides one device.
//
// The order is what makes it safe. Everything establishable without touching
// the device comes first, and the content reading runs only for a device that
// has survived all of it, so a mounted disk is never opened. A partition table
// found in sysfs is the one structural finding that does not stop the read: the
// override that accepts a stale table still wants to know what else is on the
// disk.
func judge(ctx context.Context, prober *Prober, disk Disk, usage Usage) Candidate {
	c := Candidate{Disk: disk, Usage: usage}

	if disk.Kind != KindDisk {
		c.reject(ReasonNotAWholeDisk, fmt.Sprintf("the device is a %s", disk.Kind))
	}
	if disk.Transport == TransportNVMeFabric {
		c.reject(ReasonFabricNamespace,
			"the namespace is reached over a fabric, so its bytes belong to whatever exported it")
	} else if disk.Kind == KindDisk && disk.Transport == TransportUnknown {
		c.reject(ReasonUnknownTransport, "the device sits on no bus this scan could name")
	}
	if disk.SizeBytes == 0 {
		c.reject(ReasonNoCapacity, "the device reports a size of zero")
	}
	if disk.ReadOnly {
		c.reject(ReasonReadOnly, "the kernel presents the device read-only")
	}
	if len(usage.Mountpoints) > 0 {
		c.reject(ReasonMounted, fmt.Sprintf("mounted at %v", usage.Mountpoints))
	}
	if usage.Swap {
		c.reject(ReasonSwapArea, "the device is an active swap area")
	}
	if len(usage.Holders) > 0 {
		c.reject(ReasonStacked, fmt.Sprintf("held by %v", usage.Holders))
	}
	if usage.Busy {
		c.reject(ReasonBusy, "the kernel refused an exclusive open, and gives no reason")
	}
	if usage.ProbeErr != nil {
		c.reject(ReasonUnreadable, fmt.Sprintf("the kernel could not be asked: %v", usage.ProbeErr))
	}
	if len(disk.Partitions) > 0 {
		c.reject(ReasonPartitioned, fmt.Sprintf("the device carries the partitions %v", disk.Partitions))
	}

	// Everything above was established without opening anything. A device
	// already refused on any of those grounds is one this must not read.
	if c.rejectedApartFromPartitions() {
		return c
	}

	reading, err := prober.Read(ctx, disk.Device)
	if err != nil {
		c.reject(ReasonUnreadable, fmt.Sprintf("the device could not be read: %v", err))
		return c
	}
	c.Reading = reading

	switch reading.Content {
	case ContentBlank:
	case ContentForeign:
		if reading.Type == "gpt" || reading.Type == "dos" {
			// A partition table found in the bytes and no partition children in
			// sysfs is a table the kernel has not acted on, which is exactly
			// the stale table the override exists for.
			if !c.RejectedFor(ReasonPartitioned) {
				c.reject(ReasonPartitioned, reading.Detail)
			}
			break
		}
		c.reject(ReasonNotBlank, reading.Detail)
	case ContentFilesystem, ContentStackLayer:
		c.reject(ReasonNotBlank, reading.Detail)
	case ContentUnknown:
		// Read never returns it, and a reading that carries it anyway is one
		// nothing established. Refusing is the only safe reading of that.
		c.reject(ReasonUnreadable, "the reading came back unpopulated")
	}
	return c
}

// reject records a ground.
func (c *Candidate) reject(reason Reason, detail string) {
	c.Rejections = append(c.Rejections, Rejection{Reason: reason, Detail: detail})
}

// rejectedApartFromPartitions reports whether the device was refused on any
// ground other than carrying a partition table, which is the test for whether
// reading its bytes is still worth doing.
func (c Candidate) rejectedApartFromPartitions() bool {
	for _, rejection := range c.Rejections {
		if rejection.Reason != ReasonPartitioned {
			return true
		}
	}
	return false
}
