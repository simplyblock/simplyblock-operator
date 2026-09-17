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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

	// -------------------------------------------------------------------------
	// Snapshot restore
	// -------------------------------------------------------------------------

	ginkgo.It("a volume restored from a snapshot of a VDO volume is itself VDO-backed", func() {
		ns := f.Namespace.Name
		const (
			scName = "spdkcsi-e2e-vdo-snapshot"
			// Names fixed by templates/snapshot-only.yaml (deploySnapshotOnly).
			srcPVCName   = "spdkcsi-pvc"
			srcPodName   = "spdkcsi-test-vdo-snapshot-src"
			snapshotName = "spdk-snapshot-deletion-test"
			restorePVC   = "spdkcsi-pvc-vdo-restore"
			restorePod   = "spdkcsi-test-vdo-restore"
			dataMarker   = "vdo-e2e-snapshot-restore-marker"
		)

		ginkgo.By("create a StorageClass asking for client-side compression")
		createStorageClass(f, scName, map[string]string{
			"client_compression": trueStr,
		}, nil)
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

		ginkgo.By("create source PVC and pod, write a data marker")
		framework.ExpectNoError(createPVC(f.ClientSet, ns, srcPVCName, scName, vdoVolumeSize), "create source PVC")
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), srcPVCName, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, srcPodName, srcPVCName), "create source pod")
		ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, srcPodName) })
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, srcPodName),
			"wait for source pod",
		)
		srcPodLabel := metav1.ListOptions{LabelSelector: "app=" + srcPodName}
		writeDataToPod(f, ns, &srcPodLabel, dataMarker, "/spdkvol/marker")
		// writeDataToPod does not sync, and this spec's source pod stays mounted.
		execCommandInPod(f, "sync", ns, &srcPodLabel)

		ginkgo.By("snapshot the source volume")
		deploySnapshotOnly(ns)
		ginkgo.DeferCleanup(func() { deleteSnapshotOnly(ns) })
		framework.ExpectNoError(
			waitForSnapshotReady(ns, snapshotName, 3*time.Minute),
			"wait for snapshot to be ready",
		)

		ginkgo.By("restore the snapshot into a new PVC on the same VDO StorageClass")
		framework.ExpectNoError(
			createPVCFromDataSource(f.ClientSet, ns, restorePVC, scName, resource.MustParse("20Gi"),
				corev1.TypedLocalObjectReference{
					APIGroup: strPtr("snapshot.storage.k8s.io"),
					Kind:     "VolumeSnapshot",
					Name:     snapshotName,
				}),
			"create restore PVC",
		)
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), restorePVC, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, restorePod, restorePVC), "create restore pod")
		ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, restorePod) })
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, restorePod),
			"wait for restore pod",
		)

		ginkgo.By("verify the restored volume carries the source's data")
		restorePodLabel := metav1.ListOptions{LabelSelector: "app=" + restorePod}
		compareDataInPod(f, ns, &restorePodLabel, []string{dataMarker}, []string{"/spdkvol/marker"})

		ginkgo.By("verify the restored volume has its own active VDO device")
		workerNode := testPodNode(f.ClientSet, ns, restorePod)
		pluginPod, pluginContainer := nodePluginPodOnNode(f.ClientSet, workerNode)
		lvolID := lvolIDForPVC(f.ClientSet, ns, restorePVC)
		device := vdoDeviceName(f, pluginPod, pluginContainer, lvolID)
		readVDOStats(f, pluginPod, pluginContainer, device) // fails the spec if not a real, running VDO device
	})

	// -------------------------------------------------------------------------
	// Clone
	// -------------------------------------------------------------------------

	ginkgo.It("a volume cloned from a VDO volume is itself VDO-backed", func() {
		ns := f.Namespace.Name
		const (
			scName     = "spdkcsi-e2e-vdo-clone"
			srcPVCName = "spdkcsi-pvc-vdo-clone-src"
			srcPodName = "spdkcsi-test-vdo-clone-src"
			clonePVC   = "spdkcsi-pvc-vdo-clone"
			clonePod   = "spdkcsi-test-vdo-clone"
			dataMarker = "vdo-e2e-clone-marker"
		)

		ginkgo.By("create a StorageClass asking for client-side deduplication")
		createStorageClass(f, scName, map[string]string{
			"client_deduplication": trueStr,
		}, nil)
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

		ginkgo.By("create source PVC and pod, write a data marker")
		framework.ExpectNoError(createPVC(f.ClientSet, ns, srcPVCName, scName, vdoVolumeSize), "create source PVC")
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), srcPVCName, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, srcPodName, srcPVCName), "create source pod")
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, srcPodName),
			"wait for source pod",
		)
		srcPodLabel := metav1.ListOptions{LabelSelector: "app=" + srcPodName}
		writeDataToPod(f, ns, &srcPodLabel, dataMarker, "/spdkvol/marker")

		// Mirrors SPDKCSI-CLONE: some backends require the source unused to clone.
		ginkgo.By("delete source pod before cloning")
		deletePodByName(f.ClientSet, ns, srcPodName)
		framework.ExpectNoError(
			waitForTestPodGone(f.ClientSet, ns, srcPodName),
			"wait for source pod to terminate",
		)

		ginkgo.By("clone the source PVC on the same VDO StorageClass")
		framework.ExpectNoError(
			createPVCFromDataSource(f.ClientSet, ns, clonePVC, scName, resource.MustParse("20Gi"),
				corev1.TypedLocalObjectReference{
					Kind: "PersistentVolumeClaim",
					Name: srcPVCName,
				}),
			"create clone PVC",
		)
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), clonePVC, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, clonePod, clonePVC), "create clone pod")
		ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, clonePod) })
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, clonePod),
			"wait for clone pod",
		)

		ginkgo.By("verify the clone carries the source's data")
		clonePodLabel := metav1.ListOptions{LabelSelector: "app=" + clonePod}
		compareDataInPod(f, ns, &clonePodLabel, []string{dataMarker}, []string{"/spdkvol/marker"})

		ginkgo.By("verify the clone has its own active VDO device")
		workerNode := testPodNode(f.ClientSet, ns, clonePod)
		pluginPod, pluginContainer := nodePluginPodOnNode(f.ClientSet, workerNode)
		lvolID := lvolIDForPVC(f.ClientSet, ns, clonePVC)
		device := vdoDeviceName(f, pluginPod, pluginContainer, lvolID)
		readVDOStats(f, pluginPod, pluginContainer, device)
	})

	// -------------------------------------------------------------------------
	// Online expansion
	// -------------------------------------------------------------------------

	ginkgo.It("a VDO volume can be expanded online, growing both the VDO stack and the filesystem", func() {
		ns := f.Namespace.Name
		const (
			scName     = "spdkcsi-e2e-vdo-expand"
			pvcName    = "spdkcsi-pvc-vdo-expand"
			podName    = "spdkcsi-test-vdo-expand"
			dataMarker = "vdo-e2e-expand-marker"
			startSize  = 6 << 30 // 6Gi: comfortably above the 5Gi floor
		)
		expandedSize := resource.MustParse("12Gi")

		ginkgo.By("create a StorageClass asking for client-side compression")
		createStorageClass(f, scName, map[string]string{
			"client_compression": trueStr,
		}, nil)
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

		ginkgo.By("create PVC and pod, write a data marker")
		framework.ExpectNoError(createPVC(f.ClientSet, ns, pvcName, scName, startSize), "create PVC")
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), pvcName, metav1.DeleteOptions{}),
			)
		})
		framework.ExpectNoError(createPodForPVC(f.ClientSet, ns, podName, pvcName), "create test pod")
		ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, podName) })
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, podName),
			"wait for test pod",
		)
		podLabel := metav1.ListOptions{LabelSelector: "app=" + podName}
		writeDataToPod(f, ns, &podLabel, dataMarker, "/spdkvol/marker")

		workerNode := testPodNode(f.ClientSet, ns, podName)
		pluginPod, pluginContainer := nodePluginPodOnNode(f.ClientSet, workerNode)
		lvolID := lvolIDForPVC(f.ClientSet, ns, pvcName)
		device := vdoDeviceName(f, pluginPod, pluginContainer, lvolID)
		before := readVDOStats(f, pluginPod, pluginContainer, device)

		ginkgo.By("resize the PVC")
		framework.ExpectNoError(resizePVC(f.ClientSet, ns, pvcName, expandedSize), "resize PVC")

		ginkgo.By("wait for PVC status capacity to reflect the new size")
		framework.ExpectNoError(
			waitForPVCStorageCapacity(f.ClientSet, ns, pvcName, expandedSize, 5*time.Minute),
			"wait for PVC capacity",
		)

		ginkgo.By("wait for the filesystem inside the pod to reflect the new size")
		framework.ExpectNoError(
			waitForFilesystemSize(f, ns, &podLabel, "/spdkvol", expandedSize.Value()*9/10, 5*time.Minute),
			"wait for filesystem resize",
		)

		ginkgo.By("verify the VDO pool device itself grew, and the data survived")
		after := readVDOStats(f, pluginPod, pluginContainer, device)
		gomega.Expect(after.physicalBlocks).To(gomega.BeNumerically(">", before.physicalBlocks),
			"the VDO pool's own physical capacity (%d blocks) should have grown past its pre-resize size (%d blocks)",
			after.physicalBlocks, before.physicalBlocks)
		compareDataInPod(f, ns, &podLabel, []string{dataMarker}, []string{"/spdkvol/marker"})
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
	physicalBlocks    int64
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
		physicalBlocks:    vdoStatField(out, "physical blocks"),
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

// createPVCFromDataSource creates a PVC populated from source — a
// PersistentVolumeClaim to clone or a VolumeSnapshot to restore — on scName,
// requesting size. createPVC (utils.go) has no equivalent: a data source is
// its own concern, not a variant of a plain PVC's.
func createPVCFromDataSource(
	c kubernetes.Interface, ns, pvcName, scName string, size resource.Quantity, source corev1.TypedLocalObjectReference,
) error {
	_, err := c.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
			DataSource:       &source,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}, metav1.CreateOptions{})
	return err
}

// strPtr returns a pointer to s, for the one-off *string fields Kubernetes API
// types carry (TypedLocalObjectReference.APIGroup here).
func strPtr(s string) *string { return &s }
