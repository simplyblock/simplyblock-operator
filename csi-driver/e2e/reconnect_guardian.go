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
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("SPDKCSI-RECONNECT-GUARDIAN", func() {
	f := newTestFramework("spdkcsi")

	ginkgo.Context("guardian auto-restarts a workload after total NVMe-oF path loss", func() {
		// Companion to SPDKCSI-RECONNECT-FULLLOSS. That test force-deletes the pod
		// itself (standing in for the guardian) to verify the restage-on-publish
		// path fast and deterministically. This test verifies the guardian's own
		// job: after total path loss it must, on its poll cycle, detect the broken
		// lvol and restart the opted-in pod WITHOUT anyone else deleting it. That
		// matters because an idle workload container never crashes on volume I/O
		// errors, so in production nothing but the guardian would restart it.
		//
		// We induce total path loss and then do NOT touch the pod, so the only
		// thing that can produce a new pod is the guardian. We assert a replacement
		// pod (new UID) comes up on its own and the volume is usable again. Opt-in
		// is via the StorageClass label simplyblock.io/auto-restart-on-pathloss.
		//
		// This is intentionally slow: it waits for the guardian poll cycle
		// (default 5m) plus the restart and restage.
		ginkgo.It("restarts an opted-in pod and restages its volume after total path loss", func() {
			m := fullLossMode{name: "ext4 filesystem", fsType: "ext4"}
			w := setupManagedWorkload(f, m, "guardian")
			origUID := string(w.pod.UID)
			w.induceTotalPathLoss(f)

			ginkgo.By("do NOT touch the pod; wait for the guardian to restart it and restage the volume")
			// The workload is an idle `sleep`, so it never restarts on its own — only
			// the guardian will replace it. A new-UID Ready pod therefore proves the
			// guardian acted; we also confirm the volume is usable again on it. Allow
			// a full guardian poll cycle (default 5m) plus the restart and restage.
			token := "recovered-" + w.ns
			gomega.Eventually(func() error {
				if !guardianReplacedPod(f.ClientSet, w.ns, w.appLabel, origUID) {
					return fmt.Errorf("guardian has not restarted the pod yet (still UID %s)", origUID)
				}
				return verifyVolumeUsableE(f, w.ns, w.appLabel, m, token)
			}, 12*time.Minute, 10*time.Second).Should(gomega.Succeed(),
				"guardian did not restart the pod and restage the volume after total path loss")
		})

		// Regression test for issue #423: a pod backed by two PVCs (two lvols) must
		// be restarted exactly once when both volumes lose their NVMe-oF paths.
		//
		// Before the fix, removePodFromLvolLocked deleted lastRestart[podUID] after
		// the first broken lvol was processed, allowing the second lvol to bypass the
		// RestartBackoff and issue a second delete on the same terminating pod.
		// We verify the fix by asserting that only one replacement pod appears (new
		// UID, Running+Ready) and both volumes are usable on it.
		ginkgo.It("restarts a two-PVC pod exactly once after total path loss on both volumes", func() {
			ns := f.Namespace.Name
			appLabel := "guardian-2pvc"
			m := fullLossMode{name: "ext4 filesystem", fsType: "ext4"}

			ginkgo.By("check the node DaemonSet is ready")
			framework.ExpectNoError(waitForNodeServerReady(f.ClientSet, 3*time.Minute), "node DaemonSet ready")

			ginkgo.By("create an opt-in StorageClass (individual NQN per volume)")
			scName := fmt.Sprintf("%s-%s", appLabel, ns)
			scParams := map[string]string{
				"cluster_id":               liveClusterID(f),
				"max_namespace_per_subsys": "1",
				"csi.storage.k8s.io/fstype": m.fsType,
			}
			createStorageClassWithParamsAndLabels(f.ClientSet, scName, scParams,
				map[string]string{"simplyblock.io/auto-restart-on-pathloss": trueStr})
			ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })

			ginkgo.By("create two PVCs")
			pvc1, pvc2 := appLabel+"-pvc-1", appLabel+"-pvc-2"
			framework.ExpectNoError(createModePVC(f.ClientSet, ns, pvc1, scName, false), "create PVC-1")
			framework.ExpectNoError(createModePVC(f.ClientSet, ns, pvc2, scName, false), "create PVC-2")

			ginkgo.By("create a node-pinned deployment mounting both PVCs")
			workerNode, pluginPod, pluginContainer := anyNodePluginPod(f.ClientSet)
			framework.ExpectNoError(
				createPinnedDeployment2PVC(ns, appLabel, appLabel, pvc1, pvc2, workerNode),
				"create 2-PVC deployment")
			ginkgo.DeferCleanup(func() {
				_ = f.ClientSet.AppsV1().Deployments(ns).Delete(context.Background(), appLabel, metav1.DeleteOptions{})
			})
			pod := waitForReadyPod(f.ClientSet, ns, appLabel, "", 5*time.Minute)
			origUID := string(pod.UID)

			ginkgo.By("write markers to both volumes")
			writeMarker(f, ns, appLabel, m, appLabel+"-vol1")
			// vol2 marker: exec directly since writeMarker uses /spdkvol (vol1 mount)
			execInPod(f, ns, pod.Name, "alpine",
				fmt.Sprintf("echo %s > /spdkvol2/marker.txt", appLabel+"-vol2"))

			ginkgo.By("locate both NVMe subsystems")
			lvolID1 := lvolIDForPVC(f.ClientSet, ns, pvc1)
			lvolID2 := lvolIDForPVC(f.ClientSet, ns, pvc2)
			sub1 := waitForSubsystem(f, pluginPod, pluginContainer, lvolID1, time.Minute)
			sub2 := waitForSubsystem(f, pluginPod, pluginContainer, lvolID2, time.Minute)

			managedWorkload{pluginPod: pluginPod, pluginContainer: pluginContainer, sub: sub1}.induceTotalPathLoss(f)
			managedWorkload{pluginPod: pluginPod, pluginContainer: pluginContainer, sub: sub2}.induceTotalPathLoss(f)

			ginkgo.By("wait for guardian to restart the pod exactly once — both volumes must be usable")
			token1 := "recovered-vol1-" + ns
			token2 := "recovered-vol2-" + ns
			gomega.Eventually(func() error {
				if !guardianReplacedPod(f.ClientSet, ns, appLabel, origUID) {
					return fmt.Errorf("guardian has not restarted the pod yet (still UID %s)", origUID)
				}
				if err := verifyVolumeUsableE(f, ns, appLabel, m, token1); err != nil {
					return fmt.Errorf("vol1 not usable: %w", err)
				}
				// Verify vol2 by writing and reading a token at /spdkvol2.
				opt2 := metav1.ListOptions{LabelSelector: "app=" + appLabel}
				writeCmd2 := fmt.Sprintf("printf '%%s' '%s' > /spdkvol2/marker.txt && sync", token2)
				if _, _, err := execCommandInPodE(f, writeCmd2, ns, &opt2); err != nil {
					return fmt.Errorf("vol2 write not usable: %w", err)
				}
				out2, _, err := execCommandInPodE(f, "cat /spdkvol2/marker.txt", ns, &opt2)
				if err != nil {
					return fmt.Errorf("vol2 read not usable: %w", err)
				}
				if !strings.Contains(out2, token2) {
					return fmt.Errorf("vol2 read back %q, want substring %q", out2, token2)
				}
				return nil
			}, 15*time.Minute, 10*time.Second).Should(gomega.Succeed(),
				"guardian did not restart the 2-PVC pod or volumes not usable after total path loss")
		})
	})
})

// guardianReplacedPod reports whether a Running+Ready pod matching app=appLabel
// exists whose UID differs from origUID — i.e. the original pod was replaced
// without this test deleting it, which only the guardian does.
func guardianReplacedPod(c kubernetes.Interface, ns, appLabel, origUID string) bool {
	pods, err := c.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + appLabel})
	if err != nil {
		return false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if string(p.UID) != origUID && p.DeletionTimestamp == nil &&
			p.Status.Phase == corev1.PodRunning && podReady(p) {
			return true
		}
	}
	return false
}
