// The persistent names a device answers to, read off the host's /dev/disk tree.
//
// A kernel name is a position in an enumeration order rather than an identity.
// /dev/sdb is whichever disk the kernel found second this boot, and a machine
// that comes back with its controllers probed in another order hands that name
// to another device. Anything that records which physical disk a deployment was
// given, and reads the record back after a reboot, therefore has to record
// something the device carries rather than something the boot assigned it.
//
// udev publishes exactly that under /dev/disk, as symlinks built from what the
// device reports: its WWN, the model and serial its enclosure exports, the UUID
// in its own partition table. This file reads those directories and answers two
// questions about one device — every persistent name it has, and which of them
// to write down.
//
// It lives beside the scan rather than in it because the two read different
// sources. A scan reads sysfs and is exercisable against a captured tree; these
// links exist only in a live /dev, where udev put them, so a caller that wants
// both asks for both.

package blockdev

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// linkDirByID holds a link per identity a device's bus exports: its WWN,
	// and the model-and-serial string the enclosure reports.
	linkDirByID = "by-id"

	// linkDirByPartUUID holds a link per partition, named by the identifier the
	// partition table carries for it.
	linkDirByPartUUID = "by-partuuid"
)

// selfReportedPrefixes open a by-id link built from an identifier the device
// reports about itself, rather than from strings assembled around it.
//
// The distinction is what separates the links a real machine offers for one
// disk. A Samsung namespace behind a QEMU worker exports three by-id names: its
// EUI, and two built from the controller's model and serial, one of them with
// the namespace's index appended. Only the first is the namespace's. The other
// two name the controller, so a drive presenting two namespaces gives both the
// same stem and separates them by an index counting the order they were found
// in — which is the same kind of ordering the kernel name already is.
var selfReportedPrefixes = []string{
	// The World Wide Name, which is also where a SCSI device's NAA designator
	// arrives: udev builds a wwn- link from it, so the SCSI case needs no rule
	// of its own.
	"wwn-",

	// The IEEE Extended Unique Identifier and the UUID an NVMe namespace
	// reports for itself.
	"nvme-eui.",
	"nvme-uuid.",
}

// stableLinkDirs are the /dev/disk directories a persistent name is taken from.
//
// The two left out are left out for the same reason. by-uuid and by-label name
// the filesystem on the device, which mkfs writes and which moves to whatever
// disk an image is restored onto; by-path names the slot the device is plugged
// into, which is the enclosure's enumeration order rather than the device's own
// name and hands its link to the replacement when a disk is swapped. Both are
// stable across a reboot and neither identifies the device, so recording one
// would answer the wrong question stably.
var stableLinkDirs = []string{linkDirByPartUUID, linkDirByID}

// StableLinkSet is every persistent /dev/disk link the host publishes, indexed
// by the device path each one resolves to and ordered within a device so that
// the one to record comes first.
//
// The whole set is carried rather than only the preferred link because the two
// ends of a deployment choose independently. The side that records a device and
// the side that later looks it up run different code on different machines, and
// a lookup matched against one side's preference would miss a device that is
// present under another of its own names.
type StableLinkSet map[string][]string

// ReadStableLinks reads the host's /dev/disk tree.
//
// A missing directory is not a failure. A host whose udev publishes nothing, or
// a /dev mounted without its disk subtree, has no persistent names, which is an
// answer every caller already has to handle: it falls back to the kernel path
// and says that is what it did. Failing here would instead take down a
// discovery run on a machine whose devices are all perfectly readable.
//
// Nothing is opened and no target is stat'd. A link is read and the path it
// names is recorded, whether or not anything is there: the index is keyed by
// target and every lookup comes from a device the scan already found, so a link
// udev left behind when a device was removed lands under a key nobody asks
// about. Checking would buy nothing and would cost the whole reading on a
// captured tree, which carries the links a machine had and, deliberately, none
// of its device nodes.
func ReadStableLinks(cfg ScanConfig) (StableLinkSet, error) {
	set := StableLinkSet{}
	for _, dir := range stableLinkDirs {
		base := filepath.Join(cfg.dev(), "disk", dir)
		entries, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("blockdev: list %s: %w", base, err)
		}
		for _, entry := range entries {
			link := filepath.Join(base, entry.Name())
			device, ok := linkTarget(base, link)
			if !ok {
				continue
			}
			set[device] = append(set[device], link)
		}
	}

	for device := range set {
		slices.SortFunc(set[device], func(a, b string) int {
			if rank := cmp.Compare(stableLinkRank(a), stableLinkRank(b)); rank != 0 {
				return rank
			}
			return cmp.Compare(a, b)
		})
	}
	return set, nil
}

// Preferred is the one link to write down for the device at path, or the empty
// string when udev published none.
//
// The order is the ordering of the set, which is documented on stableLinkRank.
func (s StableLinkSet) Preferred(device string) string {
	links := s[device]
	if len(links) == 0 {
		return ""
	}
	return links[0]
}

// linkTarget resolves one link to the device path it names, in the /dev the
// caller configured.
//
// The link is read and joined rather than fully evaluated, because evaluating
// would also resolve any symlink in the /dev root itself and return a path in a
// namespace the caller never named — which is the same device under a name that
// matches nothing the caller holds, so every lookup would miss.
//
// Anything that is not a symlink is skipped: /dev/disk holds only links, and an
// ordinary file there is not a device this can name.
func linkTarget(base, link string) (string, bool) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target), true
}

// stableLinkRank orders a device's links by how much of the device's own
// identity each one carries.
//
// A partition's by-partuuid link comes first because the identifier is written
// in the partition table on the device itself: it survives the disk being
// re-exported under another serial, which a hypervisor or an enclosure swap
// does and which renames every by-id link the partition has.
//
// Among the by-id links, the ones built from an identifier the device reports
// about itself come first — see selfReportedPrefixes. The rest are assembled
// from the model and serial the enclosure or the controller exports, which name
// the thing the device is plugged into rather than the device.
func stableLinkRank(link string) int {
	switch filepath.Base(filepath.Dir(link)) {
	case linkDirByPartUUID:
		return 0
	case linkDirByID:
		name := filepath.Base(link)
		for _, prefix := range selfReportedPrefixes {
			if strings.HasPrefix(name, prefix) {
				return 1
			}
		}
	}
	return 2
}
