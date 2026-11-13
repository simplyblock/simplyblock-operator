// Whether this node can run the dmLinear indirection: the probe that asks the
// kernel for device-mapper's linear target, and the decision it feeds.
//
// The indirection is what lets a staged volume follow its namespace to another
// NVMe subsystem under a live consumer, and it is on by default. A host without
// device-mapper cannot build it, so a fresh stage there would fail rather than
// fall back. The probe runs once at start, before the service serves, and the
// node stages directly when it finds none, saying so in its log: a volume
// staged that way cannot join a consistency group live, and nothing else on the
// cluster shows why.
package node

import (
	"context"
	"fmt"
	"os"
	"strings"

	"k8s.io/klog"
)

// dmIndirectionEnv is the node plugin's setting for the indirection.
const dmIndirectionEnv = "SPDKCSI_DM_INDIRECTION"

// ProbeDeviceMapper asks this node's kernel for device-mapper, records the
// answer on the service, and logs whether new volumes are staged behind the
// indirection. Call it before the service serves.
func (ns *Server) ProbeDeviceMapper(ctx context.Context, nodeName string) {
	ns.deviceMapper = probeDeviceMapper(ctx, nodeName, runCommand)
	warn, msg := dmIndirectionNotice(nodeName, ns.deviceMapper, os.Getenv(dmIndirectionEnv))
	if warn {
		klog.Warning(msg)
	} else {
		klog.Info(msg)
	}
}

// probeDeviceMapper reports whether the host can create a dm-linear mapping:
// dmsetup reaches the kernel's device-mapper driver and the driver offers the
// linear target. Each step is logged, so a no can be told apart from a probe
// that could not run.
func probeDeviceMapper(ctx context.Context, nodeName string, run commandRunner) bool {
	// A kernel with device-mapper built in has no module to load, so a failed
	// modprobe is reported and is not the verdict. dmsetup is.
	if out, err := run(ctx, "modprobe", "dm_mod"); err != nil {
		klog.Infof("device-mapper probe: node %s cannot load dm_mod: %s", nodeName, orErr(out, err))
	}
	out, err := run(ctx, "dmsetup", "targets")
	if err != nil {
		klog.Infof("device-mapper probe: node %s: dmsetup targets failed: %s", nodeName, orErr(out, err))
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "linear" {
			klog.Infof("device-mapper probe: node %s offers the linear target %s", nodeName, strings.Join(fields[1:], " "))
			return true
		}
	}
	klog.Infof("device-mapper probe: node %s offers no linear target: %q", nodeName, out)
	return false
}

// orErr is what a failed command printed, or its error when it printed nothing.
func orErr(out string, err error) string {
	if out != "" {
		return out
	}
	return err.Error()
}

// dmIndirectionNotice is the line the node logs at start about the
// indirection, and whether it is a warning. setting is SPDKCSI_DM_INDIRECTION
// as the process sees it.
func dmIndirectionNotice(nodeName string, available bool, setting string) (bool, string) {
	const lost = "new volumes are staged directly on their NVMe device, so they cannot " +
		"move to another subsystem while in use or join a consistency group live"
	if !settingOn(setting) {
		return false, fmt.Sprintf("device-mapper indirection is off on node %s: turned off by %s=%s; %s",
			nodeName, dmIndirectionEnv, setting, lost)
	}
	if !available {
		asked := ""
		if strings.TrimSpace(setting) != "" {
			asked = fmt.Sprintf(" (%s=%s asked for it)", dmIndirectionEnv, setting)
		}
		return true, fmt.Sprintf("device-mapper indirection is off on node %s: the host has no usable "+
			"device-mapper with the linear target%s; %s. Load dm_mod and install dmsetup on the host, "+
			"then restart this pod, to turn it on", nodeName, asked, lost)
	}
	return false, fmt.Sprintf("device-mapper indirection is on on node %s: new volumes are staged behind "+
		"a dm-linear device", nodeName)
}
