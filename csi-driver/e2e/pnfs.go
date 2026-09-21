// The pNFS specs: a ReadWriteMany volume served by an export, and the property
// the whole feature exists for, which is that its data never touches the
// metadata server.
//
// That property is not observable from the outside. The mount looks like any
// NFS mount and the bytes land either way, so a spec that only wrote a file and
// read it back would pass just as well on a client whose layout was refused and
// whose every byte went through the MDS. The client's own operation counters
// are what tell the two apart.

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
	// pnfsFSType is what a StorageClass names to be served by an export rather
	// than by a block device.
	pnfsFSType = "pnfs"

	// probeBytes is what the bypass spec writes. Large enough that a client
	// routing it through the metadata server could not avoid issuing NFS WRITEs,
	// and small enough to flush in seconds.
	probeBytes = 64 << 20
)

var _ = ginkgo.Describe("SPDKCSI-PNFS", func() {
	f := newTestFramework("spdkcsi")

	ginkgo.It("a pNFS volume mounts ReadWriteMany over NFSv4.1", func() {
		ns := f.Namespace.Name
		const pvcName, podName = "spdkcsi-pvc-pnfs", "spdkcsi-test-pnfs"
		label := metav1.ListOptions{LabelSelector: "app=" + podName}

		pnfsVolume(f, ns, pvcName, podName)

		ginkgo.By("verify the mount is NFSv4.1, which is the floor a layout exists at")
		opts, _ := execCommandInPod(f, "grep -m1 ' /spdkvol ' /proc/self/mounts", ns, &label)
		gomega.Expect(opts).To(gomega.ContainSubstring("nfs4"),
			"/spdkvol should be an NFSv4 mount, got: %s", opts)
		gomega.Expect(opts).To(gomega.ContainSubstring("vers=4.1"),
			"layouts do not exist before NFSv4.1, got: %s", opts)

		ginkgo.By("verify the volume is writable and reads back")
		out, stderr := execCommandInPod(f,
			"echo pnfs-ok > /spdkvol/probe && cat /spdkvol/probe", ns, &label)
		gomega.Expect(stderr).To(gomega.BeEmpty(), "write and read on a pNFS volume should succeed")
		gomega.Expect(strings.TrimSpace(out)).To(gomega.Equal("pnfs-ok"))
	})

	ginkgo.It("writes reach the storage nodes without passing through the metadata server", func() {
		ns := f.Namespace.Name
		const pvcName, podName = "spdkcsi-pvc-pnfs-direct", "spdkcsi-test-pnfs-direct"
		label := metav1.ListOptions{LabelSelector: "app=" + podName}

		pnfsVolume(f, ns, pvcName, podName)

		ginkgo.By("read the client's per-operation counters before writing")
		before := nfsOpCounts(f, ns, &label)

		ginkgo.By(fmt.Sprintf("write %d bytes and flush them", probeBytes))
		_, stderr := execCommandInPod(f, fmt.Sprintf(
			"dd if=/dev/urandom of=/spdkvol/probe bs=1M count=%d conv=fsync 2>/dev/null", probeBytes>>20),
			ns, &label)
		gomega.Expect(stderr).To(gomega.BeEmpty(), "writing the probe file should succeed")

		ginkgo.By("confirm the bytes were actually written")
		// Without this the assertion below passes on a volume nothing was ever
		// written to, which is the one way "no NFS WRITEs" means nothing.
		size, _ := execCommandInPod(f, "stat -c %s /spdkvol/probe", ns, &label)
		gomega.Expect(strings.TrimSpace(size)).To(gomega.Equal(strconv.Itoa(probeBytes)),
			"the probe file should be %d bytes", probeBytes)

		after := nfsOpCounts(f, ns, &label)

		ginkgo.By("verify the client was handed a layout")
		// A client that got none has nothing to write through but the server,
		// and falls back to doing exactly that without saying so.
		gomega.Expect(after["LAYOUTGET"]-before["LAYOUTGET"]).To(gomega.BeNumerically(">", 0),
			"the client issued no LAYOUTGET, so it never asked for a layout")

		ginkgo.By("verify no data went through the metadata server")
		gomega.Expect(after["WRITE"]-before["WRITE"]).To(gomega.BeZero(),
			"the client issued %d NFS WRITEs for %d bytes, so the data routed through the metadata "+
				"server instead of going to the storage nodes directly",
			after["WRITE"]-before["WRITE"], probeBytes)

		ginkgo.By("verify the extent was committed back to the metadata server")
		// The counterpart of the bypass: the data went around the MDS, so the
		// client has to tell it what it wrote, or the file's size is never
		// updated for anybody else.
		gomega.Expect(after["LAYOUTCOMMIT"]-before["LAYOUTCOMMIT"]).To(gomega.BeNumerically(">", 0),
			"the client issued no LAYOUTCOMMIT, so the metadata server was never told what it wrote")
	})

	ginkgo.It("two pods on different nodes write to one volume", func() {
		ns := f.Namespace.Name
		const pvcName = "spdkcsi-pvc-pnfs-rwx"
		const firstPod, secondPod = "spdkcsi-test-pnfs-a", "spdkcsi-test-pnfs-b"

		nodes := schedulableNodeNames(f.ClientSet)
		if len(nodes) < 2 {
			ginkgo.Skip("ReadWriteMany across nodes needs two schedulable nodes")
		}

		scName := ns + "-pnfs-sc"
		createStorageClass(f, scName, map[string]string{scParamFSType: pnfsFSType}, nil)
		ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

		framework.ExpectNoError(
			createRWXPVC(f.ClientSet, ns, pvcName, scName, 1<<30), "create the pNFS PVC")
		ginkgo.DeferCleanup(func() { deletePVCByName(f.ClientSet, ns, pvcName) })

		for pod, node := range map[string]string{firstPod: nodes[0], secondPod: nodes[1]} {
			framework.ExpectNoError(
				createPodForPVCOnNode(f.ClientSet, ns, pod, pvcName, node), "create pod %s", pod)
			ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, pod) })
		}

		for _, pod := range []string{firstPod, secondPod} {
			framework.ExpectNoError(
				waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, pod), "wait for pod %s", pod)
		}

		first := metav1.ListOptions{LabelSelector: "app=" + firstPod}
		second := metav1.ListOptions{LabelSelector: "app=" + secondPod}

		ginkgo.By("write from the first node")
		_, stderr := execCommandInPod(f, "echo from-a > /spdkvol/a && sync", ns, &first)
		gomega.Expect(stderr).To(gomega.BeEmpty(), "the first pod should be able to write")

		ginkgo.By("read it on the second node, and write back")
		out, stderr := execCommandInPod(f, "cat /spdkvol/a && echo from-b > /spdkvol/b && sync", ns, &second)
		gomega.Expect(stderr).To(gomega.BeEmpty(), "the second pod should see the first pod's file")
		gomega.Expect(out).To(gomega.ContainSubstring("from-a"))

		ginkgo.By("read the second node's file back on the first")
		out, _ = execCommandInPod(f, "cat /spdkvol/b", ns, &first)
		gomega.Expect(strings.TrimSpace(out)).To(gomega.Equal("from-b"))
	})
})

// pnfsVolume provisions a pNFS class, a ReadWriteMany claim, and one pod on it,
// and returns once the pod is running. Every cleanup is registered here.
func pnfsVolume(f *framework.Framework, ns, pvcName, podName string) {
	scName := ns + "-" + podName + "-sc"

	ginkgo.By("create a StorageClass with fstype " + pnfsFSType)
	createStorageClass(f, scName, map[string]string{scParamFSType: pnfsFSType}, nil)
	ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

	ginkgo.By("create a ReadWriteMany PVC")
	framework.ExpectNoError(
		createRWXPVC(f.ClientSet, ns, pvcName, scName, 1<<30), "create the pNFS PVC")
	ginkgo.DeferCleanup(func() { deletePVCByName(f.ClientSet, ns, pvcName) })

	ginkgo.By("create a pod that mounts it")
	framework.ExpectNoError(
		createPodForPVC(f.ClientSet, ns, podName, pvcName), "create the pNFS test pod")
	ginkgo.DeferCleanup(func() { deletePodByName(f.ClientSet, ns, podName) })

	ginkgo.By("wait for the pod to be ready, which is the export assembled and mounted")
	framework.ExpectNoError(
		waitForTestPodReady(f.ClientSet, 5*time.Minute, ns, podName), "wait for the pNFS test pod")
}

// nfsOpCounts is what the client reports it has issued against its NFS mount.
func nfsOpCounts(f *framework.Framework, ns string, pod *metav1.ListOptions) map[string]int64 {
	out, _ := execCommandInPod(f, "cat /proc/self/mountstats", ns, pod)
	counts, err := parseNFSOpCounts(out, "/spdkvol")
	framework.ExpectNoError(err, "read the NFS operation counters")
	return counts
}

// createRWXPVC writes a ReadWriteMany claim, which is what a pNFS volume is for
// and what the block path refuses.
func createRWXPVC(c kubernetes.Interface, ns, pvcName, scName string, size int64) error {
	_, err := c.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(size, resource.BinarySI),
				},
			},
		},
	}, metav1.CreateOptions{})
	return err
}

// createPodForPVCOnNode is createPodForPVC pinned to one node, so that a
// ReadWriteMany spec is about two nodes rather than about two pods that both
// landed on one.
func createPodForPVCOnNode(c kubernetes.Interface, ns, podName, pvcName, nodeName string) error {
	_, err := c.CoreV1().Pods(ns).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: map[string]string{"app": podName},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{corev1.LabelHostname: nodeName},
			Containers: []corev1.Container{{
				Name:         "alpine",
				Image:        "alpine:3",
				Command:      []string{"sleep", "365d"},
				VolumeMounts: []corev1.VolumeMount{{Name: "vol", MountPath: "/spdkvol"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "vol",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				},
			}},
		},
	}, metav1.CreateOptions{})
	return err
}

// deletePVCByName removes a claim, logging rather than failing: it runs in a
// cleanup, where the spec's own result is what matters.
func deletePVCByName(c kubernetes.Interface, ns, pvcName string) {
	if err := c.CoreV1().PersistentVolumeClaims(ns).
		Delete(context.Background(), pvcName, metav1.DeleteOptions{}); err != nil {
		framework.Logf("failed to delete PVC %s: %v", pvcName, err)
	}
}

// schedulableNodeNames are the nodes a pod can land on: Ready, and not tainted
// against ordinary workloads.
func schedulableNodeNames(c kubernetes.Interface) []string {
	list, err := c.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	framework.ExpectNoError(err, "list nodes")

	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		node := &list.Items[i]
		if node.Spec.Unschedulable || !nodeIsReady(node) || hasNoScheduleTaint(node) {
			continue
		}
		names = append(names, node.Name)
	}
	return names
}

func nodeIsReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasNoScheduleTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}
