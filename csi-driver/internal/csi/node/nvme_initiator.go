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
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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

// kernelBuildFile is where the kernel says who built it, which is the one
// thing a container can read about the distribution of the host it runs on.
const kernelBuildFile = "/proc/version"

// initiatorCheck asks the host whether the NVMe-oF initiator is there. Its
// fields are seams, so a test can be a kernel it does not have.
type initiatorCheck struct {
	run     commandRunner
	present func(path string) bool
	// kernelBuild is what kernelBuildFile reads, or nothing when it cannot be
	// read.
	kernelBuild func() string
}

// hostInitiatorCheck is the check against the host this plugin runs on.
func hostInitiatorCheck() initiatorCheck {
	return initiatorCheck{run: runCommand, present: pathExists, kernelBuild: hostKernelBuild}
}

func hostKernelBuild() string {
	build, err := os.ReadFile(kernelBuildFile)
	if err != nil {
		return ""
	}
	return string(build)
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

	// What is needed is the module, and that is all this says on most hosts:
	// which package carries it is the distribution's to say, and this plugin
	// runs on distributions it knows nothing about. Ubuntu is the one named,
	// because its cloud images are where the module is missing and its package
	// name follows from the kernel release.
	remedy := fmt.Sprintf("Make the %s kernel module available on the node and run `modprobe %s`",
		initiatorModule, initiatorModule)
	if c.kernelBuild != nil && strings.Contains(c.kernelBuild(), "Ubuntu") {
		remedy = fmt.Sprintf("On Ubuntu the module is in the package %s: install it on the node and run `modprobe %s`",
			extra, initiatorModule)
	}

	return fmt.Errorf(
		"this node cannot connect a volume: %s has no NVMe/TCP initiator (the %s module is not loaded, and %s). %s",
		release, initiatorModule, said, remedy)
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

// startRefusalReason is the reason of the event a refused start leaves on the
// plugin's pod.
const startRefusalReason = "NVMeTCPInitiatorMissing"

// PodRef names the pod this plugin runs as, which is the object a refused
// start is recorded against.
type PodRef struct {
	Namespace string
	Name      string
	UID       string
}

// RecordStartRefusal leaves the refusal as a Warning event on the plugin's own
// pod, so it is among the events `kubectl describe pod` ends with, beside the
// back-off the kubelet reports there. The termination message says the same in
// the container's status, which is further up and easier to miss.
//
// It is one event per refused start. The kubelet's restart backoff spaces
// them out, and each is the proof that the node was looked at again.
//
// An error is for the caller's log only: the refusal does not depend on being
// recorded.
func RecordStartRefusal(ctx context.Context, kubeClient kubernetes.Interface, pod PodRef, reason error) error {
	if kubeClient == nil {
		return errors.New("no Kubernetes client, so the refusal is not recorded as an event")
	}
	if pod.Namespace == "" || pod.Name == "" {
		return errors.New("the plugin does not know its own pod, so the refusal is not recorded as an event")
	}
	now := metav1.Now()
	_, err := kubeClient.CoreV1().Events(pod.Namespace).Create(ctx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: pod.Name + ".", Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: types.UID(pod.UID),
		},
		Type:           corev1.EventTypeWarning,
		Reason:         startRefusalReason,
		Message:        reason.Error(),
		Source:         corev1.EventSource{Component: "simplyblock-csi-node"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}, metav1.CreateOptions{})
	return err
}
