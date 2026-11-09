// The rule that takes a worker's own disks ahead of the storage built on or
// reached through others.
//
// A software-RAID array, a multipath map, and a LUN attached over iSCSI are all
// devices a logical block device run can build on, and a worker that offers one
// of them and nothing else is a worker that was set up that way. A worker that
// offers bare local disks beside them was set up for something the run cannot
// read, and the run has to pick one set rather than draft both into one node:
// the local disks are the pick, because a cluster built on them touches nothing
// another system may also be using, and because that is what every deployment
// before arrays and maps were admitted was built out of. The switch that turns
// the preference off is for the fleet whose LUNs or arrays are what it means to
// use.

package discovery

import (
	"fmt"
	"slices"

	"github.com/simplyblock/atlas/blockdev"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// PrefersLocalDisks reads whether a run takes local disks first, which it does
// unless it says otherwise.
func PrefersLocalDisks(run *simplyblockv1alpha2.DiscoverSpec) bool {
	return run == nil || run.PreferLocalDisks == nil || *run.PreferLocalDisks
}

// LocalDisksFirstRule declines an array, a map, or an iSCSI LUN on a worker
// that also offers a bare local disk the run could take.
//
// It reads the worker's whole report rather than the device alone, because
// whether a device is declined depends on what else the worker has: the same
// array is taken on a worker with nothing else and declined on one with local
// disks beside it.
type LocalDisksFirstRule struct {
	// AllowPartitioned is the run's partition waiver, so that a local disk
	// whose only refusal is a stale partition table counts as one the run
	// could take. Without it, such a disk is not offered and does not displace
	// an array.
	AllowPartitioned bool
}

func (LocalDisksFirstRule) Name() string { return "local disks first" }

func (r LocalDisksFirstRule) Admit(report nodeprobe.Report, device nodeprobe.Device) (bool, string) {
	what := r.displaced(report, device)
	if what == "" {
		return true, ""
	}
	local := r.localDisks(report)
	if len(local) == 0 {
		return true, ""
	}
	return false, fmt.Sprintf(
		"it is %s and the worker also offers %d local disk(s) (%s), which a run takes first; "+
			"set spec.discover.preferLocalDisks to false to take it as well",
		what, len(local), joinNames(local))
}

// displaced names what the device is when it is one the preference declines,
// and is empty for a device it leaves alone.
func (LocalDisksFirstRule) displaced(report nodeprobe.Report, device nodeprobe.Device) string {
	switch device.Kind {
	case string(blockdev.KindMDRaid):
		if device.RAIDLevel != "" {
			return "a software-RAID array (" + device.RAIDLevel + ")"
		}
		return "a software-RAID array"
	case string(blockdev.KindDeviceMapper):
		if reachedOverISCSI(report, device) {
			return "a multipath map over iSCSI paths"
		}
		return "a multipath map"
	}
	if device.Transport == string(blockdev.TransportISCSI) {
		return "a LUN attached over iSCSI"
	}
	return ""
}

// localDisks is the worker's bare local disks the run could take: whole disks
// on a bus inside the chassis, free or refused only for a partition table the
// run waives.
func (r LocalDisksFirstRule) localDisks(report nodeprobe.Report) []string {
	var names []string
	for _, device := range report.Devices {
		if device.Kind != string(blockdev.KindDisk) || device.Transport == string(blockdev.TransportISCSI) ||
			device.Transport == string(blockdev.TransportNVMeFabric) {
			continue
		}
		if device.Available || (r.AllowPartitioned && device.OnlyRejectedFor(string(blockdev.ReasonPartitioned))) {
			names = append(names, device.Name)
		}
	}
	slices.Sort(names)
	return names
}

// reachedOverISCSI reports whether any device the map is built on, at any
// depth, is on the iSCSI transport: a map over two paths of a LUN is storage
// on the other side of a network however local the map itself looks.
func reachedOverISCSI(report nodeprobe.Report, device nodeprobe.Device) bool {
	byName := make(map[string]nodeprobe.Device, len(report.Devices))
	for _, d := range report.Devices {
		byName[d.Name] = d
	}
	seen := map[string]bool{}
	var walk func(d nodeprobe.Device) bool
	walk = func(d nodeprobe.Device) bool {
		if d.Transport == string(blockdev.TransportISCSI) {
			return true
		}
		for _, name := range d.Members {
			if seen[name] {
				continue
			}
			seen[name] = true
			if member, ok := byName[name]; ok && walk(member) {
				return true
			}
		}
		return false
	}
	return walk(device)
}

// joinNames lists kernel names for a reason, shortened past a few so that a
// worker with many disks does not turn one refusal into a paragraph.
func joinNames(names []string) string {
	const shown = 4
	if len(names) <= shown {
		return fmt.Sprintf("%v", names)
	}
	return fmt.Sprintf("%v and %d more", names[:shown], len(names)-shown)
}
