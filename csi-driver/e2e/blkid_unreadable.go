// The staging decision on a device blkid cannot read: does the node plugin
// format it?
//
// blkid answers "this device carries no filesystem" and "I could not read this
// device" with the same exit code, no output, and nothing on stderr. Measured on
// a live node, a blank device and an ext4 device whose every read fails are
// byte-identical to the caller. So a driver that treats exit 2 as "blank" will
// run mkfs over a volume that still holds data the moment its fabric breaks, and
// no test that only offers blkid a blank device would ever see it.
//
// This spec puts the driver in front of the ambiguous reading with real data
// underneath, and asserts what the volume looks like afterward rather than what
// the driver logged: the filesystem UUID written at provisioning time has to
// still be there. A reformat changes it, so the assertion cannot pass by
// accident.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	// annotationOnDiskFilesystem is the claim annotation the node plugin records
	// the staged filesystem in, and reads back to settle an unreadable device.
	annotationOnDiskFilesystem = "storage.simplyblock.io/on-disk-filesystem"

	// blkidProbe is the argv getDiskFormat runs, so what the spec observes is
	// what the driver observes rather than an approximation of it.
	blkidProbe = "blkid -p -s TYPE -s PTTYPE -o export %s; echo exit=$?"

	// blkidUUIDProbe reads the filesystem UUID, which is the identity a reformat
	// cannot preserve.
	blkidUUIDProbe = "blkid -p -s UUID -o export %s"

	// fastIOFailSeconds turns a blocked read into a failed one. See
	// failFastOnPathLoss for why a test cannot do without it.
	fastIOFailSeconds = 1

	// ctrlLossSeconds keeps the controllers, and therefore the device node,
	// alive for the whole outage. The driver's own default is a minute, which is
	// shorter than a pod replacement takes.
	ctrlLossSeconds = 900
)

var _ = ginkgo.Describe("SPDKCSI-BLKID-UNREADABLE", func() {
	f := newTestFramework("spdkcsi")

	ginkgo.Context("staging a volume whose device cannot be read", func() {
		ginkgo.It("does not reformat an ext4 volume when blkid reports nothing", func() {
			mode := fullLossMode{name: "ext4 filesystem", fsType: "ext4"}
			w := setupManagedWorkload(f, mode, "blkidunread")

			ginkgo.By("record the filesystem identity that a reformat would destroy")
			device := headDeviceForSubsystem(f, w)
			originalUUID := filesystemUUID(f, w, device)
			gomega.Expect(originalUUID).NotTo(gomega.BeEmpty(),
				"the volume was never formatted, so this spec would prove nothing")
			framework.Logf("volume %s staged on %s as ext4, UUID %s", w.lvolID, device, originalUUID)

			ginkgo.By("check the claim records the filesystem that is on the device")
			// The recorded filesystem is the only thing that distinguishes an
			// unreadable device from a blank one, so a missing annotation makes
			// the rest of the spec vacuous rather than failing it later.
			gomega.Eventually(func() string {
				return pvcAnnotation(f, w.ns, w.appLabel+"-pvc", annotationOnDiskFilesystem)
			}, 2*time.Minute, 5*time.Second).Should(gomega.Equal("ext4"),
				"the node plugin never recorded the on-disk filesystem on the claim")

			ginkgo.By("make the volume's paths fail I/O rather than wait for a path to return")
			failFastOnPathLoss(f, w, fastIOFailSeconds, ctrlLossSeconds)

			ginkgo.By("drop every active NVMe-oF path by blackholing its endpoints")
			blackhole := blackholeFabric(f, w.workerNode, pathEndpoints(w.sub))

			ginkgo.By("confirm the device is now exactly the reading the driver cannot resolve")
			// The premise of the spec, asserted rather than assumed: the node
			// still has the device, and blkid reports it the way it reports a
			// blank one.
			gomega.Eventually(func() string {
				return strings.TrimSpace(
					execInPod(f, driverNamespace(), w.pluginPod, w.pluginContainer,
						fmt.Sprintf("[ -b %s ] && echo present || echo absent", device)),
				)
			}, time.Minute, 5*time.Second).Should(gomega.Equal("present"),
				"the device node went away, so staging never reaches the blkid decision")

			gomega.Eventually(func() string {
				return probeDisk(f, w, device)
			}, 2*time.Minute, 5*time.Second).Should(gomega.Equal("exit=2"),
				"blkid never reached the unreadable-but-present state this spec is about")

			ginkgo.By("force a pod replacement, so the volume is staged against that device")
			forceDeletePod(f, w)

			// The replacement pod cannot start: the device it needs is off the
			// fabric. What matters is what the driver did while trying, and the
			// only wrong answer leaves its evidence on the device itself.
			ginkgo.By("let the node plugin work on the volume while it cannot be read")
			time.Sleep(2 * time.Minute)

			ginkgo.By("restore the fabric")
			blackhole.clear()

			ginkgo.By("the filesystem the volume was provisioned with is still the one on the device")
			gomega.Eventually(func() string {
				return filesystemUUID(f, w, headDeviceForSubsystem(f, w))
			}, 5*time.Minute, 10*time.Second).Should(gomega.Equal(originalUUID),
				"the volume was reformatted while its device could not be read: "+
					"UUID was %s. Every byte that was on it is gone.", originalUUID)

			ginkgo.By("and the volume is usable again once its paths are back")
			gomega.Eventually(func() error {
				return verifyVolumeUsableE(f, w.ns, w.appLabel, mode, "blkid-unreadable-"+w.ns)
			}, 10*time.Minute, 10*time.Second).Should(gomega.Succeed(),
				"volume never recovered after the fabric came back")
		})
	})
})

// probeDisk runs the driver's own blkid invocation against device and returns
// the trailing "exit=<code>" line, which is the whole of what getDiskFormat has
// to decide on.
func probeDisk(f *framework.Framework, w managedWorkload, device string) string {
	out := execInPod(f, driverNamespace(), w.pluginPod, w.pluginContainer, fmt.Sprintf(blkidProbe, device))
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "exit=") {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(out)
}

// filesystemUUID returns the UUID of the filesystem on device, or the empty
// string when there is none to read.
func filesystemUUID(f *framework.Framework, w managedWorkload, device string) string {
	out := execInPod(f, driverNamespace(), w.pluginPod, w.pluginContainer,
		fmt.Sprintf(blkidUUIDProbe, device))
	for _, line := range strings.Split(out, "\n") {
		if uuid, ok := strings.CutPrefix(strings.TrimSpace(line), "UUID="); ok {
			return uuid
		}
	}
	return ""
}

// pvcAnnotation reads one annotation off a claim, returning the empty string
// when the claim or the annotation is not there yet.
func pvcAnnotation(f *framework.Framework, ns, pvcName, key string) string {
	pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), pvcName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return pvc.Annotations[key]
}

// forceDeletePod removes the workload pod without a grace period, standing in
// for the guardian's restart the way the total-path-loss spec does, so the
// replacement pod's staging runs while the fabric is still down.
func forceDeletePod(f *framework.Framework, w managedWorkload) {
	zero := int64(0)
	framework.ExpectNoError(
		f.ClientSet.CoreV1().Pods(w.ns).Delete(context.Background(), w.pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}),
		"force-delete pod %s", w.pod.Name,
	)
}
