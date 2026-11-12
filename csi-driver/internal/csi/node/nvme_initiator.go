// Whether this node's kernel can connect a volume at all: the check a stage
// makes before it brings anything up.
//
// A volume reaches the node over NVMe/TCP, which needs the kernel's nvme-tcp
// module. Ubuntu ships it in linux-modules-extra, a package its cloud images
// leave out, and the DaemonSet's postStart hook only printed that the modprobe
// had failed, to output nobody can read. The first sign was then a mount that
// failed with what nvme-cli says when /dev/nvme-fabrics is not there, once for
// every path, and nothing in it names the package. So the check is made here,
// where the answer reaches the pod's events, and it says what to install.
//
// The same check is the node plugin's readiness probe (CheckInitiator), so a
// node that cannot connect a volume shows a plugin that is not ready before any
// volume is asked for.
package node

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/klog"
)

// fabricsDevice is what the kernel's NVMe-oF initiator exposes once any of its
// transports is loaded, and what every connect opens.
const fabricsDevice = "/dev/nvme-fabrics"

// initiatorModule is the transport a volume is connected over.
const initiatorModule = "nvme-tcp"

// initiatorCheck asks the host whether the NVMe-oF initiator is there. Both
// halves are seams, so a test can be a kernel it does not have.
type initiatorCheck struct {
	run     commandRunner
	present func(path string) bool
}

// hostInitiatorCheck is the check against the host this plugin runs on.
func hostInitiatorCheck() initiatorCheck {
	return initiatorCheck{run: runCommand, present: pathExists}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ensure reports why this node cannot connect a volume, or nil when it can.
//
// The device being there is the whole answer and costs a stat, which matters
// because this runs on every stage. Only when it is absent is the module asked
// for: one that is installed and not loaded is loaded here, so a node that
// needs nothing done to it does not fail a stage. It is asked again on every
// stage rather than once at start, because the remedy is applied to a running
// node and the next retry should find it.
func (c initiatorCheck) ensure(ctx context.Context) error {
	if c.present(fabricsDevice) {
		return nil
	}
	out, err := c.run(ctx, "modprobe", initiatorModule)
	if c.present(fabricsDevice) {
		return nil
	}

	said := "modprobe " + initiatorModule + " succeeded and the device did not appear"
	switch {
	case err != nil && out != "":
		said = "modprobe " + initiatorModule + " said: " + out
	case err != nil:
		said = "modprobe " + initiatorModule + " failed: " + err.Error()
	}

	release := "the running kernel"
	extra := "linux-modules-extra for the running kernel"
	if name, unameErr := c.run(ctx, "uname", "-r"); unameErr == nil && strings.TrimSpace(name) != "" {
		release = "kernel " + strings.TrimSpace(name)
		extra = "linux-modules-extra-" + strings.TrimSpace(name)
	}

	return fmt.Errorf(
		"this node cannot connect a volume: %s has no NVMe/TCP initiator loaded (%s is absent, and %s). "+
			"Install the package that carries the %s module on the node (%s on Ubuntu, "+
			"kernel-modules-extra on RHEL and its derivatives) and run `modprobe %s`. "+
			"The mount is retried and needs no restart",
		release, fabricsDevice, said, initiatorModule, extra, initiatorModule)
}

// ReportInitiator says once, at start, whether this node can connect a volume.
//
// It is a log line and not a refusal to start: the plugin still serves the
// volumes that need no initiator, and the readiness probe and the stage of a
// new volume make the same check where a reader sees it. What this adds is the
// answer in the plugin's own log.
func ReportInitiator(ctx context.Context, nodeName string) {
	if err := hostInitiatorCheck().ensure(ctx); err != nil {
		klog.Errorf("initiator check: node %s: %v", nodeName, err)
		return
	}
	klog.Infof("initiator check: node %s has the NVMe-oF initiator loaded", nodeName)
}

// CheckInitiator is the node plugin's readiness check, run as its own process
// by the readiness probe (`spdkcsi --check-initiator`). It returns the exit
// code: 0 when this node can connect a volume, 1 when it cannot, having written
// why to w.
//
// The plugin is not ready rather than not running. A pod that is not ready is
// as visible as one that restarts, in the pod list and in the deployment's
// phase, and kubelet records what the probe printed on the pod, so the package
// to install is there to read. A plugin that refused to start would also stop
// serving the pNFS volumes on the node, which need no initiator, and would
// answer a stage with a dead socket instead of the refusal that names the
// package. The module is asked for on every probe, so a node that was remedied
// turns ready on the next one.
func CheckInitiator(ctx context.Context, w io.Writer) int {
	return checkInitiator(ctx, hostInitiatorCheck().ensure, w)
}

func checkInitiator(ctx context.Context, ensure func(context.Context) error, w io.Writer) int {
	if err := ensure(ctx); err != nil {
		// A reason that cannot be written changes nothing about the verdict: the
		// node still cannot connect a volume, and the exit code says so.
		_, _ = fmt.Fprintln(w, err)
		return 1
	}
	return 0
}
