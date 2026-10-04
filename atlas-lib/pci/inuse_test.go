// That a controller nobody checked is not reported as one nobody is using.
//
// The two answers used to be one value. A device nothing holds and a device
// whose process table could not be read both left InUse false, and only the
// first is safe to reclaim, so a caller reading the field alone could not tell
// a free disk from a disk it never asked about.

package pci

import "testing"

func TestAnUncheckedControllerReportsNoAnswer(t *testing.T) {
	// A device the check never looked at, which is every device before
	// CheckHolders runs.
	device := Device{Address: "0000:5e:00.0", Driver: DriverVFIO}

	if device.InUse != nil {
		t.Errorf("a device nothing checked reports %v, want no answer", *device.InUse)
	}
	if device.Held() {
		t.Error("a device nothing checked reports itself held")
	}
	if device.Free() {
		t.Error("a device nothing checked reports itself free, which is the whole defect")
	}
}

func TestACheckedControllerReportsWhatWasFound(t *testing.T) {
	held := Device{Address: "0000:5e:00.0", Driver: DriverVFIO}
	held.InUse = new(bool)
	*held.InUse = true

	free := Device{Address: "0000:5f:00.0", Driver: DriverVFIO}
	free.InUse = new(bool)

	if !held.Held() || held.Free() {
		t.Error("a held device does not report itself held")
	}
	if free.Held() || !free.Free() {
		t.Error("a device nothing holds does not report itself free")
	}
}
