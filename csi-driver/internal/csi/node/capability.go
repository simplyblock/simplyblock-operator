// Whether this node can run a client-side compressed or deduplicated volume:
// the probe that finds out, and the node label that publishes the answer
// (issue #277 §4.1, §4.3).
//
// The probe runs here rather than in the DaemonSet's postStart hook, which is
// where it started. A hook's output reaches neither the container's log stream
// nor anywhere else a reader can get at it: Kubernetes surfaces it only as an
// event when the hook fails, so a probe that ran and answered no left no trace
// of having run at all. That is not worth working around with a file written on
// one side and read on the other, because the plugin runs in the same
// privileged container over the same /lib/modules, and can ask the kernel
// itself and say what it was told.
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/kube"
)

// The two names VDO goes by. dm-vdo is the in-tree module (kernel 6.9 and
// newer) and kvdo is what RHEL-family systems ship it as through the separate
// kmod-kvdo package, verified live on a Rocky/RHEL 9 kernel carrying no dm-vdo
// at all. Either one loading means the node can run VDO, so both are tried and
// both answers are reported.
var vdoModules = []string{"dm-vdo", "kvdo"}

// commandRunner runs one command and returns what it said, combined, whether or
// not it succeeded. It is a seam so a test can probe a kernel it does not have.
type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

// runCommand is the shipped runner. Output is combined because what a failed
// modprobe says is the whole point of asking it.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// AdvertiseVDOCapability probes this node's kernel and sets nodeName's
// vdo-capable label to match.
//
// An operator's hand-set label, which is one carrying no managed-by annotation,
// is left alone: that is the override a golden-image node depends on. The probe
// still runs and still reports, because an override that disagrees with the
// kernel underneath it is worth being able to see.
//
// Runs once at process start. A capability gained afterward is not noticed
// until the pod restarts (§14, Q12).
func AdvertiseVDOCapability(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	return advertiseVDOCapability(ctx, kubeClient, nodeName, runCommand)
}

func advertiseVDOCapability(
	ctx context.Context, kubeClient kubernetes.Interface, nodeName string, run commandRunner,
) error {
	capable := probeVDO(ctx, nodeName, run)

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	existing, hasLabel := node.Labels[kube.LabelVDOCapable]
	if _, managed := node.Annotations[kube.AnnoVDOCapableManagedBy]; hasLabel && !managed {
		klog.Infof("vdo probe: node %s carries a hand-set %s=%s, which this probe leaves alone "+
			"(it would have set %t)", nodeName, kube.LabelVDOCapable, existing, capable)
		return nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels":      map[string]string{kube.LabelVDOCapable: strconv.FormatBool(capable)},
			"annotations": map[string]string{kube.AnnoVDOCapableManagedBy: kube.AnnoVDOCapableManagedByAutoDetect},
		},
	})
	if err != nil {
		return fmt.Errorf("build vdo-capable label patch: %w", err)
	}

	if _, err := kubeClient.CoreV1().Nodes().Patch(
		ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch node %s with vdo-capable=%t: %w", nodeName, capable, err)
	}

	klog.Infof("vdo probe: node %s labeled %s=%t", nodeName, kube.LabelVDOCapable, capable)
	return nil
}

// probeVDO asks the kernel for VDO and says, step by step, what it was told.
//
// Every step is logged and not only the verdict, because the verdict alone
// cannot be acted on: a node that answered no and a node whose probe never ran
// leave a cluster in the same visible state, with every volume needing the
// capability unschedulable and nothing anywhere having failed. The kernel
// version, what modprobe said about each module by name, and whether LVM offers
// the segment types are between them enough to tell those apart from a log.
//
// Nothing here fails the caller. A probe is a question, and a node that cannot
// answer it still stages every volume that does not need VDO.
func probeVDO(ctx context.Context, nodeName string, run commandRunner) bool {
	if release, err := run(ctx, "uname", "-r"); err == nil {
		klog.Infof("vdo probe: node %s runs kernel %s", nodeName, release)
	} else {
		klog.Infof("vdo probe: node %s kernel version unavailable: %v", nodeName, err)
	}

	capable := false
	for _, module := range vdoModules {
		out, err := run(ctx, "modprobe", module)
		switch {
		case err == nil:
			klog.Infof("vdo probe: node %s loaded %s", nodeName, module)
			capable = true
		case out != "":
			klog.Infof("vdo probe: node %s cannot load %s: %s", nodeName, module, out)
		default:
			klog.Infof("vdo probe: node %s cannot load %s: %v", nodeName, module, err)
		}
		if capable {
			break
		}
	}

	// Reported and not acted on. Whether the probe should also require the
	// segment types is undecided (§14, Q7), and this is what will settle it: a
	// node whose module loads while LVM lists no vdo segtype is exactly the case
	// that question is about, and nothing today would show it.
	if segtypes, err := run(ctx, "lvm", "segtypes"); err == nil {
		klog.Infof("vdo probe: node %s lvm vdo segtypes: %s", nodeName, vdoSegtypes(segtypes))
	} else {
		klog.Infof("vdo probe: node %s could not list lvm segtypes: %v", nodeName, err)
	}

	return capable
}

// vdoSegtypes are the vdo segment types in `lvm segtypes` output, joined for a
// log line, and a plain "none" when there are none.
func vdoSegtypes(out string) string {
	found := make([]string, 0, 2)
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); strings.HasPrefix(name, "vdo") {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return "none"
	}
	return strings.Join(found, ", ")
}
