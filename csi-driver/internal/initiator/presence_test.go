// The device record against what sysfs says the kernel has.
//
// A fake resolver stands in for sysfs, so each case states exactly which
// namespace devices the kernel holds and whether the scan could be read at all.

package initiator

import (
	"context"
	"errors"
	"testing"

	atlasnvme "github.com/simplyblock/atlas/nvme"
)

// sysfsDevices is a DeviceResolver answering List with fixed devices or a
// fixed error. Only List is used by the presence record.
type sysfsDevices struct {
	paths []string
	err   error
}

func (s sysfsDevices) List(context.Context) ([]atlasnvme.Device, error) {
	if s.err != nil {
		return nil, s.err
	}
	devices := make([]atlasnvme.Device, len(s.paths))
	for i, p := range s.paths {
		devices[i] = atlasnvme.Device{Namespace: atlasnvme.Namespace{DevicePath: p}}
	}
	return devices, nil
}

func (s sysfsDevices) ListWithSelector(ctx context.Context, _ atlasnvme.DeviceSelector) ([]atlasnvme.Device, error) {
	return s.List(ctx)
}

func (s sysfsDevices) ByUUID(context.Context, string) (atlasnvme.Device, error) {
	return atlasnvme.Device{}, errors.ErrUnsupported
}

func (s sysfsDevices) ByDevicePath(context.Context, string) (atlasnvme.Device, error) {
	return atlasnvme.Device{}, errors.ErrUnsupported
}

func (s sysfsDevices) ByNamespace(context.Context, string, atlasnvme.NamespaceID) (atlasnvme.Device, error) {
	return atlasnvme.Device{}, errors.ErrUnsupported
}

// withSysfs points the presence record at devices for the duration of the test
// and starts it from an empty record.
func withSysfs(t *testing.T, devices sysfsDevices) {
	t.Helper()
	saved := presenceDevices
	presenceDevices = devices
	t.Cleanup(func() {
		presenceDevices = saved
		presenceMu.Lock()
		clear(devicePresent)
		clear(deviceLvolID)
		presenceMu.Unlock()
	})
}

// A device the kernel still has is not gone, whatever nvme-cli lists. nvme-cli
// leaves out a namespace whose paths are all in error recovery, and reading that
// as a removal marked volumes broken a minute before the kernel removed them.
//
// Regression: 2026-10-04-presence-from-nvme-list — on
// lblk_outage_matrix_k8s-20261003-080237 worker-3 reported nvme0n1 and nvme2n1
// removed at 08:56:43 while the kernel was still queuing I/O on nvme0n1 at
// 08:57:34.
func TestPruneKeepsADeviceSysfsStillHas(t *testing.T) {
	withSysfs(t, sysfsDevices{paths: []string{"/dev/nvme0n1", "/dev/nvme2n1"}})
	MarkDevicePresent("/dev/nvme0n1", "4e8f8a42-01b6-4e36-8f6e-885d6f7a50f9")
	MarkDevicePresent("/dev/nvme2n1", "e2d2c840-effc-4fce-be2f-e8d5da82dc9a")

	if missing := PruneMissingDevices(context.Background()); len(missing) != 0 {
		t.Fatalf("PruneMissingDevices reported %v gone while sysfs still has them", missing)
	}
}

func TestPruneReportsADeviceSysfsNoLongerHasOnce(t *testing.T) {
	withSysfs(t, sysfsDevices{paths: []string{"/dev/nvme0n1"}})
	MarkDevicePresent("/dev/nvme0n1", "4e8f8a42-01b6-4e36-8f6e-885d6f7a50f9")
	MarkDevicePresent("/dev/nvme2n2", "edb5ab15-4417-46f0-b99a-3610034e6917")

	missing := PruneMissingDevices(context.Background())
	want := MissingDevice{DevicePath: "/dev/nvme2n2", LvolID: "edb5ab15-4417-46f0-b99a-3610034e6917"}
	if len(missing) != 1 || missing[0] != want {
		t.Fatalf("PruneMissingDevices = %v, want only %v", missing, want)
	}
	if again := PruneMissingDevices(context.Background()); len(again) != 0 {
		t.Fatalf("a removal was reported a second time: %v", again)
	}
}

// A scan that cannot be read says nothing about what is gone. Reading it as "no
// devices" would mark every volume on the node broken, and dropping the record
// would lose a removal that a later, readable scan shows.
func TestPruneReportsNothingWhenSysfsCannotBeRead(t *testing.T) {
	withSysfs(t, sysfsDevices{err: context.DeadlineExceeded})
	MarkDevicePresent("/dev/nvme2n2", "edb5ab15-4417-46f0-b99a-3610034e6917")

	if missing := PruneMissingDevices(context.Background()); len(missing) != 0 {
		t.Fatalf("PruneMissingDevices reported %v gone from a scan that timed out", missing)
	}

	presenceDevices = sysfsDevices{}
	missing := PruneMissingDevices(context.Background())
	if len(missing) != 1 || missing[0].LvolID != "edb5ab15-4417-46f0-b99a-3610034e6917" {
		t.Fatalf("after the scan recovered, PruneMissingDevices = %v, want the removed device reported", missing)
	}
}
