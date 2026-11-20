// What the pNFS suite checks before it creates anything, and why a cluster
// that cannot host the metadata server fails the suite at once.
//
// An export waits in Pending until a SimplyblockDriver sets spec.pnfs.mds, and
// the metadata server's pod schedules only onto a node labeled kvm-capable=true.
// With either missing, every claim stays unbound and every spec spends its full
// timeout on a pod that was never going to start: GCP e2e run 38026667986 lost
// five minutes per spec that way after the node-hosted server was removed.

package e2e

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

func kvmNode(name, capable string) *corev1.Node {
	labels := map[string]string{}
	if capable != "" {
		labels[kube.LabelKVMCapable] = capable
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func driverObject(name string, pnfs map[string]any) *unstructured.Unstructured {
	spec := map[string]any{}
	if pnfs != nil {
		spec["pnfs"] = pnfs
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": simplyblockDriverGVR.Group + "/" + simplyblockDriverGVR.Version,
		"kind":       "SimplyblockDriver",
		"metadata":   map[string]any{"name": name, "namespace": "simplyblock"},
		"spec":       spec,
	}}
}

func driverClient(objects ...runtime.Object) *dfake.FakeDynamicClient {
	return dfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{simplyblockDriverGVR: "SimplyblockDriverList"}, objects...)
}

func TestPNFSPrerequisitesPassWithAMetadataServerAndAKVMNode(t *testing.T) {
	nodes := kfake.NewSimpleClientset(kvmNode("worker-1", "false"), kvmNode("worker-2", "true"))
	drivers := driverClient(driverObject("simplyblock", map[string]any{"mds": map[string]any{}}))

	if err := pnfsPrerequisites(nodes, drivers); err != nil {
		t.Errorf("pnfsPrerequisites = %v, want nil", err)
	}
}

// The configuration the GCP workflows still wrote after the node-hosted server
// was removed: the old field, which the API server drops, and no MDS.
func TestPNFSPrerequisitesFailWithoutSpecPNFSMDS(t *testing.T) {
	nodes := kfake.NewSimpleClientset(kvmNode("worker-1", "true"))
	drivers := driverClient(driverObject("simplyblock", map[string]any{"enablePNFS": true}))

	err := pnfsPrerequisites(nodes, drivers)
	if err == nil || !strings.Contains(err.Error(), "spec.pnfs.mds") {
		t.Errorf("pnfsPrerequisites = %v, want an error naming spec.pnfs.mds", err)
	}
}

// Every node answered no, which is what a VM without nested virtualization is.
// The error names each node's answer, so the reader can tell a cluster without
// KVM from a probe that never ran.
func TestPNFSPrerequisitesFailWithoutAKVMCapableNode(t *testing.T) {
	nodes := kfake.NewSimpleClientset(kvmNode("worker-1", "false"), kvmNode("worker-2", ""))
	drivers := driverClient(driverObject("simplyblock", map[string]any{"mds": map[string]any{}}))

	err := pnfsPrerequisites(nodes, drivers)
	if err == nil {
		t.Fatal("pnfsPrerequisites = nil, want an error naming the kvm-capable label")
	}
	for _, want := range []string{kube.LabelKVMCapable, `worker-1="false"`, `worker-2=""`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// Both missing at once is reported as both, so one run says everything that
// has to change.
func TestPNFSPrerequisitesReportBothProblemsTogether(t *testing.T) {
	nodes := kfake.NewSimpleClientset(kvmNode("worker-1", "false"))
	drivers := driverClient()

	err := pnfsPrerequisites(nodes, drivers)
	if err == nil {
		t.Fatal("pnfsPrerequisites = nil, want an error")
	}
	for _, want := range []string{"spec.pnfs.mds", kube.LabelKVMCapable} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
