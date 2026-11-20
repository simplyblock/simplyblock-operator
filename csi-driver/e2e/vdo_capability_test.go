// What the VDO suite checks before it starts, and why a cluster that cannot run
// VDO is a skip rather than a failure.
//
// The capability is the node's kernel, which no test can install. A volume
// asking for it is pinned by node affinity to a node advertising it, so on a
// cluster where none does, the pod is not scheduled, nothing fails, and every
// spec spends its full timeout waiting for a pod that was never going to run.
// Five specs did exactly that for five minutes each.

package e2e

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
)

func capabilityNode(name, capable string) *corev1.Node {
	labels := map[string]string{}
	if capable != "" {
		labels[kube.LabelVDOCapable] = capable
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// One capable node is enough: the volume needs somewhere to go, not everywhere.
func TestVDOCapableNodesFindsTheOneCapableNode(t *testing.T) {
	client := kfake.NewSimpleClientset(
		capabilityNode("worker-1", "false"),
		capabilityNode("worker-2", "true"),
		capabilityNode("worker-3", ""),
	)

	capable, err := vdoCapableNodes(client)
	if err != nil {
		t.Fatalf("vdoCapableNodes: %v", err)
	}
	if len(capable) != 1 || capable[0] != "worker-2" {
		t.Errorf("capable nodes = %v, want [worker-2]", capable)
	}
}

// A cluster whose every node answered no. This is what the e2e runners are:
// RHEL 9.4 on kernel 5.14, which carries no in-tree dm-vdo, with no kmod-kvdo
// installed either.
func TestVDOCapableNodesFindsNoneWhenEveryNodeAnsweredNo(t *testing.T) {
	client := kfake.NewSimpleClientset(
		capabilityNode("worker-1", "false"),
		capabilityNode("worker-2", "false"),
	)

	capable, err := vdoCapableNodes(client)
	if err != nil {
		t.Fatalf("vdoCapableNodes: %v", err)
	}
	if len(capable) != 0 {
		t.Errorf("capable nodes = %v, want none", capable)
	}
}

// A cluster where the probe has not answered at all is equally a cluster with
// nowhere to put the volume. It is reported the same way, because the suite
// cannot do anything different about it either way.
func TestVDOCapableNodesFindsNoneWhenNothingProbed(t *testing.T) {
	client := kfake.NewSimpleClientset(capabilityNode("worker-1", ""))

	capable, err := vdoCapableNodes(client)
	if err != nil {
		t.Fatalf("vdoCapableNodes: %v", err)
	}
	if len(capable) != 0 {
		t.Errorf("capable nodes = %v, want none", capable)
	}
}
