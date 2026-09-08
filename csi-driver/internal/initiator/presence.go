// Which block device currently backs which logical volume, as this process
// last saw it.
//
// The record exists because a device that disappears is the only evidence the
// node has that a volume lost every one of its paths: the kernel removes the
// device and nothing else reports it. Diffing a fresh scan against what was
// there before is what turns that silence into an event.
//
// Both halves of the data path write to it. An attach registers its device
// immediately rather than waiting for the monitor's next poll, because a volume
// that connects and loses its paths inside one poll interval would otherwise
// never have been "present" and its loss would go unnoticed forever. That is
// why the record lives here, in the package both halves already depend on,
// behind an API rather than as three package-level variables.
//
// TODO: replace this with a live sysfs scan via atlas nvme.SysfsDeviceResolver
// once the atlas connector is sufficiently tested, since it duplicates what atlas
// already reads from /sys.
package initiator

import "sync"

var (
	presenceMu    sync.Mutex
	devicePresent = make(map[string]bool)
	deviceLvolID  = make(map[string]string)
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
}

// ForgetDevice drops devicePath from the record, for a teardown this node
// performed itself. A device that goes away on its own is reported by
// PruneMissingDevices instead.
func ForgetDevice(devicePath string) {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	delete(devicePresent, devicePath)
	delete(deviceLvolID, devicePath)
}

// PruneMissingDevices diffs the record against the devices present now and
// returns those that vanished, dropping them from the record as it goes. A
// device whose logical volume was never resolved is dropped silently: without
// an lvol ID there is nothing a caller could act on.
func PruneMissingDevices(current map[string]bool) []MissingDevice {
	presenceMu.Lock()
	defer presenceMu.Unlock()

	var missing []MissingDevice
	for devicePath := range devicePresent {
		if current[devicePath] {
			continue
		}
		lvolID := deviceLvolID[devicePath]
		delete(devicePresent, devicePath)
		delete(deviceLvolID, devicePath)
		if lvolID != "" {
			missing = append(missing, MissingDevice{DevicePath: devicePath, LvolID: lvolID})
		}
	}
	return missing
}
