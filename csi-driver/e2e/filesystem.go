package e2e

import (
	"context"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("SPDKCSI-FILESYSTEM", func() {
	f := newTestFramework("spdkcsi")

	// -------------------------------------------------------------------------
	// XFS filesystem
	// -------------------------------------------------------------------------

	// A unique StorageClass name is generated per test so parallel runs do not
	// collide. The SC is created by copying the default one and overriding
	// `csi.storage.k8s.io/fstype` to `xfs`.
	ginkgo.It("XFS volume is provisioned and data persists across pod restarts", func() {
		ns := f.Namespace.Name
		const xfsSC = "spdkcsi-e2e-xfs"

		ginkgo.By("create XFS StorageClass derived from the default one")
		createStorageClassWithParams(f.ClientSet, xfsSC, map[string]string{
			"csi.storage.k8s.io/fstype": "xfs",
		})
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, xfsSC) })

		ginkgo.By("create PVC using XFS StorageClass")
		framework.ExpectNoError(
			createPVC(f.ClientSet, ns, "spdkcsi-pvc-xfs", xfsSC, 1<<30),
			"create XFS PVC",
		)
		ginkgo.DeferCleanup(func() {
			framework.ExpectNoError(
				f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
					Delete(context.Background(), "spdkcsi-pvc-xfs", metav1.DeleteOptions{}),
			)
		})

		ginkgo.By("create pod that mounts the XFS volume")
		framework.ExpectNoError(
			createPodForPVC(f.ClientSet, ns, "spdkcsi-test-xfs", "spdkcsi-pvc-xfs"),
			"create XFS test pod",
		)
		xfsPodLabel := metav1.ListOptions{LabelSelector: "app=spdkcsi-test-xfs"}
		ginkgo.DeferCleanup(func() {
			deletePodByName(f.ClientSet, ns, "spdkcsi-test-xfs")
		})

		ginkgo.By("wait for XFS pod to be ready")
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, "spdkcsi-test-xfs"),
			"wait for XFS test pod",
		)

		ginkgo.By("verify the mounted filesystem is XFS")
		// Alpine's `df` supports `-T`, and an XFS mount reports type `xfs`.
		fsType, _ := execCommandInPod(f,
			"df -T /spdkvol | awk 'NR==2 {print $2}'",
			ns, &xfsPodLabel)
		gomega.Expect(fsType).To(gomega.ContainSubstring("xfs"),
			"filesystem type should be xfs")

		ginkgo.By("write data to the XFS volume")
		writeDataToPod(f, ns, &xfsPodLabel, "xfs-persistence-test-data", "/spdkvol/test")

		ginkgo.By("delete and recreate pod to verify persistence")
		deletePodByName(f.ClientSet, ns, "spdkcsi-test-xfs")
		framework.ExpectNoError(
			waitForTestPodGone(f.ClientSet, ns, "spdkcsi-test-xfs"),
			"wait for XFS pod to terminate",
		)
		framework.ExpectNoError(
			createPodForPVC(f.ClientSet, ns, "spdkcsi-test-xfs", "spdkcsi-pvc-xfs"),
			"recreate XFS test pod",
		)
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, "spdkcsi-test-xfs"),
			"wait for XFS pod after restart",
		)

		ginkgo.By("verify data survived the pod restart")
		compareDataInPod(f, ns, &xfsPodLabel,
			[]string{"xfs-persistence-test-data"},
			[]string{"/spdkvol/test"},
		)
	})

	// -------------------------------------------------------------------------
	// ext4 filesystem (default): subdirectory writes survive pod restart
	// -------------------------------------------------------------------------

	ginkgo.It("ext4 volume supports nested directory writes that persist across pod restarts", func() {
		ns := f.Namespace.Name
		testPodLabel := metav1.ListOptions{LabelSelector: "app=spdkcsi-pvc"}

		ginkgo.By("create PVC and test pod")
		deployPVC(ns)
		deployTestPod(ns)
		ginkgo.DeferCleanup(func() { deletePVCAndTestPod(ns) })

		ginkgo.By("wait for test pod to be ready")
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, testPodName),
			"wait for test pod",
		)

		ginkgo.By("check the claim records the filesystem the volume was staged with")
		// The node plugin records this best effort: a claim it cannot resolve or
		// patch leaves a warning in its log and nothing else. That annotation is
		// the whole of what keeps a blkid reading of "nothing here" from
		// reformatting a volume whose device merely could not be read, so a
		// driver that quietly stopped writing it (a narrowed RBAC, a claim it no
		// longer resolves) disarms the guard with no other symptom. A cluster is
		// the only place that shows up: the unit tests patch a fake client, which
		// has no RBAC to get wrong.
		gomega.Eventually(func() string {
			return pvcAnnotation(f, ns, defaultPVCName, annotationOnDiskFilesystem)
		}, 2*time.Minute, 5*time.Second).Should(gomega.Equal("ext4"),
			"the node plugin never recorded the on-disk filesystem on claim %s", defaultPVCName)

		ginkgo.By("create a subdirectory and write data")
		execCommandInPod(f, "mkdir -p /spdkvol/subdir/nested", ns, &testPodLabel)
		writeDataToPod(f, ns, &testPodLabel, "nested-dir-data", "/spdkvol/subdir/nested/file")

		ginkgo.By("delete and recreate pod")
		deleteTestPod(ns)
		framework.ExpectNoError(
			waitForTestPodGone(f.ClientSet, ns, testPodName),
			"wait for test pod to terminate",
		)
		deployTestPod(ns)
		framework.ExpectNoError(
			waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, testPodName),
			"wait for test pod after restart",
		)

		ginkgo.By("verify nested directory and data survived the restart")
		compareDataInPod(f, ns, &testPodLabel,
			[]string{"nested-dir-data"},
			[]string{"/spdkvol/subdir/nested/file"},
		)
	})
})

// defaultPVCName is the claim deployPVC creates, from templates/pvc.yaml.
const defaultPVCName = "spdkcsi-pvc"

// annotationOnDiskFilesystem is the claim annotation the node plugin records the
// staged filesystem in, and reads back to settle a device blkid could not read.
// The literal is repeated here rather than imported because the driver's own
// constant is unexported, and a shared one would let a renamed annotation pass:
// what this asserts is the string the driver writes to a live cluster.
const annotationOnDiskFilesystem = "storage.simplyblock.io/on-disk-filesystem"

// pvcAnnotation reads one annotation off a claim, returning the empty string
// when the claim or the annotation is not there yet.
func pvcAnnotation(f *framework.Framework, ns, pvcName, key string) string {
	pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), pvcName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return pvc.Annotations[key]
}
