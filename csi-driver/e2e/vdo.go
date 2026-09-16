// Live verification that client-side compression and deduplication (VDO)
// actually reduce physical usage, not just that a volume carrying those
// StorageClass parameters mounts and is writable (params.go already covers
// that shape of check for every other parameter). Each spec reads vdostats
// inside the csi-node pod hosting the volume, so a regression that silently
// drops the VDO layer (falls back to a plain LVM volume, say) would still
// pass every other suite in this package but fail here.
//
// The csi-node image needs the "vdo" package (vdoformat) for lvcreate --type
// vdo to succeed; deploy/image/Dockerfile_base installs it, but only the base
// image build (a separate, cron/manual-triggered workflow) bakes it in, not
// the per-branch app image this suite runs against. A run against an image
// missing vdoformat fails at NodeStageVolume, not inside these specs.
package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	// vdoVolumeSize is well above vdoMinimumVolumeSize's 5Gi floor (the
	// operator's admission webhook enforces that minimum): VDO's own fixed
	// metadata overhead (multiple GiB, confirmed live) dominates a volume at
	// the floor, leaving too little logical headroom for the payload sizes
	// below to show a clean before/after signal.
	vdoVolumeSize = 20 << 30 // 20Gi

	// compressiblePayloadSize and duplicatePayloadSize are each comfortably
	// inside vdoVolumeSize's logical headroom once VDO's own overhead is
	// subtracted, and large enough that the resulting block-count delta is
	// not noise from filesystem metadata.
	compressiblePayloadSize = 200 << 20 // 200MiB
	duplicatePayloadSize    = 200 << 20 // 200MiB
)

var _ = ginkgo.Describe("SPDKCSI-VDO", func() {
	f := newTestFramework("spdkcsi")

	// -------------------------------------------------------------------------
	// Client-side compression
	// -------------------------------------------------------------------------

	ginkgo.It("StorageClass with client_compression=true measurably reduces physical usage", func() {
		ns := f.Namespace.Name
		const (
			scName  = "spdkcsi-e2e-vdo-compression"
			pvcName = "spdkcsi-pvc-vdo-compression"
			podName = "spdkcsi-test-vdo-compression"
		)

		ginkgo.By("create a StorageClass asking for client-side compression only")
		createStorageClass(f, scName, map[string]string{
			"client_compression": trueStr,
		}, nil)
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

		ginkgo.By("create PVC and pod")
		framework.ExpectNoError(createPVC(f.ClientSet, ns, pvcName, scName, vdoVolumeSize), "create compression PVC")
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), pvcName, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, podName, pvcName), "create compression test pod")
		ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, podName) })

		ginkgo.By("wait for pod to be ready")
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, podName),
			"wait for compression test pod",
		)

		workerNode := testPodNode(f.ClientSet, ns, podName)
		pluginPod, pluginContainer := nodePluginPodOnNode(f.ClientSet, workerNode)
		lvolID := lvolIDForPVC(f.ClientSet, ns, pvcName)
		device := vdoDeviceName(f, pluginPod, pluginContainer, lvolID)

		podLabel := metav1.ListOptions{LabelSelector: "app=" + podName}

		ginkgo.By("read physical/logical usage before writing anything")
		baseline := readVDOStats(f, pluginPod, pluginContainer, device)

		ginkgo.By("write a highly compressible payload")
		// A repeated line compresses well under VDO's per-block LZ4 (each ~4KB
		// block is compressed independently of any other), and dedup is off on
		// this class, so no block can be eliminated by matching another one —
		// any saving measured here can only be compression.
		execCommandInPod(f,
			fmt.Sprintf("yes 'vdo-e2e-compressible-payload-line' | head -c %d > /spdkvol/compressible.bin && sync",
				compressiblePayloadSize),
			ns, &podLabel)

		ginkgo.By("verify physical usage grew far less than logical usage")
		after := readVDOStats(f, pluginPod, pluginContainer, device)
		logicalGrowth := after.logicalBlocksUsed - baseline.logicalBlocksUsed
		dataGrowth := after.dataBlocksUsed - baseline.dataBlocksUsed
		gomega.Expect(logicalGrowth).To(gomega.BeNumerically(">", 0),
			"writing the payload should have grown logical usage")
		gomega.Expect(dataGrowth).To(gomega.BeNumerically("<", logicalGrowth/2),
			"compression should keep physical growth (%d blocks) well under logical growth (%d blocks)",
			dataGrowth, logicalGrowth)
	})

	// -------------------------------------------------------------------------
	// Client-side deduplication
	// -------------------------------------------------------------------------

	ginkgo.It("StorageClass with client_deduplication=true eliminates space for duplicate writes", func() {
		ns := f.Namespace.Name
		const (
			scName  = "spdkcsi-e2e-vdo-dedup"
			pvcName = "spdkcsi-pvc-vdo-dedup"
			podName = "spdkcsi-test-vdo-dedup"
		)

		ginkgo.By("create a StorageClass asking for client-side deduplication only")
		createStorageClass(f, scName, map[string]string{
			"client_deduplication": trueStr,
		}, nil)
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

		ginkgo.By("create PVC and pod")
		framework.ExpectNoError(createPVC(f.ClientSet, ns, pvcName, scName, vdoVolumeSize), "create dedup PVC")
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), pvcName, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, podName, pvcName), "create dedup test pod")
		ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, podName) })

		ginkgo.By("wait for pod to be ready")
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, podName),
			"wait for dedup test pod",
		)

		workerNode := testPodNode(f.ClientSet, ns, podName)
		pluginPod, pluginContainer := nodePluginPodOnNode(f.ClientSet, workerNode)
		lvolID := lvolIDForPVC(f.ClientSet, ns, pvcName)
		device := vdoDeviceName(f, pluginPod, pluginContainer, lvolID)

		podLabel := metav1.ListOptions{LabelSelector: "app=" + podName}

		ginkgo.By("write one incompressible blob and record physical usage")
		// /dev/urandom defeats compression, so any saving measured after the
		// duplicate copies below can only be deduplication, not compression —
		// this class does not even ask for compression, but the payload stays
		// incompressible regardless so the two mechanisms cannot be conflated.
		execCommandInPod(f,
			fmt.Sprintf("dd if=/dev/urandom of=/spdkvol/blob.bin bs=1M count=%d 2>/dev/null && sync",
				duplicatePayloadSize/(1<<20)),
			ns, &podLabel)
		afterOne := readVDOStats(f, pluginPod, pluginContainer, device)

		ginkgo.By("copy the same blob nine more times")
		for i := 1; i <= 9; i++ {
			execCommandInPod(f, fmt.Sprintf("cp /spdkvol/blob.bin /spdkvol/blob-copy-%d.bin", i), ns, &podLabel)
		}
		execCommandInPod(f, "sync", ns, &podLabel)

		ginkgo.By("verify physical usage stayed flat while logical usage grew ninefold")
		afterTen := readVDOStats(f, pluginPod, pluginContainer, device)
		logicalGrowth := afterTen.logicalBlocksUsed - afterOne.logicalBlocksUsed
		dataGrowth := afterTen.dataBlocksUsed - afterOne.dataBlocksUsed
		gomega.Expect(logicalGrowth).To(gomega.BeNumerically(">", 0),
			"copying nine duplicates should have grown logical usage")
		gomega.Expect(dataGrowth).To(gomega.BeNumerically("<", logicalGrowth/4),
			"deduplication should keep physical growth (%d blocks) far under logical growth (%d blocks) "+
				"for nine exact duplicates", dataGrowth, logicalGrowth)
	})
})

// vdoDeviceName resolves the VDO pool device backing lvolID inside the
// csi-node pod. LVM mangles a literal '-' in a device-mapper name as '--', so
// matching the escaped id is what actually appears under /dev/mapper,
// regardless of the volume group/logical volume naming scheme in use.
//
// A lvol's VDO stack always shows two matching /dev/mapper entries: the pool
// device itself (suffix "-vdopool-vpool," what vdostats operates on) and its
// backing data volume (suffix "-vdopool_vdata," vdostats refuses it outright
// as "Not a valid running VDO device") — the trailing "-vpool" anchor is what
// tells the two apart.
func vdoDeviceName(f *framework.Framework, podName, container, lvolID string) string {
	mangled := strings.ReplaceAll(lvolID, "-", "--")
	out := execInPod(f, driverNamespace(), podName, container,
		fmt.Sprintf("ls /dev/mapper | grep %s | grep -- -vpool$", mangled))
	name := strings.TrimSpace(out)
	gomega.Expect(name).NotTo(gomega.BeEmpty(), "find VDO pool device for lvol %s", lvolID)
	gomega.Expect(name).NotTo(gomega.ContainSubstring("\n"),
		"expected exactly one VDO pool device for lvol %s, got:\n%s", lvolID, name)
	return name
}

// vdoStats is the subset of `vdostats --verbose` this suite asserts on: the
// physical blocks VDO actually wrote (dataBlocksUsed) against what the
// filesystem believes it wrote (logicalBlocksUsed). Compression and
// deduplication both work by keeping the former from growing in step with
// the latter.
type vdoStats struct {
	dataBlocksUsed    int64
	logicalBlocksUsed int64
}

// readVDOStats runs vdostats --verbose against device inside the csi-node pod
// and parses the two fields vdoStats needs.
//
// device is passed bare, not as /dev/mapper/<device>: confirmed live that
// vdostats 8.3.2.1 reliably resolves a device by its dmsetup name but
// intermittently reports "Not a valid running VDO device" for the identical
// target given as a full /dev/mapper path, even with dmsetup itself reporting
// the mapping ACTIVE/LIVE at that moment.
func readVDOStats(f *framework.Framework, podName, container, device string) vdoStats {
	out := execInPod(f, driverNamespace(), podName, container,
		"vdostats --verbose "+device)
	return vdoStats{
		dataBlocksUsed:    vdoStatField(out, "data blocks used"),
		logicalBlocksUsed: vdoStatField(out, "logical blocks used"),
	}
}

// vdoStatField extracts one "name: value" line's value from vdostats
// --verbose output. It fails the spec outright (rather than returning a zero
// that would silently pass a "grew by more than" assertion) since a field
// vdostats did not report means the device was not resolved correctly.
func vdoStatField(out, field string) int64 {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, field) {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err == nil {
			return v
		}
	}
	ginkgo.Fail(fmt.Sprintf("vdostats output has no parsable %q field:\n%s", field, out))
	return 0
}
