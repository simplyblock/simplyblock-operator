// What the device-mapper probe finds on a host, and what a fresh stage does
// with the answer.

package node

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// dmsetupTargets is what `dmsetup targets` prints on a host with device-mapper.
const dmsetupTargets = "striped          v1.6.0\nlinear           v1.4.0\nerror            v1.6.0\n"

// fakeDMHost answers the probe's commands, so a test can stand in for a host
// that does or does not have device-mapper.
type fakeDMHost struct {
	asked      []string
	targets    string
	targetsErr error
}

func (h *fakeDMHost) run(_ context.Context, name string, args ...string) (string, error) {
	h.asked = append(h.asked, strings.Join(append([]string{name}, args...), " "))
	switch name {
	case "modprobe":
		return "", nil
	case "dmsetup":
		if h.targetsErr != nil {
			return "/dev/mapper/control: open failed: No such device", h.targetsErr
		}
		return h.targets, nil
	}
	return "", errors.New("unexpected command " + name)
}

func TestTheProbeFindsDeviceMapperWhenTheLinearTargetIsThere(t *testing.T) {
	host := &fakeDMHost{targets: dmsetupTargets}
	if !probeDeviceMapper(context.Background(), "worker-1", host.run) {
		t.Fatalf("a host listing the linear target has no device-mapper; asked %v", host.asked)
	}
	if len(host.asked) == 0 || host.asked[0] != "modprobe dm_mod" {
		t.Errorf("asked %v, want the module loaded first", host.asked)
	}
}

// dmsetup fails when the kernel has no device-mapper driver or the host has no
// /dev/mapper/control, and either one means a mapping cannot be created.
func TestTheProbeFindsNoDeviceMapperWhenDmsetupFails(t *testing.T) {
	host := &fakeDMHost{targetsErr: errors.New("exit status 1")}
	if probeDeviceMapper(context.Background(), "worker-1", host.run) {
		t.Fatal("a host whose dmsetup fails has device-mapper")
	}
}

// The indirection is a linear mapping, so device-mapper without that target is
// no use to it.
func TestTheProbeFindsNoDeviceMapperWithoutTheLinearTarget(t *testing.T) {
	host := &fakeDMHost{targets: "striped          v1.6.0\nerror            v1.6.0\n"}
	if probeDeviceMapper(context.Background(), "worker-1", host.run) {
		t.Fatal("a host without the linear target has device-mapper")
	}
}

// A host without device-mapper stages every fresh volume directly, whatever
// SPDKCSI_DM_INDIRECTION says: the layer cannot be built there, and a stage
// that tried would fail.
func TestAFreshStageOnAHostWithoutDeviceMapperIsDirect(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.deviceMapper = false
	for _, setting := range []string{"", "true"} {
		t.Setenv("SPDKCSI_DM_INDIRECTION", setting)
		for _, shape := range []stackShape{shapeRawBlock, shapePlain, shapeLVM, shapeLVMRawBlock} {
			if got := ns.attachShape(pvcTestHandle, shape); got != shape {
				t.Errorf("SPDKCSI_DM_INDIRECTION=%q, shape %v on a host without device-mapper: got %v",
					setting, shape, got)
			}
		}
	}
}

// A volume already staged behind the layer keeps it: its record decides, and
// a probe that now answers no does not pull the layer from under a consumer.
func TestAStagedVolumeKeepsItsLayerOnAHostWithoutDeviceMapper(t *testing.T) {
	ns, _ := newStackedServer(t, newRecordingRunner())
	ns.deviceMapper = false
	writeRecord(t, ns.stack, indirectHandle, []string{"fabric", "dmLinear", "filesystem"})
	if got := ns.attachShape(indirectHandle, shapePlain); got != shapeIndirectPlain {
		t.Errorf("staged with the layer: shape %v, want indirect plain", got)
	}
}

// The log line at start says whether the indirection is on, and when it is off
// because the host cannot run it, warns, naming the node and what is lost.
func TestTheNoticeWarnsWhenTheHostHasNoDeviceMapper(t *testing.T) {
	for _, setting := range []string{"", "true"} {
		warn, msg := dmIndirectionNotice("worker-1", false, setting)
		if !warn {
			t.Errorf("SPDKCSI_DM_INDIRECTION=%q, no device-mapper: not a warning: %q", setting, msg)
		}
		for _, want := range []string{"worker-1", "off", "device-mapper", "consistency group"} {
			if !strings.Contains(msg, want) {
				t.Errorf("SPDKCSI_DM_INDIRECTION=%q, no device-mapper: notice %q does not say %q", setting, msg, want)
			}
		}
	}
	if _, msg := dmIndirectionNotice("worker-1", false, "true"); !strings.Contains(msg, "SPDKCSI_DM_INDIRECTION=true") {
		t.Errorf("a setting that asked for the layer and cannot be honored is not named: %q", msg)
	}
}

func TestTheNoticeReportsTheIndirectionOnOrTurnedOff(t *testing.T) {
	warn, msg := dmIndirectionNotice("worker-1", true, "")
	if warn || !strings.Contains(msg, "on") {
		t.Errorf("device-mapper present, nothing set: warn=%v %q, want an on notice", warn, msg)
	}
	warn, msg = dmIndirectionNotice("worker-1", true, "false")
	if warn || !strings.Contains(msg, "SPDKCSI_DM_INDIRECTION=false") || !strings.Contains(msg, "consistency group") {
		t.Errorf("turned off: warn=%v %q, want it named as turned off and what that costs", warn, msg)
	}
}
