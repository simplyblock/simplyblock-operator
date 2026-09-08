// Who is already using a block device, from the four sources that know.
//
// No single one of them is enough. The mount table knows about filesystems and
// nothing about a disk an LVM volume group has taken; the holders directory
// knows about that and nothing about swap; /proc/swaps knows about swap and
// names devices by whatever path they were activated under; and the kernel's own
// exclusive open knows about all three and about whatever none of them saw,
// while saying nothing about why.
//
// So all four are read, and each one's answer is kept as its own field. A
// discovery run reports to a human who has to decide whether the run was right,
// and "the root filesystem is on it" is a finding that reader can act on where
// "busy" is not.
//
// The mount reading climbs from a partition to the disk that carries it, because
// what a caller is deciding about is the disk: nothing is mounted at
// /dev/nvme1n1, and handing it over would take the root filesystem with it.

package blockdev

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrDeviceBusy is what an ExclusiveOpener returns when the kernel refused to
// hand a device over because something holds it.
//
// It is a sentinel rather than the platform's own errno so that the contract is
// the same everywhere and a test can produce the answer without a device: what
// a caller needs to distinguish is a refusal from a probe that could not be
// made, and those are two different things whatever the errno was.
var ErrDeviceBusy = errors.New("blockdev: the kernel holds the device")

// ExclusiveOpener asks the kernel whether it will hand a device over, and
// closes it again immediately.
//
// It returns nil when the device was opened exclusively and nothing holds it,
// ErrDeviceBusy when the kernel refused, and any other error when the probe
// could not be made at all. The third case is not a synonym for either of the
// first two: a device nobody could ask about is a device whose usage is
// unknown, and reporting it as free is how a mounted disk gets handed over.
type ExclusiveOpener func(path string) error

// Usage is what was found to be using one device.
//
// The zero value says nothing is using it, which is only true of a Usage that a
// reading populated: a value nobody filled in reports a free device, so it is
// never constructed except by ReadUsage.
type Usage struct {
	// Mountpoints is where this device or any of its partitions is mounted,
	// ascending. It is empty for a disk that carries a filesystem nothing has
	// mounted.
	Mountpoints []string

	// Swap reports whether this device or one of its partitions is an active
	// swap area.
	Swap bool

	// Holders is the kernel names of the devices stacked directly on top of
	// this one, which is the readable reason a disk an LVM volume group took is
	// not free.
	Holders []string

	// Busy reports that the kernel refused to hand the device over. It is the
	// backstop for everything the three readable sources did not see, and it
	// carries no reason: the kernel does not give one.
	Busy bool

	// ProbeErr is why the exclusive open could not be attempted, when it could
	// not be. A device with a ProbeErr has an unknown usage rather than a free
	// one, and candidate.go rejects it on exactly that ground.
	ProbeErr error
}

// InUse reports whether anything was found to be using the device.
func (u Usage) InUse() bool {
	return len(u.Mountpoints) > 0 || u.Swap || len(u.Holders) > 0 || u.Busy
}

// ReadUsage reads what is using each of the scanned devices, keyed by kernel
// name.
//
// The whole scan is passed in rather than one device at a time because the
// mount table and the swap list are read once for the host, and because
// climbing from a partition to its disk needs the disk's partitions, which is
// what the scan already holds.
//
// An unreadable mount table is a failure and not an empty answer. A host whose
// mounts could not be read is not a host with nothing mounted, and treating it
// as one would hand over the disk the root filesystem is on.
func ReadUsage(cfg ScanConfig, disks []Disk, exclusive ExclusiveOpener) (map[string]Usage, error) {
	if exclusive == nil {
		exclusive = OpenExclusive
	}

	mounts, err := readMountinfo(cfg.mountinfo())
	if err != nil {
		return nil, err
	}
	swaps, err := readSwaps(cfg.proc())
	if err != nil {
		return nil, err
	}

	byName := make(map[string]Disk, len(disks))
	for _, disk := range disks {
		byName[disk.Name] = disk
	}

	usage := make(map[string]Usage, len(disks))
	for _, disk := range disks {
		u := Usage{Holders: disk.Holders}

		// The device itself, then everything on it: a mount recorded against a
		// partition is a reason the whole disk is not free.
		for _, name := range append([]string{disk.Name}, disk.Partitions...) {
			part, ok := byName[name]
			if !ok {
				continue
			}
			u.Mountpoints = append(u.Mountpoints, mounts[devNumber{part.Major, part.Minor}]...)
			if swaps[part.Path] || swaps[part.Name] {
				u.Swap = true
			}
		}
		slices.Sort(u.Mountpoints)
		u.Mountpoints = slices.Compact(u.Mountpoints)

		// A device with no size has no bytes to hold and no node worth opening.
		// Opening one by path would be a probe against whatever /dev happens to
		// carry at that name.
		if disk.SizeBytes > 0 {
			switch err := exclusive(disk.Path); {
			case err == nil:
			case errors.Is(err, ErrDeviceBusy):
				u.Busy = true
			default:
				u.ProbeErr = err
			}
		}

		usage[disk.Name] = u
	}
	return usage, nil
}

// devNumber is a device's major and minor, which is what a mount is recorded
// against and the only identifier that survives a path being renamed.
type devNumber struct{ major, minor uint32 }

// mountinfoFields is how many fields precede the optional ones. The line is
// id, parent, major:minor, root, mountpoint, options, then a variable number of
// optional fields, then a lone dash, then the filesystem type and source.
const mountinfoFields = 6

// readMountinfo reads which device is mounted where.
//
// It reads mountinfo rather than mounts because mountinfo states the device
// numbers, and mounts states only a source string: /dev/mapper/vg0-root,
// /dev/disk/by-uuid/…, and /dev/dm-0 are one device under three names, and
// comparing strings would match one of the three.
func readMountinfo(path string) (map[devNumber][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("blockdev: read %s: %w", path, err)
	}

	mounts := map[devNumber][]string{}
	for line := range strings.Lines(string(raw)) {
		fields := strings.Fields(line)
		if len(fields) < mountinfoFields {
			continue
		}
		majorText, minorText, ok := strings.Cut(fields[2], ":")
		if !ok {
			continue
		}
		major, err := strconv.ParseUint(majorText, 10, 32)
		if err != nil {
			continue
		}
		minor, err := strconv.ParseUint(minorText, 10, 32)
		if err != nil {
			continue
		}
		key := devNumber{uint32(major), uint32(minor)}
		mounts[key] = append(mounts[key], unescapeMountField(fields[4]))
	}
	return mounts, nil
}

// swapsHeaderPrefix is the first field of the header line /proc/swaps opens
// with, which is not a swap area.
const swapsHeaderPrefix = "Filename"

// readSwaps reads which devices are active swap areas, keyed both by the path
// the swap list names and by that path's last element.
//
// Swap is the one source that names a device by path rather than by number, so
// this is a path match and cannot be anything else. Keying by the last element
// as well is what makes it survive a caller that mounted the host's /dev
// somewhere else: the swap list names the host's /dev/vda where such a caller's
// device paths read /host/dev/vda, and a full-path comparison would match
// nothing and report the swap disk without the reason it is unavailable.
//
// It still does not catch every spelling. An area activated through
// /dev/mapper/vg-swap is named that way here and the device's kernel name is
// dm-1, so neither key matches. That is why this is not the only defense: the
// kernel holds a swap device exclusively, so the exclusive open refuses it
// whatever path it was activated under. What this reading adds is the reason,
// which the kernel does not give.
func readSwaps(proc string) (map[string]bool, error) {
	path := filepath.Join(proc, "swaps")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A kernel built without swap support exports no file, and a host
			// with swap off exports the header alone. Neither is a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("blockdev: read %s: %w", path, err)
	}

	swaps := map[string]bool{}
	for line := range strings.Lines(string(raw)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == swapsHeaderPrefix {
			continue
		}
		path := unescapeMountField(fields[0])
		swaps[path] = true
		swaps[filepath.Base(path)] = true
	}
	return swaps, nil
}

// unescapeMountField undoes the octal escaping the kernel applies to the
// characters that would otherwise split a field: space, tab, newline, and
// backslash. A mountpoint at /mnt/my disk arrives with its space written as
// the escape \040, and a caller comparing it against a real path never matches.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] != '\\' || i+3 >= len(field) {
			out.WriteByte(field[i])
			continue
		}
		code, err := strconv.ParseUint(field[i+1:i+4], 8, 8)
		if err != nil {
			out.WriteByte(field[i])
			continue
		}
		out.WriteByte(byte(code))
		i += 3
	}
	return out.String()
}
