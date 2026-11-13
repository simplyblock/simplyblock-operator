// Whether this node's kernel can connect a volume at all: the check the node
// plugin makes before it starts, and again before a stage brings anything up.
//
// A volume reaches the node over NVMe/TCP, which needs the kernel's nvme-tcp
// module. Ubuntu ships it in linux-modules-extra, a package its cloud images
// leave out, and the DaemonSet's postStart hook only printed that the modprobe
// had failed, to output nobody can read. The first sign was then a mount that
// failed with what nvme-cli says when /dev/nvme-fabrics is not there, once for
// every path, and nothing in it names the package.
//
// So the plugin refuses to start on such a node (RequireInitiator). Every
// volume it stages is connected over NVMe/TCP, a pNFS volume included, whose
// client reads and writes the backing namespace directly, so there is nothing
// it could serve. A container in a restart loop is the signal a missing
// prerequisite gets everywhere else, and the reason is left in the container's
// termination message, where the pod shows it. The stage makes the same check
// for a module that went away under a running plugin.
package node

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// initiatorModule is the transport a volume is connected over.
const initiatorModule = "nvme-tcp"

// initiatorModuleDir is where the kernel shows the transport once it is loaded,
// or built in. It is the transport that is asked for and not /dev/nvme-fabrics:
// that device comes with nvme_fabrics, which ships in the base kernel package,
// so it can be there on a node that has no transport to connect over.
const initiatorModuleDir = "/sys/module/nvme_tcp"

// TerminationMessagePath is where a container leaves the reason it exited, the
// default terminationMessagePath of a pod.
const TerminationMessagePath = "/dev/termination-log"

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
// The module being loaded is the whole answer and costs a stat, which matters
// because this runs on every stage. Only when it is not is the module asked
// for: one that is installed and not loaded is loaded here, so a node that
// needs nothing done to it is not refused. A modprobe that succeeds is
// believed without looking again, because a kernel with the transport built
// in need not show it in sysfs, and refusing such a node would stop a plugin
// that works.
func (c initiatorCheck) ensure(ctx context.Context) error {
	if c.present(initiatorModuleDir) {
		return nil
	}
	out, err := c.run(ctx, "modprobe", initiatorModule)
	if err == nil {
		return nil
	}

	said := "modprobe " + initiatorModule + " failed: " + err.Error()
	if out != "" {
		said = "modprobe " + initiatorModule + " said: " + out
	}

	release := "the running kernel"
	extra := "linux-modules-extra for the running kernel"
	if name, unameErr := c.run(ctx, "uname", "-r"); unameErr == nil && strings.TrimSpace(name) != "" {
		release = "kernel " + strings.TrimSpace(name)
		extra = "linux-modules-extra-" + strings.TrimSpace(name)
	}

	return fmt.Errorf(
		"this node cannot connect a volume: %s has no NVMe/TCP initiator (the %s module is not loaded, and %s). "+
			"Install the package that carries the module on the node (%s on Ubuntu) and run `modprobe %s`",
		release, initiatorModule, said, extra, initiatorModule)
}

// RequireInitiator is the node plugin's precondition for starting: it returns
// why this node cannot connect a volume, having left that reason as the
// container's termination message, or nil when it can.
//
// The caller exits on an error. kubelet restarts the container with its usual
// backoff, so a node that was remedied is picked up by the next restart, and
// until then `kubectl describe pod` shows the reason under the last state.
func RequireInitiator(ctx context.Context) error {
	return requireInitiator(ctx, hostInitiatorCheck().ensure, TerminationMessagePath)
}

func requireInitiator(ctx context.Context, ensure func(context.Context) error, messagePath string) error {
	err := ensure(ctx)
	if err == nil {
		return nil
	}
	// The message is the convenience and the error is the verdict: one that
	// cannot be written is still returned, and still logged by the caller.
	if writeErr := os.WriteFile(messagePath, []byte(err.Error()+"\n"), 0o600); writeErr != nil {
		return fmt.Errorf("%w (and the termination message could not be written: %v)", err, writeErr)
	}
	return err
}
