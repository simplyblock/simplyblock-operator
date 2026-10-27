// The LBA formats a namespace can be formatted with, decoded from the Identify
// Namespace data structure, and the namespaces a controller has to ask about.
//
// It sits beside identify.go because both decode an Identify payload and share
// its ioctl. The inventory is the caller, reading every kernel-driven
// controller's formats before a deployment decides which block size to format
// its disks with.

package nvme

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/simplyblock/atlas/internal/sysfs"
)

// NVMe Identify Namespace data structure for the NVM command set (CNS 00h): a
// fixed 4096-byte blob. Only the fields the LBA formats need are decoded.
// See the NVM Command Set Specification, "Identify Namespace data structure."
//
//	offset  size  field    meaning
//	------  ----  -------  ------------------------------------------------
//	     0     8  NSZE     namespace size, in logical blocks
//	    25     1  NLBAF    number of LBA formats, 0's based      (decoded)
//	    26     1  FLBAS    formatted LBA size: the format in use (decoded)
//	   128   256  LBAF0-63 one 4-byte LBA format descriptor each (decoded)
//
// An LBA format descriptor is one little-endian dword:
//
//	bits   field  meaning
//	-----  -----  ------------------------------------------------
//	15:0   MS     metadata size per block, in bytes
//	23:16  LBADS  data size per block, as a power of two
//	25:24  RP     relative performance, 0 best to 3 degraded
const (
	identifyNamespaceLen = 4096

	nlbafOffset    = 25
	flbasOffset    = 26
	lbafOffset     = 128
	lbafEntryLen   = 4
	maxLBAFormats  = 64
	minLBADataSize = 9 // LBADS below 9 (512 bytes) marks an unused entry
)

// NSIDAll is the broadcast namespace id. An Identify Namespace sent with it
// answers with the capabilities common to every namespace the controller could
// hold, which is how a controller with no namespace attached still says which
// formats it supports. Only a controller that supports namespace management
// answers it.
const NSIDAll NamespaceID = 0xFFFFFFFF

// RelativePerformance is the controller's ranking of one LBA format against
// the others it supports, as the RP field of the descriptor states it.
type RelativePerformance uint8

const (
	PerformanceBest     RelativePerformance = 0
	PerformanceBetter   RelativePerformance = 1
	PerformanceGood     RelativePerformance = 2
	PerformanceDegraded RelativePerformance = 3
)

// String is the name nvme-cli prints for the value.
func (p RelativePerformance) String() string {
	switch p {
	case PerformanceBest:
		return "best"
	case PerformanceBetter:
		return "better"
	case PerformanceGood:
		return "good"
	case PerformanceDegraded:
		return "degraded"
	}
	return fmt.Sprintf("RelativePerformance(%d)", uint8(p))
}

// LBAFormat is one combination of block size and metadata size a namespace can
// be formatted with.
type LBAFormat struct {
	// Index is the format's number, the value a Format NVM command takes.
	Index int

	// DataSize is the size of one logical block, in bytes.
	DataSize uint32

	// MetadataSize is the metadata carried with each block, in bytes.
	MetadataSize uint16

	// RelativePerformance is the controller's ranking of this format.
	RelativePerformance RelativePerformance
}

// NamespaceFormats is the LBA formats one namespace supports, and the one it is
// formatted with.
type NamespaceFormats struct {
	// NSID is the namespace the formats were read from, or NSIDAll when they
	// are the controller's common formats.
	NSID NamespaceID

	// Formats is every supported format, ascending by index. An entry the
	// controller left unused is omitted.
	Formats []LBAFormat

	// InUse is the index of the format the namespace is formatted with, and -1
	// for NSIDAll, which describes no formatted namespace.
	InUse int

	// ExtendedMetadata reports whether the metadata is transferred at the end
	// of each block rather than in a separate buffer. It is meaningless for
	// NSIDAll.
	ExtendedMetadata bool
}

// Current returns the format the namespace is formatted with, and false when
// there is none: for NSIDAll, and for a namespace whose format in use is an
// entry the controller left unused.
func (n NamespaceFormats) Current() (LBAFormat, bool) {
	for _, format := range n.Formats {
		if format.Index == n.InUse {
			return format, true
		}
	}
	return LBAFormat{}, false
}

// namespaceFormatsFromIdentify decodes the LBA formats from a raw Identify
// Namespace buffer. A buffer too short to hold the descriptor table yields no
// formats.
func namespaceFormatsFromIdentify(buf []byte, nsid NamespaceID) NamespaceFormats {
	formats := NamespaceFormats{NSID: nsid, InUse: -1}
	if len(buf) < lbafOffset+maxLBAFormats*lbafEntryLen {
		return formats
	}

	// NLBAF bounds the table. NVMe 1.x controllers support at most 16 formats
	// and leave the rest of the table reserved, so it is read no further than
	// NLBAF says even though 64 entries fit.
	count := min(int(buf[nlbafOffset])+1, maxLBAFormats)
	for i := range count {
		entry := buf[lbafOffset+i*lbafEntryLen:]
		lbads := entry[2]
		if lbads < minLBADataSize || lbads > 31 {
			continue
		}
		formats.Formats = append(formats.Formats, LBAFormat{
			Index:               i,
			DataSize:            1 << lbads,
			MetadataSize:        binary.LittleEndian.Uint16(entry[0:2]),
			RelativePerformance: RelativePerformance(entry[3] & 0x3),
		})
	}

	// A broadcast Identify zero-fills FLBAS, which would read as format 0.
	if nsid == NSIDAll {
		return formats
	}
	// The format index is FLBAS bits 3:0, with bits 6:5 as its bits 5:4
	// since NVMe 2.0 (reserved, so zero, before it). This is the kernel's
	// nvme_lbaf_index.
	flbas := buf[flbasOffset]
	formats.InUse = int(flbas&0x0f) | int(flbas&0x60)>>1
	formats.ExtendedMetadata = flbas&0x10 != 0
	return formats
}

// identifyNS reads the Identify Namespace data of nsid through the controller
// character device at devicePath. It is a package variable so tests can
// substitute the ioctl.
var identifyNS = identifyNamespace

// ReadNamespaceFormats issues an NVMe Identify Namespace admin command for nsid
// against the controller character device at controllerDevicePath (such as
// "/dev/nvme0") and returns the namespace's LBA formats. NSIDAll asks for the
// formats common to the controller.
//
// It is Linux-only (errs.ErrUnsupported elsewhere), and it gives up after
// bounded.ReadTimeout on a controller that does not answer.
func ReadNamespaceFormats(controllerDevicePath string, nsid NamespaceID) (NamespaceFormats, error) {
	buf, err := identifyNS(controllerDevicePath, nsid)
	if err != nil {
		return NamespaceFormats{}, fmt.Errorf("identify namespace %d on %s: %w", nsid, controllerDevicePath, err)
	}
	return namespaceFormatsFromIdentify(buf, nsid), nil
}

// ControllerNamespaceIDs lists the namespaces attached to the controller whose
// sysfs directory is controllerDir, such as /sys/class/nvme/nvme0, ascending.
//
// The kernel presents a namespace under its controller either as a private
// block device (nvme0n1, when native multipath is off) or as a multipath leg
// (nvme0c0n1), and the NSID comes from the entry's nsid attribute in both
// cases: the number in the name is the kernel's instance count, not the NSID.
// A controller with no namespace attached lists none and is not an error.
func ControllerNamespaceIDs(controllerDir string) ([]NamespaceID, error) {
	entries, err := sysfs.List(controllerDir)
	if err != nil {
		return nil, fmt.Errorf("list the namespaces of %s: %w", controllerDir, err)
	}
	var ids []NamespaceID
	for _, e := range entries {
		if !nsNameRE.MatchString(e) && !legNameRE.MatchString(e) {
			continue
		}
		// NSID 0 is not a namespace. It is what a missing attribute reads as.
		id := NamespaceID(sysfs.Uint32(controllerDir, e, "nsid"))
		if id != 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}
