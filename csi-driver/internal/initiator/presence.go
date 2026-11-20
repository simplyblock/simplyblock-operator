// Which block device currently backs which logical volume, as this process
// last saw it.
//
// The record exists because a device that disappears is the only evidence the
// node has that a volume lost every one of its paths: the kernel removes the
// device and nothing else reports it. Comparing the record against the devices
// the kernel has now is what turns that silence into an event.
//
// Both halves of the data path write to it. An attach registers its device
// immediately rather than waiting for the monitor's next poll, because a volume
// that connects and loses its paths inside one poll interval would otherwise
// never have been "present" and its loss would go unnoticed forever. That is
// why the record lives here, in the package both halves already depend on,
// behind an API rather than as three package-level variables.
//
// What the kernel has now is read from sysfs, never from `nvme list`. nvme-cli
// leaves out a namespace it cannot query, and a namespace whose paths are all in
// error recovery is exactly that: on lblk_outage_matrix_k8s-20261003-080237 two
// devices the kernel still held were reported removed a minute before they were.
package initiator

import (
	"context"
	"sync"

	atlasnvme "github.com/simplyblock/atlas/nvme"
	"k8s.io/klog"
)

var (
	presenceMu    sync.Mutex
	devicePresent = make(map[string]bool)
	deviceLvolID  = make(map[string]string)

	// presenceGen counts records, and deviceGen holds the count at which each
	// device was last recorded. A prune compares a record only against a scan
	// that began after it, because an attach running beside the monitor can
	// record a device after the scan's snapshot was taken, and that device is
	// missing from the snapshot for no other reason than being newer than it.
	presenceGen uint64
	deviceGen   = make(map[string]uint64)

	// presenceDevices answers which namespace devices the kernel has. A
	// variable so tests can stand in for sysfs.
	presenceDevices atlasnvme.DeviceResolver = atlasnvme.NewSysfsDeviceResolver(atlasnvme.SysfsConfig{})
)

// MissingDevice is a device that was present when last seen and is not present
// now, together with the logical volume it backed.
type MissingDevice struct {
	DevicePath string
	LvolID     string
}

// MarkDevicePresent records that devicePath is attached and backs lvolID.
// devicePath must be the resolved device, not a by-id symlink, so that a later
// scan compares like with like.
func MarkDevicePresent(devicePath, lvolID string) {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	devicePresent[devicePath] = true
	deviceLvolID[devicePath] = lvolID
	presenceGen++
	deviceGen[devicePath] = presenceGen
}

// ForgetDevice drops devicePath from the record, for a teardown this node
// performed itself. A device that goes away on its own is reported by
// PruneMissingDevices instead.
func ForgetDevice(devicePath string) {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	delete(devicePresent, devicePath)
	delete(deviceLvolID, devicePath)
	delete(deviceGen, devicePath)
}

// PruneMissingDevices compares the record against the namespace devices sysfs
// shows now and returns those that vanished, dropping them from the record as
// it goes. A device whose logical volume was never resolved is dropped silently:
// without an lvol ID there is nothing a caller could act on.
//
// A scan that fails, including one that timed out, reports nothing and keeps the
// record as it is. A scan that cannot be read says nothing about what is gone,
// and reading it as "no devices" would report every volume on the node broken.
//
// A device recorded while the scan ran is left for the next one, which is the
// first scan able to have seen it.
func PruneMissingDevices(ctx context.Context) []MissingDevice {
	presenceMu.Lock()
	scanGen := presenceGen
	presenceMu.Unlock()

	devices, err := presenceDevices.List(ctx)
	if err != nil {
		klog.Warningf("presence: cannot read the namespace devices from sysfs, checking again next tick: %v", err)
		return nil
	}
	current := make(map[string]bool, len(devices))
	for _, d := range devices {
		current[d.Namespace.DevicePath] = true
	}

	presenceMu.Lock()
	defer presenceMu.Unlock()

	var missing []MissingDevice
	for devicePath := range devicePresent {
		if current[devicePath] || deviceGen[devicePath] > scanGen {
			continue
		}
		lvolID := deviceLvolID[devicePath]
		delete(devicePresent, devicePath)
		delete(deviceLvolID, devicePath)
		delete(deviceGen, devicePath)
		if lvolID != "" {
			missing = append(missing, MissingDevice{DevicePath: devicePath, LvolID: lvolID})
		}
	}
	return missing
}
