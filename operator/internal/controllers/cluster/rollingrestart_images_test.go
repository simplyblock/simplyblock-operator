// A rolling restart moves every node it restarts onto the node's desired
// images, can restart only the nodes not on them yet, and stops when a node
// comes back on something else (design-release-upgrade.md §4.3, §5).

package cluster

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	targetSpdkImage  = "quay.io/simplyblock-io/ultra:26.5.0"
	targetProxyImage = "quay.io/simplyblock-io/simplyblock:26.5.0"
	sourceSpdkImage  = "quay.io/simplyblock-io/ultra:26.4.0-beta6.1"
	sourceProxyImage = "quay.io/simplyblock-io/simplyblock:26.4.0-beta6.1"
)

// imageFleet is a rolling fleet whose nodes also report the images they run. A
// restart moves a node onto the images it names, unless the fleet is stubborn,
// which is a control plane that ignores them.
type imageFleet struct {
	*rollingFleet
	spdk, proxy map[string]string
	stubborn    bool
}

func newImageFleet(ids ...string) *imageFleet {
	f := &imageFleet{rollingFleet: newRollingFleet(ids...), spdk: map[string]string{}, proxy: map[string]string{}}
	for _, id := range ids {
		f.spdk[id], f.proxy[id] = sourceSpdkImage, sourceProxyImage
	}
	return f
}

func (f *imageFleet) nodes() []utils.NodeStatusResponse {
	out := f.rollingFleet.nodes()
	for i := range out {
		out[i].SpdkImage, out[i].SpdkProxyImage = f.spdk[out[i].UUID], f.proxy[out[i].UUID]
	}
	return out
}

func imageAPI(f *imageFleet) *fakeControlPlane {
	api := rollingAPI(f.rollingFleet)
	api.storageNodes = func(string) ([]utils.NodeStatusResponse, error) { return f.nodes(), nil }
	api.restartNode = func(_, id string) error {
		f.status[id] = utils.NodeStatusOnline
		return nil
	}
	return api
}

// restartsMoveNodes makes the fleet apply the images each restart names, which
// the fake can only see after RestartNode recorded them.
func restartsMoveNodes(f *imageFleet, api *fakeControlPlane) {
	api.restartNode = func(_, id string) error {
		f.status[id] = utils.NodeStatusOnline
		if f.stubborn {
			return nil
		}
		images := api.restartImages[len(api.restartImages)-1]
		if images.SpdkImage != "" {
			f.spdk[id] = images.SpdkImage
		}
		if images.SpdkProxyImage != "" {
			f.proxy[id] = images.SpdkProxyImage
		}
		return nil
	}
}

// aStorageNode is the StorageNode object backing a control-plane node, with the
// images its reconciler resolved and whether it runs them.
func aStorageNode(name, uuid string, current *bool) *simplyblockv1alpha2.StorageNode {
	node := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: testClusterName},
	}
	node.Status.UUID = uuid
	node.Status.DesiredSpdkImage = targetSpdkImage
	node.Status.DesiredSpdkProxyImage = targetProxyImage
	node.Status.ImagesCurrent = current
	return node
}

func rollingOps(selection simplyblockv1alpha2.RollingRestartSelection) *simplyblockv1alpha2.StorageClusterOps {
	return newTestOps(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		func(o *simplyblockv1alpha2.StorageClusterOps) {
			o.Spec.RollingRestart = &simplyblockv1alpha2.RollingRestartSpec{Selection: selection}
		})
}

func objectsWith(ops *simplyblockv1alpha2.StorageClusterOps, nodes ...client.Object) []client.Object {
	return append([]client.Object{newTestCluster(), ops}, nodes...)
}

func TestEveryRestartOfTheWalkCarriesTheNodesDesiredImages(t *testing.T) {
	fleet := newImageFleet(nodeA, nodeB)
	api := imageAPI(fleet)
	restartsMoveNodes(fleet, api)
	r := newOpsReconciler(t, api, &recorder{}, objectsWith(rollingOps(""),
		aStorageNode("node-a", nodeA, ptr.To(false)), aStorageNode("node-b", nodeB, ptr.To(false)))...)

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	want := []NodeImages{{targetSpdkImage, targetProxyImage}, {targetSpdkImage, targetProxyImage}}
	if diff := cmp.Diff(want, api.restartImages); diff != "" {
		t.Errorf("the restarts asked for the wrong images (-want +got):\n%s", diff)
	}
}

func TestANodeWithoutAnObjectIsRestartedOnTheImageItRuns(t *testing.T) {
	fleet := newImageFleet(nodeA)
	api := imageAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{}, objectsWith(rollingOps(""))...)

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if diff := cmp.Diff([]NodeImages{{}}, api.restartImages); diff != "" {
		t.Errorf("a node nobody desires images for was asked to change (-want +got):\n%s", diff)
	}
}

func TestAnOutOfDateWalkRestartsOnlyTheNodesNotOnTheirImages(t *testing.T) {
	fleet := newImageFleet(nodeA, nodeB)
	fleet.spdk[nodeA], fleet.proxy[nodeA] = targetSpdkImage, targetProxyImage
	api := imageAPI(fleet)
	restartsMoveNodes(fleet, api)
	r := newOpsReconciler(t, api, &recorder{}, objectsWith(
		rollingOps(simplyblockv1alpha2.RollingRestartSelectionOutOfDate),
		aStorageNode("node-a", nodeA, ptr.To(true)), aStorageNode("node-b", nodeB, ptr.To(false)))...)

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if diff := cmp.Diff([]string{nodeB}, ops.Status.RollingRestart.Nodes); diff != "" {
		t.Errorf("the walk planned the wrong nodes (-want +got):\n%s", diff)
	}
	if api.shutdownNodeCalls != 1 || api.restartNodeCalls != 1 {
		t.Errorf("shutdown=%d restart=%d, want one of each, for the out-of-date node only",
			api.shutdownNodeCalls, api.restartNodeCalls)
	}
}

func TestAnOutOfDateWalkOfACurrentFleetRestartsNothing(t *testing.T) {
	fleet := newImageFleet(nodeA, nodeB)
	api := imageAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{}, objectsWith(
		rollingOps(simplyblockv1alpha2.RollingRestartSelectionOutOfDate),
		aStorageNode("node-a", nodeA, ptr.To(true)), aStorageNode("node-b", nodeB, ptr.To(true)))...)

	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if api.shutdownNodeCalls != 0 || api.restartNodeCalls != 0 {
		t.Errorf("shutdown=%d restart=%d, want none", api.shutdownNodeCalls, api.restartNodeCalls)
	}
	if !strings.Contains(ops.Status.Message, "every node runs its desired images") {
		t.Errorf("message = %q, want it to say why nothing was restarted", ops.Status.Message)
	}
}

func TestAnOutOfDateWalkIsRefusedWhenNoNodeReportsItsImages(t *testing.T) {
	fleet := newImageFleet(nodeA, nodeB)
	api := imageAPI(fleet)
	r := newOpsReconciler(t, api, &recorder{}, objectsWith(
		rollingOps(simplyblockv1alpha2.RollingRestartSelectionOutOfDate),
		aStorageNode("node-a", nodeA, nil), aStorageNode("node-b", nodeB, nil))...)

	ops, _ := reconcileOps(t, r, 10)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "use selection All") {
		t.Errorf("message = %q, want it to name the selection that works", ops.Status.Message)
	}
	if api.shutdownNodeCalls != 0 {
		t.Errorf("shutdown=%d, want none", api.shutdownNodeCalls)
	}
}

func TestANodeThatBecameCurrentBeforeTheWalkReachedItIsPassedOver(t *testing.T) {
	fleet := newImageFleet(nodeA, nodeB)
	api := imageAPI(fleet)
	restartsMoveNodes(fleet, api)
	nodeBObject := aStorageNode("node-b", nodeB, ptr.To(false))
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, objectsWith(
		rollingOps(simplyblockv1alpha2.RollingRestartSelectionOutOfDate),
		aStorageNode("node-a", nodeA, ptr.To(false)), nodeBObject)...)

	walkUntilNode(t, r, 1)
	markCurrent(t, r, "node-b")

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message %q)", ops.Status.Phase, ops.Status.Message)
	}
	if diff := cmp.Diff([]string{nodeB}, ops.Status.RollingRestart.Current); diff != "" {
		t.Errorf("the walk's record of current nodes is wrong (-want +got):\n%s", diff)
	}
	if len(ops.Status.RollingRestart.Skipped) != 0 {
		t.Errorf("skipped = %v, want a current node recorded as current rather than skipped",
			ops.Status.RollingRestart.Skipped)
	}
	if api.restartNodeCalls != 1 || !rec.has(NodeCurrent) {
		t.Errorf("restarts=%d NodeCurrent=%v, want only node A restarted and node B announced current",
			api.restartNodeCalls, rec.has(NodeCurrent))
	}
}

func TestANodeThatCameBackOnOtherImagesFailsTheWalk(t *testing.T) {
	fleet := newImageFleet(nodeA, nodeB)
	fleet.stubborn = true
	api := imageAPI(fleet)
	restartsMoveNodes(fleet, api)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, objectsWith(rollingOps(""),
		aStorageNode("node-a", nodeA, ptr.To(false)), aStorageNode("node-b", nodeB, ptr.To(false)))...)

	ops, _ := reconcileOps(t, r, 20)
	if ops.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, sourceSpdkImage) || !strings.Contains(ops.Status.Message, targetSpdkImage) {
		t.Errorf("message = %q, want both the image it came back on and the one expected", ops.Status.Message)
	}
	if api.restartNodeCalls != 1 || !rec.has(ImageMismatch) {
		t.Errorf("restarts=%d ImageMismatch=%v, want the walk stopped on the first node with the event",
			api.restartNodeCalls, rec.has(ImageMismatch))
	}
}

// markCurrent records, as the node's own reconciler would, that a node now runs
// its desired images.
func markCurrent(t *testing.T, r *StorageClusterOpsReconciler, name string) {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.ImagesCurrent = ptr.To(true)
	if err := r.Status().Update(t.Context(), &node); err != nil {
		t.Fatal(err)
	}
}
