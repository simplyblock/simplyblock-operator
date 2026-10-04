// What a node reports about itself, and what the scheduler can therefore match
// a volume against.
//
// The volume half of this pairing lives in the controller service, which stamps
// a PV's accessible topology with the segments a volume needs. A segment only
// one of the two sides knows about matches nothing: the PV asks for a key no
// CSINode carries, and the pod stays Pending with no error anywhere to say why.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/kube"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// topologyServer is a node service that answers for nodeName against the given
// node object, which is all NodeGetInfo reads.
func topologyServer(t *testing.T, node *corev1.Node) *Server {
	t.Helper()
	driver := csicommon.NewCSIDriver("csi.simplyblock.io", "test", node.Name)
	if driver == nil {
		t.Fatal("build the CSI driver")
	}
	return &Server{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(driver),
		kubeClient:        kfake.NewSimpleClientset(node),
	}
}

// labeledNode is a worker carrying the zone every node has plus whatever else
// the case is about.
func labeledNode(labels map[string]string) *corev1.Node {
	all := map[string]string{csicommon.TopologyKeyZoneStable: "zone-a"}
	for k, v := range labels {
		all[k] = v
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: all}}
}

// Regression: a capable node has to advertise that it is capable.
//
// The controller stamps a PV asking for client-side compression with
// accessible topology naming storage.simplyblock.io/vdo-capable, so the
// scheduler will only place the pod on a node whose CSINode carries that key.
// NodeGetInfo is the only thing that puts a key there, and it published zone,
// region, pool, and storage-node segments while dropping this one. The label
// was on every node and reached no CSINode, so every VDO volume was
// unschedulable and its pod sat Pending until the test timed out — with nothing
// logged anywhere, because nothing had failed.
func TestNodeGetInfoAdvertisesTheVDOCapability(t *testing.T) {
	ns := topologyServer(t, labeledNode(map[string]string{kube.LabelVDOCapable: "true"}))

	segments := ns.buildAccessibleTopology(context.Background())

	if got := segments[kube.LabelVDOCapable]; got != "true" {
		t.Errorf("the node advertises %q for %s, want true; a volume asking for it can match no node",
			got, kube.LabelVDOCapable)
	}
}

// A node that answered no is advertised as a no rather than left silent, so
// that a node whose probe reported false is distinguishable from one whose
// probe has not answered at all.
func TestNodeGetInfoAdvertisesAnIncapableNodeAsIncapable(t *testing.T) {
	ns := topologyServer(t, labeledNode(map[string]string{kube.LabelVDOCapable: vdoCapableFalse}))

	segments := ns.buildAccessibleTopology(context.Background())

	if got, ok := segments[kube.LabelVDOCapable]; !ok || got != vdoCapableFalse {
		t.Errorf("the node advertises %q (present: %t), want false", got, ok)
	}
}

// A node with no such label advertises no such segment. The label is written by
// a probe that may not have run, and inventing a value here would claim an
// answer nobody established.
func TestNodeGetInfoInventsNoCapabilityForAnUnprobedNode(t *testing.T) {
	ns := topologyServer(t, labeledNode(nil))

	segments := ns.buildAccessibleTopology(context.Background())

	if got, ok := segments[kube.LabelVDOCapable]; ok {
		t.Errorf("an unprobed node advertised %s=%q", kube.LabelVDOCapable, got)
	}
}

// The segments a node already advertised are untouched by the one added here.
func TestNodeGetInfoKeepsTheSegmentsItAlreadyPublished(t *testing.T) {
	ns := topologyServer(t, labeledNode(map[string]string{
		kube.LabelVDOCapable:              "true",
		kube.LabelPoolPrefix + "pool-a":   kube.LabelPoolAllowed,
		csicommon.TopologyKeyRegionStable: "region-a",
	}))

	segments := ns.buildAccessibleTopology(context.Background())

	for key, want := range map[string]string{
		csicommon.TopologyKeyZoneStable:   "zone-a",
		csicommon.TopologyKeyRegionStable: "region-a",
		kube.LabelPoolPrefix + "pool-a":   kube.LabelPoolAllowed,
	} {
		if got := segments[key]; got != want {
			t.Errorf("segment %s = %q, want %q", key, got, want)
		}
	}
}
