// Inducements that break a volume's NVMe-oF data path without taking the device
// away with it, and the sysfs knobs that decide whether a broken path errors or
// waits.
//
// The suite already has an inducement for total path loss: `nvme disconnect`,
// which removes the controllers and, with them, the namespace head. That models
// an outage the driver recovers from by connecting again, and it is the wrong
// tool for any defect that only exists while the device node is still there and
// its reads no longer work. Blackholing the endpoints leaves every controller in
// place, reconnecting, so the head survives and I/O against it fails — which is
// the state a node actually sits in between losing the fabric and ctrl_loss_tmo
// expiring.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

// blackholeImage is the image the blackhole pod runs. It only has to hold a
// shell and nsenter; the iptables it drives is the host's own.
const blackholeImage = "alpine:3"

// parsePathEndpoint pulls "host:port" out of an nvme-cli path address, which is
// a comma-separated key=value list ("traddr=...,trsvcid=...,src_addr=...").
// Reports false for an address carrying neither.
func parsePathEndpoint(address string) (string, bool) {
	var traddr, trsvcid string
	for _, kv := range strings.Split(address, ",") {
		switch {
		case strings.HasPrefix(kv, "traddr="):
			traddr = strings.TrimPrefix(kv, "traddr=")
		case strings.HasPrefix(kv, "trsvcid="):
			trsvcid = strings.TrimPrefix(kv, "trsvcid=")
		}
	}
	if traddr == "" || trsvcid == "" {
		return "", false
	}
	return traddr + ":" + trsvcid, true
}

// pathEndpoints returns the distinct transport endpoints behind a subsystem's
// paths, which is what has to be blackholed to take the whole volume off the
// fabric rather than degrade it to one leg.
func pathEndpoints(sub *nvmeSubsystem) []string {
	seen := map[string]bool{}
	endpoints := make([]string, 0, len(sub.Paths))
	for _, p := range sub.Paths {
		endpoint, ok := parsePathEndpoint(p.Address)
		if !ok || seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		endpoints = append(endpoints, endpoint)
	}
	return endpoints
}

// failFastOnPathLoss makes a volume's controllers give up on queued I/O instead
// of holding it until a path returns.
//
// This is not cosmetic, and it is not the test making the failure worse than it
// is. With the driver's own settings a controller queues I/O for the whole of
// ctrl_loss_tmo, so a reader of the device blocks in uninterruptible I/O rather
// than seeing an error — unkillable, and invisible to any timeout the test could
// wrap around it. fast_io_fail_tmo is what converts that wait into the EIO a
// real reader eventually gets, and it is the only way to reach that state on a
// bounded schedule.
//
// ctrlLoss is raised at the same time, and has to be: it bounds how long the
// controllers survive the outage at all, and once they are gone so is the device
// node, which ends the state under test.
func failFastOnPathLoss(f *framework.Framework, w managedWorkload, fastIOFail, ctrlLoss int) {
	for _, p := range w.sub.Paths {
		// ctrl_loss_tmo first: the kernel rejects a fast_io_fail_tmo above it.
		execInPod(f, driverNamespace(), w.pluginPod, w.pluginContainer, fmt.Sprintf(
			"echo %d > /sys/class/nvme/%s/ctrl_loss_tmo && echo %d > /sys/class/nvme/%s/fast_io_fail_tmo",
			ctrlLoss, p.Name, fastIOFail, p.Name,
		))
	}
	framework.Logf("volume %s: controllers set to fail I/O %ds after path loss, surviving %ds",
		w.lvolID, fastIOFail, ctrlLoss)
}

// headDeviceForSubsystem returns the multipath head device of the volume's
// subsystem, the /dev node the driver itself stages. The per-controller nodes
// below it (nvme0c0n1 and friends) are hidden by NVMe multipath and are not what
// anything opens.
func headDeviceForSubsystem(f *framework.Framework, w managedWorkload) string {
	const findHead = `for s in /sys/class/nvme-subsystem/*; do
  [ -r "$s/subsysnqn" ] || continue
  [ "$(cat "$s/subsysnqn")" = "%s" ] || continue
  for d in "$s"/nvme*n*; do
    case "$(basename "$d")" in *c*n*) continue;; esac
    [ -d "$d" ] && { printf '/dev/%%s' "$(basename "$d")"; exit 0; }
  done
done`
	out := strings.TrimSpace(
		execInPod(f, driverNamespace(), w.pluginPod, w.pluginContainer, fmt.Sprintf(findHead, w.sub.NQN)),
	)
	gomega.Expect(out).To(gomega.HavePrefix("/dev/"),
		"no multipath head device found for subsystem %s on node %s", w.sub.NQN, w.workerNode)
	return out
}

// fabricBlackhole is an active set of DROP rules against a volume's NVMe-oF
// endpoints, and the pod that installed them.
type fabricBlackhole struct {
	f         *framework.Framework
	ns        string
	podName   string
	endpoints []string
	cleared   bool
}

// blackholeFabric drops every packet to the given endpoints on nodeName, taking
// the volume off the fabric while leaving its controllers and its device node in
// place.
//
// The rules are the host's, not a container's: the pod shares the host network
// namespace, so the iptables it runs there is the one that governs the node's
// own NVMe-oF traffic. It reaches the host's iptables binary through nsenter,
// because the node image, not this pod's image, is where that binary lives.
//
// Registers its own cleanup, so a failing spec cannot leave a node blackholed.
func blackholeFabric(f *framework.Framework, nodeName string, endpoints []string) *fabricBlackhole {
	gomega.Expect(endpoints).NotTo(gomega.BeEmpty(), "no endpoints to blackhole")

	b := &fabricBlackhole{
		f:         f,
		ns:        f.Namespace.Name,
		podName:   "fabric-blackhole",
		endpoints: endpoints,
	}

	yes := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: b.podName, Labels: map[string]string{"app": b.podName}},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostNetwork:   true,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "blackhole",
				Image:           blackholeImage,
				Command:         []string{"sleep", "3600"},
				SecurityContext: &corev1.SecurityContext{Privileged: &yes},
			}},
		},
	}
	_, err := f.ClientSet.CoreV1().Pods(b.ns).Create(context.Background(), pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "create blackhole pod on node %s", nodeName)
	ginkgo.DeferCleanup(b.clear)

	framework.ExpectNoError(
		waitForTestPodReady(f.ClientSet, 3*time.Minute, b.ns, b.podName),
		"blackhole pod never became ready on node %s", nodeName,
	)

	// Fail loudly rather than run a spec whose premise silently did not hold: a
	// blackhole that installed no rule looks exactly like a driver that handled
	// the outage.
	out := execInPod(f, b.ns, b.podName, "blackhole", "nsenter -t 1 -m -- iptables -V 2>&1")
	gomega.Expect(out).To(gomega.ContainSubstring("iptables"),
		"the node's iptables is not reachable from the blackhole pod: %s", out)

	for _, endpoint := range b.endpoints {
		host, port, ok := strings.Cut(endpoint, ":")
		gomega.Expect(ok).To(gomega.BeTrue(), "malformed endpoint %q", endpoint)
		execInPod(f, b.ns, b.podName, "blackhole", fmt.Sprintf(
			"nsenter -t 1 -m -- iptables -I OUTPUT 1 -p tcp -d %s --dport %s -j DROP", host, port))
	}
	framework.Logf("blackholed %v on node %s", b.endpoints, nodeName)
	return b
}

// clear removes the DROP rules and the pod that installed them. It is safe to
// call twice, since it also runs as the deferred cleanup.
func (b *fabricBlackhole) clear() {
	if b == nil || b.cleared {
		return
	}
	b.cleared = true

	for _, endpoint := range b.endpoints {
		host, port, ok := strings.Cut(endpoint, ":")
		if !ok {
			continue
		}
		// Best effort, and deliberately so: the rules have to come off even when
		// the pod is already going away, and a rule that was never installed is
		// not a cleanup failure.
		_, _, _ = execCommandInPodE(b.f, fmt.Sprintf(
			"nsenter -t 1 -m -- iptables -D OUTPUT -p tcp -d %s --dport %s -j DROP", host, port),
			b.ns, &metav1.ListOptions{LabelSelector: "app=" + b.podName})
	}

	_ = b.f.ClientSet.CoreV1().Pods(b.ns).Delete(context.Background(), b.podName, metav1.DeleteOptions{})
	framework.Logf("cleared the blackhole of %v", b.endpoints)
}
