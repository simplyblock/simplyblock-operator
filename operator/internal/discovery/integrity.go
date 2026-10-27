// The integrity settings a draft derives from its devices: the metadata
// journal, and the 4K atomicity checksum validation needs on a device it
// cannot check any other way.
//
// Both follow from one property of each device: whether it completes a 4K write
// whole across a power failure. The data plane writes its metadata pages and
// its fallback checksum blocks 4K at a time, so a device that can tear a 4K
// write needs the metadata journal, and checksum validation on such a device
// needs either a metadata format, where the checksum travels with the block,
// or 4K blocks.
//
// Each device is judged as it will be once a storage node has taken it. With
// formatting on, the control plane reformats an NVMe namespace that is not at
// 4K to the plain 4K format where it has one, and to format 0 where it does not
// (format_device_with_4k). A device keeps its format otherwise. Judging the
// format a device has today would answer for a device the cluster never sees.

package discovery

import (
	"fmt"
	"slices"
	"strings"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const (
	// fourKiB is the write the data plane's metadata pages and fallback
	// checksum blocks are made of.
	fourKiB = 4096

	// minChecksumMetadata is the metadata a block needs for alceml to keep
	// its checksum there rather than on a separate page.
	minChecksumMetadata = 8

	// listedDevices is how many devices a note names before it counts the rest.
	listedDevices = 4
)

// atomicity is what is known about whether a device completes a 4K write whole.
type atomicity int

const (
	atomicityUnknown atomicity = iota
	atomicityYes
	atomicityNo
)

// deviceIntegrity is one drafted device as a storage node will have it.
type deviceIntegrity struct {
	// name is the worker and the device as the draft names it.
	name string

	// blockSize is the logical block size, and zero when nothing says.
	blockSize uint32

	// metadataSize is the metadata each block carries, meaningful only when
	// metadataKnown.
	metadataSize  uint16
	metadataKnown bool

	atomic4K atomicity
}

// checkable reports whether checksum validation runs on the device without
// enforced atomicity: with the checksum in the block's metadata, or on a
// separate page of 4K blocks.
func (d deviceIntegrity) checkable() bool {
	return (d.metadataKnown && d.metadataSize >= minChecksumMetadata) || d.blockSize >= fourKiB
}

// integrityFindings sorts a plan's devices by what each integrity setting needs
// from them.
type integrityFindings struct {
	// devices is how many devices were judged.
	devices int

	// journal is every device not known to complete a 4K write whole.
	journal []string

	// needsAtomicity is every device checksum validation can check only with
	// enforced atomicity, and whose own report says that is safe.
	needsAtomicity []string

	// cannotCheck is every device checksum validation cannot check: no
	// metadata format, blocks under 4K, and an atomic write unit under 4K.
	cannotCheck []string

	// checkUnknown is every device nothing says checksum validation can check.
	checkUnknown []string
}

// integrityOf judges every device the plan names, with formatting as the
// draft states it.
func integrityOf(plan Plan, formatting bool) integrityFindings {
	var out integrityFindings
	for _, worker := range plan.Workers {
		seen := map[string]struct{}{}
		for _, device := range worker.Devices {
			// A controller with two namespaces is two devices and one entry
			// in the draft, and the control plane formats the controller.
			key := device.PCIAddress
			if worker.Class != ClassNVMe || key == "" {
				key = device.Path
			}
			if _, done := seen[key]; done {
				continue
			}
			seen[key] = struct{}{}

			d := judge(worker, device, formatting)
			out.devices++
			if d.atomic4K != atomicityYes {
				out.journal = append(out.journal, d.name)
			}
			switch {
			case d.checkable():
			case d.atomic4K == atomicityYes:
				out.needsAtomicity = append(out.needsAtomicity, d.name)
			case d.atomic4K == atomicityNo && d.metadataKnown && d.blockSize != 0:
				out.cannotCheck = append(out.cannotCheck, d.name)
			default:
				out.checkUnknown = append(out.checkUnknown, d.name)
			}
		}
	}
	return out
}

// judge is one device as a storage node will have it.
func judge(worker Worker, device nodeprobe.Device, formatting bool) deviceIntegrity {
	d := deviceIntegrity{name: worker.Name + " " + device.Path, blockSize: device.LogicalBlockSize}
	if worker.Class != ClassNVMe {
		// A logical block device is handed over as it is, and carries no
		// metadata a checksum could travel in.
		d.metadataKnown = true
		d.atomic4K = atomicityAt4K(d.blockSize, device, true)
		return d
	}
	if device.PCIAddress != "" {
		d.name = worker.Name + " " + device.PCIAddress
	}

	formats := namespaceFormatsOf(worker.Report, device.PCIAddress)
	current := currentFormat(formats)
	if current != nil {
		d.blockSize, d.metadataSize, d.metadataKnown = current.DataSize, current.MetadataSize, true
	}

	sameFormat := true
	if formatting && d.blockSize != fourKiB {
		target := formatTarget(formats)
		d.blockSize, d.metadataSize, d.metadataKnown = 0, 0, false
		sameFormat = false
		if target != nil {
			d.blockSize, d.metadataSize, d.metadataKnown = target.DataSize, target.MetadataSize, true
			sameFormat = current != nil && target.Index == current.Index
		}
	}
	d.atomic4K = atomicityAt4K(d.blockSize, device, sameFormat)
	return d
}

// atomicityAt4K is whether a device with blocks of blockSize completes a 4K
// write whole. The atomic write units the kernel reported describe the format
// the device had when it was probed, so they answer only for that one.
func atomicityAt4K(blockSize uint32, device nodeprobe.Device, sameFormat bool) atomicity {
	switch {
	case blockSize >= fourKiB:
		return atomicityYes
	case blockSize == 0 || !sameFormat || device.AtomicWriteUnitMaxBytes == nil:
		return atomicityUnknown
	}
	if minimum := device.AtomicWriteUnitMinBytes; minimum != nil && *minimum > fourKiB {
		return atomicityNo
	}
	if *device.AtomicWriteUnitMaxBytes >= fourKiB {
		return atomicityYes
	}
	return atomicityNo
}

// namespaceFormatsOf is the formats of the one namespace the controller at
// address presents, or nil when the probe read none or there is more than one
// to choose from.
func namespaceFormatsOf(report nodeprobe.Report, address string) *nodeprobe.NamespaceFormats {
	index := slices.IndexFunc(report.NVMeControllers, func(c nodeprobe.Controller) bool {
		return address != "" && c.Address == address
	})
	if index < 0 {
		return nil
	}
	var found *nodeprobe.NamespaceFormats
	for i, ns := range report.NVMeControllers[index].LBAFormats {
		if ns.Common {
			continue
		}
		if found != nil {
			return nil
		}
		found = &report.NVMeControllers[index].LBAFormats[i]
	}
	return found
}

func currentFormat(formats *nodeprobe.NamespaceFormats) *nodeprobe.LBAFormat {
	if formats == nil || formats.Current == nil {
		return nil
	}
	return formatAt(formats, *formats.Current)
}

// formatTarget is the format the control plane reformats a namespace to: the
// 4K one without metadata, or format 0 where there is none.
func formatTarget(formats *nodeprobe.NamespaceFormats) *nodeprobe.LBAFormat {
	if formats == nil {
		return nil
	}
	for i, f := range formats.Formats {
		if f.DataSize == fourKiB && f.MetadataSize == 0 {
			return &formats.Formats[i]
		}
	}
	return formatAt(formats, 0)
}

func formatAt(formats *nodeprobe.NamespaceFormats, index int) *nodeprobe.LBAFormat {
	for i, f := range formats.Formats {
		if f.Index == index {
			return &formats.Formats[i]
		}
	}
	return nil
}

// journalNote accounts for a derived metadata journal.
func journalNote(findings integrityFindings) string {
	if len(findings.journal) == 0 {
		return "enableMetadataJournal is off, because every device completes a 4K write " +
			"whole as a storage node will have it: its blocks are 4K, or the atomic write " +
			"unit it reports covers 4K."
	}
	const journals = " The data plane journals the lvstore metadata pages, so a torn write " +
		"cannot leave a half-written one behind."
	if len(findings.journal) == findings.devices && findings.devices > 1 {
		return fmt.Sprintf("enableMetadataJournal is on, because none of the %d devices is known "+
			"to complete a 4K write whole as a storage node will have it.", findings.devices) + journals
	}
	return fmt.Sprintf("enableMetadataJournal is on, because %s not known to complete a 4K "+
		"write whole as a storage node will have it: %s.",
		countOf(len(findings.journal), findings.devices), listOf(findings.journal)) + journals
}

// checksumFeasibility says what turning checksum validation on would need,
// for a draft that leaves it to the reviewer.
func checksumFeasibility(findings integrityFindings) string {
	var parts []string
	if len(findings.cannotCheck) > 0 {
		parts = append(parts, fmt.Sprintf("the control plane cannot check %s, which have no "+
			"metadata format, blocks under 4K, and an atomic write unit under 4K",
			listOf(findings.cannotCheck)))
	}
	if len(findings.needsAtomicity) > 0 {
		parts = append(parts, fmt.Sprintf("it needs enableAtomicity4K for %s, whose atomic "+
			"write unit covers 4K", listOf(findings.needsAtomicity)))
	}
	switch n := len(findings.checkUnknown); {
	case n > 1 && n == findings.devices:
		parts = append(parts, fmt.Sprintf("nothing in this run says whether it can check any "+
			"of the %d devices", n))
	case n > 0:
		parts = append(parts, fmt.Sprintf("nothing in this run says whether it can check %s",
			listOf(findings.checkUnknown)))
	}
	if len(parts) == 0 {
		return "enableChecksumValidation is left to the reviewer, and every device can be " +
			"checked as a storage node will have it."
	}
	return "enableChecksumValidation is left to the reviewer. Turned on, " +
		strings.Join(parts, "; and ") + "."
}

// countOf counts n devices of m as the subject of a sentence: "1 of the 4
// devices is" or "the one device is."
func countOf(n, of int) string {
	switch {
	case of == 1:
		return "the one device is"
	case n == 1:
		return fmt.Sprintf("1 of the %d devices is", of)
	}
	return fmt.Sprintf("%d of the %d devices are", n, of)
}

// listOf names the first few devices and counts the rest.
func listOf(names []string) string {
	if len(names) <= listedDevices {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:listedDevices], ", "), len(names)-listedDevices)
}
