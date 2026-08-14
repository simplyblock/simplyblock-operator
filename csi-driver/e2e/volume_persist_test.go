package e2e

import (
	"context"
	"fmt"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

const (
	pvcPersistPath           = "templates/pvc-persist.yaml"
	testPodPersistWritePath  = "templates/testpod-persist-write.yaml"
	testPodPersistVerifyPath = "templates/testpod-persist-verify.yaml"

	persistWritePodName  = "persist-write"
	persistVerifyPodName = "persist-verify"
)

var _ = ginkgo.Describe("SPDKCSI-VOLUME-PERSIST", func() {
	f := newTestFramework("spdkcsi")

	ginkgo.Context("volume data persists across pod delete and immediate re-mount", func() {
		// Exercises the CSI restage path under an immediate workload handoff:
		// a write workload finishes, its pod is deleted without waiting for
		// termination, and a new pod is created at once on the same PVC.
		// This races NodeUnpublish of the old pod against NodePublish of the
		// new one and validates both volume reattach and data integrity.
		ginkgo.It("retains 1GiB fio data across pod delete and immediate re-mount", func() {
			ns := f.Namespace.Name

			ginkgo.By("check node DaemonSet is ready")
			framework.ExpectNoError(
				waitForNodeServerReady(f.ClientSet, 3*time.Minute),
				"node DaemonSet ready",
			)

			ginkgo.By("create PVC")
			framework.ExpectNoError(
				applyTemplateWithStorageClass(ns, pvcPersistPath),
				"deploy persist PVC",
			)
			ginkgo.DeferCleanup(func() {
				if _, err := e2ekubectl.RunKubectl(ns, "delete", "-f", pvcPersistPath); err != nil {
					framework.Logf("failed to delete persist PVC: %v", err)
				}
			})

			ginkgo.By("create write pod: fio 1GiB sequential write + sha256 checksum")
			_, err := e2ekubectl.RunKubectl(ns, "apply", "-f", testPodPersistWritePath)
			framework.ExpectNoError(err, "deploy persist write pod")
			ginkgo.DeferCleanup(func() {
				if _, err := e2ekubectl.RunKubectl(ns, "delete", "-f", testPodPersistWritePath); err != nil {
					framework.Logf("failed to delete persist write pod: %v", err)
				}
			})

			ginkgo.By("wait for write pod to complete (fio write + sha256)")
			framework.ExpectNoError(
				waitForPodSucceeded(f.ClientSet, ns, persistWritePodName, 10*time.Minute),
				"write pod did not complete",
			)

			ginkgo.By("delete write pod without waiting for termination")
			// Force-delete to race NodeUnpublish against the verify pod's
			// NodePublish — this is the edge case the test is designed to hit.
			zero := int64(0)
			framework.ExpectNoError(
				f.ClientSet.CoreV1().Pods(ns).Delete(
					context.Background(), persistWritePodName,
					metav1.DeleteOptions{GracePeriodSeconds: &zero}),
				"force-delete write pod",
			)

			ginkgo.By("immediately create verify pod on the same PVC")
			_, err = e2ekubectl.RunKubectl(ns, "apply", "-f", testPodPersistVerifyPath)
			framework.ExpectNoError(err, "deploy persist verify pod")
			ginkgo.DeferCleanup(func() {
				if _, err := e2ekubectl.RunKubectl(ns, "delete", "-f", testPodPersistVerifyPath); err != nil {
					framework.Logf("failed to delete persist verify pod: %v", err)
				}
			})

			ginkgo.By("wait for verify pod to succeed (fio read + sha256 match proves data integrity)")
			// Allow extra time: the verify pod may need to wait for NodeUnpublish
			// of the write pod before it can stage its own mount.
			framework.ExpectNoError(
				waitForPodSucceeded(f.ClientSet, ns, persistVerifyPodName, 15*time.Minute),
				"verify pod did not complete — fio read or sha256 mismatch indicates data loss",
			)
		})
	})
})

// waitForPodSucceeded polls until the named pod enters Succeeded phase.
// It returns immediately with an error when the pod enters Failed phase.
func waitForPodSucceeded(c kubernetes.Interface, ns, podName string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			pod, err := c.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				return true, nil
			case corev1.PodFailed:
				return false, fmt.Errorf("pod %q failed", podName)
			default:
				return false, nil
			}
		})
	if err != nil {
		return fmt.Errorf("pod %q did not reach Succeeded within %s: %w", podName, timeout, err)
	}
	return nil
}
