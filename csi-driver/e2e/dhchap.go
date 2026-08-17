/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var _ = ginkgo.Describe("SPDKCSI-DHCHAP", func() {
	f := newTestFramework("spdkcsi")

	// A pool with DHCHAP enabled only authorizes the host NQNs explicitly added
	// to its allowed_hosts list. This exercises the whole chain end-to-end: the
	// node plugin must present that exact NQN (and, once the control plane wires
	// per-connection secrets, the matching DHCHAP key) on the real `nvme connect`
	// it runs — not just on the /connect API call it uses to fetch connection
	// info. A pool/host mismatch here reproduces the bug this test exists to
	// catch: the connect silently used the wrong identity and the backend
	// rejected it, so DHCHAP-gated volumes never mounted at all.
	ginkgo.Context("a pool with DHCHAP allowed_hosts", func() {
		ginkgo.It("mounts only for the allowed host, rejects others, and reconnects after a path drop", func() {
			ns := f.Namespace.Name
			const (
				pvcName  = "dhchap-pvc"
				depName  = "dhchap-test"
				appLabel = "dhchap-test"
			)

			ginkgo.By("check driver components are running")
			framework.ExpectNoError(waitForControllerReady(f.ClientSet, 4*time.Minute), "controller ready")
			framework.ExpectNoError(waitForNodeServerReady(f.ClientSet, 3*time.Minute), "node DaemonSet ready")

			ginkgo.By("pick a worker node to allow, computing its NQN the same way the operator/CSI do")
			workerNode, _, _ := anyNodePluginPod(f.ClientSet)
			allowedHostNQN := hostNQNForNode(f.ClientSet, workerNode)
			clusterID := liveClusterID(f)

			ginkgo.By("create a DHCHAP-enabled pool on the live cluster, allowing only that node's NQN")
			poolName := "e2e-dhchap-" + ns
			sbctl(f, fmt.Sprintf("pool add %s %s --dhchap", poolName, clusterID))
			poolID := sbctlPoolIDByName(f, poolName)
			gomega.Expect(poolID).NotTo(gomega.BeEmpty(), "pool %q not found in sbctl pool list", poolName)
			// Registered last (LIFO), so this runs only after the StorageClass/PVC/
			// Deployment below are already gone — but the backend's async volume
			// deletion triggered by the PVC's reclaim can still lag behind that,
			// so retry until the pool is actually empty rather than failing once.
			ginkgo.DeferCleanup(func() {
				gomega.Eventually(func() error {
					err := sbctlE(f, "pool delete "+poolID)
					return err
				}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed(),
					"pool %s should become empty and deletable once its PVC is reclaimed", poolID)
			})
			sbctl(f, fmt.Sprintf("pool add-host %s %s", poolID, allowedHostNQN))

			ginkgo.By("create a StorageClass pinned to that pool and a PVC")
			scName := "dhchap-" + ns
			// max_namespace_per_subsys=1 gives this volume its own NVMe-oF
			// subsystem, so its NQN (and the PV's "model" attribute) carry this
			// volume's own lvol id rather than a shared subsystem's master lvol
			// id — see the same rationale in setupManagedWorkload.
			createStorageClassWithParams(f.ClientSet, scName, map[string]string{
				scParamClusterID:             clusterID,
				"pool_name":                  poolName,
				scParamMaxNamespacePerSubsys: "1",
			})
			ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })
			framework.ExpectNoError(createPVC(f.ClientSet, ns, pvcName, scName, 1<<30), "create DHCHAP PVC")
			ginkgo.DeferCleanup(func() {
				framework.ExpectNoError(
					f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
						Delete(context.Background(), pvcName, metav1.DeleteOptions{}),
				)
			})

			ginkgo.By("pin a pod to the allowed node and wait for it to mount the volume")
			framework.ExpectNoError(
				createPinnedDeployment(f.ClientSet, ns, depName, appLabel, pvcName, workerNode, false),
				"create pod pinned to the allowed node",
			)
			ginkgo.DeferCleanup(func() {
				framework.ExpectNoError(
					f.ClientSet.AppsV1().Deployments(ns).
						Delete(context.Background(), depName, metav1.DeleteOptions{}),
				)
			})
			waitForReadyPod(f.ClientSet, ns, appLabel, "", 5*time.Minute)

			ginkgo.By("verify the volume is writable — proves DHCHAP auth actually succeeded, not just scheduling")
			podLabel := metav1.ListOptions{LabelSelector: "app=" + appLabel}
			const marker = "dhchap-e2e-ok"
			const markerPath = "/spdkvol/proof"
			writeDataToPod(f, ns, &podLabel, marker, markerPath)

			ginkgo.By("verify an unauthorized host is genuinely rejected by the backend authorization gate")
			// The K8s scheduler would normally keep a pod off a node the pool
			// doesn't allow (via the operator's AllowedTopologies), but that's a
			// separate gate from the one this bug was about. Call /connect
			// directly with an unregistered host NQN — the same request path the
			// CSI driver takes — to confirm the backend itself still refuses it
			// independent of anything Kubernetes-side.
			lvolID := lvolIDForPVC(f.ClientSet, ns, pvcName)
			pluginPod, pluginContainer := nodePluginPodOnNode(f.ClientSet, workerNode)
			unauthorizedNQN := "nqn.2014-08.io.simplyblock:uuid:00000000-0000-0000-0000-000000000000"
			status, body := connectAsHost(f, pluginPod, pluginContainer, clusterID, poolID, lvolID, unauthorizedNQN)
			gomega.Expect(status).To(gomega.Equal(404),
				"connect with an unauthorized host NQN should be rejected, got %d: %s", status, body)
			gomega.Expect(body).To(gomega.ContainSubstring("not found in allowed hosts"),
				"rejection reason should name the allowed-hosts gate, got: %s", body)

			ginkgo.By("drop one NVMe path and confirm the guardian reconnects using the authorized identity")
			sub := waitForSubsystem(f, pluginPod, pluginContainer, lvolID)
			origLive := liveControllers(sub)
			framework.Logf("DHCHAP lvol %s subsystem %s has live paths: %v", lvolID, sub.NQN, origLive)
			if len(origLive) < 1 {
				ginkgo.Skip("volume has no live paths; cannot exercise reconnect")
			}
			victim := origLive[len(origLive)-1]
			execInPod(f, driverNamespace(), pluginPod, pluginContainer,
				fmt.Sprintf("echo 1 > /sys/class/nvme/%s/delete_controller", victim))

			ginkgo.By("wait for the node plugin to reconnect the dropped path")
			gomega.Eventually(func() int {
				s := subsystemForLvol(listSubsystems(f, pluginPod, pluginContainer), lvolID)
				if s == nil {
					return 0
				}
				return len(liveControllers(s))
			}, 2*time.Minute, 3*time.Second).Should(gomega.BeNumerically(">=", len(origLive)),
				"guardian should reconnect the DHCHAP-gated volume's dropped path using the same host identity")

			ginkgo.By("verify the data written before the disruption survived the reconnect")
			compareDataInPod(f, ns, &podLabel, []string{marker}, []string{markerPath})
		})
	})

	// #403: CreateVolume never populated AccessibleTopology for StorageClasses
	// that select their cluster directly via cluster_id — the common case, and
	// the one a DHCHAP-gated pool's StorageClass uses. Without it,
	// external-provisioner never set PersistentVolume.spec.nodeAffinity, so a
	// pod consuming an already-bound PVC could be deleted and recreated onto
	// any node — even one outside the pool's allowed nodes — with no drain or
	// failure needed, a plain restart was enough. The backend's DHCHAP gate
	// (tested above) still rejects the connection from the wrong node, so the
	// bug surfaced as the pod's volume simply never mounting again.
	ginkgo.Context("a DHCHAP pool's allowed-node topology gating", func() {
		ginkgo.It("keeps a recreated pod pinned to the pool's allowed node via PV nodeAffinity", func() {
			ns := f.Namespace.Name
			const (
				pvcName  = "dhchap-sched-pvc"
				depName  = "dhchap-sched-test"
				appLabel = "dhchap-sched-test"
			)

			ginkgo.By("check driver components are running")
			framework.ExpectNoError(waitForControllerReady(f.ClientSet, 4*time.Minute), "controller ready")
			framework.ExpectNoError(waitForNodeServerReady(f.ClientSet, 3*time.Minute), "node DaemonSet ready")

			ginkgo.By("pick a worker node and label it the way the operator labels a DHCHAP pool's allowed nodes")
			workerNode, _, _ := anyNodePluginPod(f.ClientSet)
			allowedHostNQN := hostNQNForNode(f.ClientSet, workerNode)
			clusterID := liveClusterID(f)
			// Real key format: simplyblock.io/pool.<namespace>.<clusterName>.<poolName>
			// (poolNodeLabelKey in simplyblockstoragepool_controller.go). Only the
			// prefix and "allowed" value matter to nodeserver.buildAccessibleTopology
			// and dhchapAllowedNodeSegment — the rest is opaque, so a synthetic name
			// scoped to this spec's namespace is fine.
			nodeLabelKey := fmt.Sprintf("simplyblock.io/pool.%s.e2e-cluster.e2e-sched-pool", ns)
			setNodeLabel(f.ClientSet, workerNode, nodeLabelKey, "allowed")
			ginkgo.DeferCleanup(func() { removeNodeLabel(f.ClientSet, workerNode, nodeLabelKey) })

			ginkgo.By("create a DHCHAP-enabled pool on the live cluster, allowing only that node's NQN")
			poolName := "e2e-dhchap-sched-" + ns
			sbctl(f, fmt.Sprintf("pool add %s %s --dhchap", poolName, clusterID))
			poolID := sbctlPoolIDByName(f, poolName)
			gomega.Expect(poolID).NotTo(gomega.BeEmpty(), "pool %q not found in sbctl pool list", poolName)
			ginkgo.DeferCleanup(func() {
				gomega.Eventually(func() error {
					err := sbctlE(f, "pool delete "+poolID)
					return err
				}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed(),
					"pool %s should become empty and deletable once its PVC is reclaimed", poolID)
			})
			sbctl(f, fmt.Sprintf("pool add-host %s %s", poolID, allowedHostNQN))

			ginkgo.By("create a StorageClass carrying the exact per-pool label key the operator would write")
			scName := "dhchap-sched-" + ns
			createStorageClassWithParams(f.ClientSet, scName, map[string]string{
				scParamClusterID:             clusterID,
				"pool_name":                  poolName,
				scParamMaxNamespacePerSubsys: "1",
				// Must match paramDHCHAPNodeLabel in pkg/spdk/controllerserver.go —
				// unexported there, so duplicated as a literal here.
				"dhchap_node_label": nodeLabelKey,
			})
			ginkgo.DeferCleanup(func() { deleteStorageClass(f.ClientSet, scName) })
			framework.ExpectNoError(createPVC(f.ClientSet, ns, pvcName, scName, 1<<30), "create PVC")
			ginkgo.DeferCleanup(func() {
				framework.ExpectNoError(
					f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
						Delete(context.Background(), pvcName, metav1.DeleteOptions{}),
				)
			})

			ginkgo.By("pin the first pod to the labeled node so CreateVolume sees its topology")
			framework.ExpectNoError(
				createPinnedDeployment(f.ClientSet, ns, depName, appLabel, pvcName, workerNode, false),
				"create pod pinned to the labeled node",
			)
			ginkgo.DeferCleanup(func() {
				framework.ExpectNoError(
					f.ClientSet.AppsV1().Deployments(ns).
						Delete(context.Background(), depName, metav1.DeleteOptions{}),
				)
			})
			firstPod := waitForReadyPod(f.ClientSet, ns, appLabel, "", 5*time.Minute)

			ginkgo.By("verify the PV's nodeAffinity was pinned to the labeled node, not left empty")
			pv := pvForPVC(f.ClientSet, ns, pvcName)
			gomega.Expect(pvNodeAffinityRequires(pv, nodeLabelKey, "allowed")).To(gomega.BeTrue(),
				"PV %s should require node label %s=allowed, got NodeAffinity: %+v",
				pv.Name, nodeLabelKey, pv.Spec.NodeAffinity)

			ginkgo.By("drop this test's own placement pin so recreation relies solely on the PV's nodeAffinity")
			clearDeploymentNodeAffinity(f.ClientSet, ns, depName)

			ginkgo.By("delete the pod and let the Deployment recreate it with no pinning of our own")
			framework.ExpectNoError(
				f.ClientSet.CoreV1().Pods(ns).Delete(context.Background(), firstPod.Name, metav1.DeleteOptions{}),
				"delete pod %s to force recreation", firstPod.Name,
			)
			secondPod := waitForReadyPod(f.ClientSet, ns, appLabel, string(firstPod.UID), 3*time.Minute)

			ginkgo.By("verify the recreated pod still landed on the allowed node, via PV nodeAffinity alone")
			gomega.Expect(secondPod.Spec.NodeName).To(gomega.Equal(workerNode),
				"recreated pod should stay on the pool's allowed node %s (enforced by PV nodeAffinity), "+
					"landed on %q instead — a pod could reschedule off an allowed-nodes pool with no drain "+
					"or failure, just a restart",
				workerNode, secondPod.Spec.NodeName)

			ginkgo.By("verify the volume is still usable after recreation")
			podLabel := metav1.ListOptions{LabelSelector: "app=" + appLabel}
			writeDataToPod(f, ns, &podLabel, "dhchap-sched-ok", "/spdkvol/proof")
		})
	})
})

// hostNQNForNode computes the host NQN the operator and CSI node plugin
// derive for nodeName — nqn.2014-08.io.simplyblock:uuid:<node.UID> — so tests
// can register exactly the identity the real connect path will present.
func hostNQNForNode(c kubernetes.Interface, nodeName string) string {
	node, err := c.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	framework.ExpectNoError(err, "get node %s", nodeName)
	return fmt.Sprintf("nqn.2014-08.io.simplyblock:uuid:%s", node.UID)
}

// sbctlE runs `sbctl <args>` inside the webappapi pod like sbctl, but returns
// the exec error instead of failing the test — for callers (e.g. a retried
// "pool delete" that can legitimately fail while the pool's last volume is
// still being reclaimed) that need to handle failure themselves.
func sbctlE(f *framework.Framework, args string) error {
	ns, pod, container := webappAPIPod(f.ClientSet)
	_, stderr, err := e2epod.ExecWithOptions(f, e2epod.ExecOptions{
		Command:            []string{shPath, "-c", "sbctl " + args},
		PodName:            pod,
		Namespace:          ns,
		ContainerName:      container,
		CaptureStdout:      true,
		CaptureStderr:      true,
		PreserveWhitespace: true,
	})
	if err != nil {
		return fmt.Errorf("%w: %s", err, stderr)
	}
	return nil
}

// sbctlPoolIDByName resolves a simplyblock pool's UUID from its name via
// `sbctl pool list --json`. Returns "" if no pool with that name exists.
func sbctlPoolIDByName(f *framework.Framework, name string) string {
	out := sbctl(f, "pool list --json")
	if i := strings.IndexByte(out, '['); i > 0 {
		out = out[i:]
	}
	var pools []struct {
		UUID string `json:"UUID"`
		Name string `json:"Name"`
	}
	if err := json.Unmarshal([]byte(out), &pools); err != nil {
		framework.Failf("parse sbctl pool list --json %q: %v", out, err)
	}
	for _, p := range pools {
		if p.Name == name {
			return p.UUID
		}
	}
	return ""
}

// connectAsHost calls the control plane's GET .../connect?host_nqn=hostNQN
// directly from inside a csi-node pod, using that pod's own mounted
// credentials (the exact request path the CSI driver itself takes — see
// getLvolConnections in pkg/util/jsonrpc.go). This lets a test probe the
// backend's authorization decision for an arbitrary host NQN without going
// through the Go CSI client or the Kubernetes scheduler. Returns the HTTP
// status code and response body.
func connectAsHost(
	f *framework.Framework,
	pluginPod, pluginContainer, clusterID, poolID, lvolID, hostNQN string,
) (int, string) {
	env := fmt.Sprintf(
		`CLUSTER_ID=%q POOL_ID=%q LVOL_ID=%q HOST_NQN=%q`,
		clusterID, poolID, lvolID, hostNQN,
	)
	script := env + ` python3 - <<'PYEOF'
import json
import os
import urllib.error
import urllib.parse
import urllib.request

with open("/etc/spdkcsi-secret/secret.json") as fh:
    secret = json.load(fh)
cluster = next(c for c in secret["clusters"] if c["cluster_id"] == os.environ["CLUSTER_ID"])

credential = cluster["cluster_secret"]
token_path = os.environ.get("SPDKCSI_API_TOKEN_PATH")
if token_path:
    try:
        with open(token_path) as tf:
            tok = tf.read().strip()
        if tok:
            credential = tok
    except OSError:
        pass

query = urllib.parse.urlencode({"host_nqn": os.environ["HOST_NQN"]})
url = (
    cluster["cluster_endpoint"].rstrip("/")
    + "/api/v2/clusters/" + os.environ["CLUSTER_ID"]
    + "/storage-pools/" + os.environ["POOL_ID"]
    + "/volumes/" + os.environ["LVOL_ID"]
    + "/connect?" + query
)
req = urllib.request.Request(url, headers={"Authorization": "Bearer " + credential})
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(resp.status)
        print(resp.read().decode())
except urllib.error.HTTPError as e:
    print(e.code)
    print(e.read().decode())
PYEOF`
	out := execInPod(f, driverNamespace(), pluginPod, pluginContainer, script)
	lines := strings.SplitN(strings.TrimLeft(out, "\n"), "\n", 2)
	status, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	framework.ExpectNoError(err, "parse HTTP status from connectAsHost output %q", out)
	body := ""
	if len(lines) > 1 {
		body = lines[1]
	}
	return status, body
}

// setNodeLabel adds key=value to nodeName's labels via a JSON merge patch —
// the same mechanism (if not the same code) the operator's syncNodeLabels
// uses to mark a DHCHAP pool's allowed nodes.
func setNodeLabel(c kubernetes.Interface, nodeName, key, value string) {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, key, value))
	_, err := c.CoreV1().Nodes().Patch(
		context.Background(), nodeName, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	framework.ExpectNoError(err, "label node %s with %s=%s", nodeName, key, value)
}

// removeNodeLabel removes key from nodeName's labels via a JSON merge patch
// (a null value deletes the key). Best-effort: logs but does not fail the
// test, consistent with other cleanup helpers in this package.
func removeNodeLabel(c kubernetes.Interface, nodeName, key string) {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, key))
	if _, err := c.CoreV1().Nodes().Patch(
		context.Background(), nodeName, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		framework.Logf("failed to remove label %s from node %s: %v", key, nodeName, err)
	}
}

// pvForPVC resolves the bound PersistentVolume for a PVC.
func pvForPVC(c kubernetes.Interface, ns, pvcName string) *corev1.PersistentVolume {
	pvc, err := c.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), pvcName, metav1.GetOptions{})
	framework.ExpectNoError(err, "get PVC %s/%s", ns, pvcName)
	gomega.Expect(pvc.Spec.VolumeName).NotTo(gomega.BeEmpty(), "PVC %s not bound", pvcName)

	pv, err := c.CoreV1().PersistentVolumes().Get(context.Background(), pvc.Spec.VolumeName, metav1.GetOptions{})
	framework.ExpectNoError(err, "get PV %s", pvc.Spec.VolumeName)
	return pv
}

// pvNodeAffinityRequires reports whether pv.Spec.NodeAffinity has a required
// term matching key=value via an "In" match expression — the shape
// external-provisioner writes from a CSI CreateVolumeResponse's
// AccessibleTopology.
func pvNodeAffinityRequires(pv *corev1.PersistentVolume, key, value string) bool {
	na := pv.Spec.NodeAffinity
	if na == nil || na.Required == nil {
		return false
	}
	for _, term := range na.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != key || expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			for _, v := range expr.Values {
				if v == value {
					return true
				}
			}
		}
	}
	return false
}

// clearDeploymentNodeAffinity removes the pod template's affinity via a
// strategic merge patch, triggering a rollout of a replacement pod that
// carries no placement pin of the test's own — so where that pod lands is
// governed solely by the PV's own nodeAffinity (or, pre-fix, by nothing).
func clearDeploymentNodeAffinity(c kubernetes.Interface, ns, name string) {
	patch := []byte(`{"spec":{"template":{"spec":{"affinity":null}}}}`)
	_, err := c.AppsV1().Deployments(ns).Patch(
		context.Background(), name, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	framework.ExpectNoError(err, "clear node affinity from deployment %s/%s", ns, name)
}
