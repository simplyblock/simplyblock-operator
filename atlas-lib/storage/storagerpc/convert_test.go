package storagerpc

import (
	"reflect"
	"testing"

	"github.com/simplyblock/atlas/nvme"
)

// fullDevice is a device with every field set to a distinct non-zero value.
//
// "Every field" is enforced by requireFullyPopulated rather than by reading it,
// which is what makes this fixture a guard rather than a sample: a field added
// to one of the nvme types fails the population check here until it is added to
// the fixture, and then fails the round trip until it is added to convert.go. A
// field that silently arrived as its zero value on the operator would otherwise
// be indistinguishable from one the node genuinely reported as empty.
func fullDevice() nvme.Device {
	path := nvme.Path{
		Controller: "nvme1",
		Name:       "nvme0c1n1",
		SysfsPath:  "/sys/class/nvme/nvme1/nvme0c1n1",
		NSID:       3,
		ANAState:   nvme.ANAOptimized,
		ANAGroupID: 7,
	}
	namespace := nvme.Namespace{
		ID:               3,
		Name:             "nvme0n3",
		SysfsPath:        "/sys/class/nvme-subsystem/nvme-subsys0/nvme0n3",
		DevicePath:       "/dev/nvme0n3",
		Dev:              "259:1",
		UUID:             "6f1a4d1e-0b8f-4a0e-9a3f-2f1c0e3d4b5a",
		NGUID:            "0123456789abcdef0123456789abcdef",
		WWID:             "uuid.6f1a4d1e-0b8f-4a0e-9a3f-2f1c0e3d4b5a",
		CSI:              1,
		LogicalBlockSize: 4096,
		Capacity:         2097152,
		Used:             1048576,
		MetadataBytes:    8,
		ReadOnly:         true,
		Hidden:           true,
		Paths:            []nvme.Path{path},
		Controller:       "nvme1",
	}
	controller := nvme.Controller{
		ID:         "nvme1",
		SysfsPath:  "/sys/class/nvme/nvme1",
		DevicePath: "/dev/nvme1",
		Dev:        "238:1",

		NQN:       "nqn.2023-01.io.simplyblock:lvol",
		CntlID:    42,
		Type:      "io",
		Transport: "tcp",
		State:     "live",
		Address: nvme.Address{
			TrAddr:  "192.168.10.69",
			TrSvcID: "4420",
			SrcAddr: "192.168.10.67",
		},

		HostNQN:  "nqn.2014-08.org.nvmexpress:uuid:host",
		HostID:   "6d1f0f0e-1111-2222-3333-444455556666",
		NUMANode: -1,

		QueueCount: 8,
		SQSize:     127,

		KeepAliveTOSec:    30,
		CtrlLossTMOSec:    600,
		ReconnectDelaySec: 10,
		FastIOFailTMO:     "off",
	}
	subsystem := nvme.Subsystem{
		ID:          "nvme-subsys0",
		SysfsPath:   "/sys/class/nvme-subsystem/nvme-subsys0",
		NQN:         "nqn.2023-01.io.simplyblock:lvol",
		Model:       "simplyblock lvol",
		Serial:      "SB0001",
		FirmwareRev: "1.0",
		Type:        "nvm",
		IOPolicy:    "round-robin",
		Controllers: []nvme.Controller{controller},
		Namespaces:  []nvme.Namespace{namespace},
	}
	return nvme.Device{Namespace: namespace, Subsystem: subsystem}
}

func TestDeviceRoundTrip(t *testing.T) {
	device := fullDevice()
	requireFullyPopulated(t, "nvme.Device", reflect.ValueOf(device))

	got := deviceFromProto(deviceToProto(device))
	if !reflect.DeepEqual(got, device) {
		t.Errorf("device did not survive the round trip:\n got %+v\nwant %+v", got, device)
	}
}

func TestSubsystemRoundTrip(t *testing.T) {
	subsystem := fullDevice().Subsystem
	requireFullyPopulated(t, "nvme.Subsystem", reflect.ValueOf(subsystem))

	got := subsystemFromProto(subsystemToProto(subsystem))
	if !reflect.DeepEqual(got, subsystem) {
		t.Errorf("subsystem did not survive the round trip:\n got %+v\nwant %+v", got, subsystem)
	}
}

func TestSelectorRoundTrip(t *testing.T) {
	selector := nvme.DeviceSelector{
		NQN:        "nqn.2023-01.io.simplyblock:lvol",
		NSID:       3,
		UUID:       "6f1a4d1e-0b8f-4a0e-9a3f-2f1c0e3d4b5a",
		DevicePath: "/dev/nvme0n3",
	}
	requireFullyPopulated(t, "nvme.DeviceSelector", reflect.ValueOf(selector))

	if got := selectorFromProto(selectorToProto(selector)); got != selector {
		t.Errorf("selector round trip = %+v, want %+v", got, selector)
	}
}

// The zero selector means "everything", and must not become something narrower
// by travelling.
func TestZeroSelectorRoundTrip(t *testing.T) {
	if got := selectorFromProto(selectorToProto(nvme.DeviceSelector{})); !got.IsZero() {
		t.Errorf("zero selector round-tripped to %s, want the zero selector", got)
	}
}

// Decoding must tolerate absent submessages rather than panic: a peer running a
// different build may leave one unset.
func TestDecodingNilMessages(t *testing.T) {
	if got := deviceFromProto(nil); !reflect.DeepEqual(got, nvme.Device{}) {
		t.Errorf("deviceFromProto(nil) = %+v, want the zero device", got)
	}
	if got := subsystemFromProto(nil); !reflect.DeepEqual(got, nvme.Subsystem{}) {
		t.Errorf("subsystemFromProto(nil) = %+v, want the zero subsystem", got)
	}
	if got := namespaceFromProto(nil); !reflect.DeepEqual(got, nvme.Namespace{}) {
		t.Errorf("namespaceFromProto(nil) = %+v, want the zero namespace", got)
	}
	if got := controllerFromProto(nil); !reflect.DeepEqual(got, nvme.Controller{}) {
		t.Errorf("controllerFromProto(nil) = %+v, want the zero controller", got)
	}
	if got := pathFromProto(nil); !reflect.DeepEqual(got, nvme.Path{}) {
		t.Errorf("pathFromProto(nil) = %+v, want the zero path", got)
	}
	if got := selectorFromProto(nil); !got.IsZero() {
		t.Errorf("selectorFromProto(nil) = %s, want the zero selector", got)
	}
}

// requireFullyPopulated fails when any exported field reachable from v still
// holds its zero value. Unexported fields are skipped: nvme.Device carries a
// resolver handle that is deliberately not part of the snapshot.
func requireFullyPopulated(t *testing.T, path string, v reflect.Value) {
	t.Helper()

	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			requireFullyPopulated(t, path+"."+field.Name, v.Field(i))
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty; the fixture must exercise every field", path)
			return
		}
		requireFullyPopulated(t, path+"[0]", v.Index(0))
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("%s is nil; the fixture must exercise every field", path)
			return
		}
		requireFullyPopulated(t, path+".*", v.Elem())
	default:
		if v.IsZero() {
			t.Errorf("%s is the zero value; the fixture must exercise every field "+
				"so that a field missing from convert.go fails the round trip", path)
		}
	}
}
