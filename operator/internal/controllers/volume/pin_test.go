// What a storage-node pin on a claim does to an operation moving its volume.
//
// A pin is the user's statement of where a volume lives. The pinned-volume
// controller moves the volume when the pin changes and leaves it where it is
// when the pin is removed; no other operation may move it, and that includes
// moving it as the sibling of a volume on the same NVMe-oF subsystem.

package volume

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// pinnedClaim is a claim pinned to the given storage node, or unpinned when the
// node is empty.
func pinnedClaim(name, node string) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: consumerNamespace},
	}
	if node != "" {
		claim.Annotations = kube.KeySelectedStorageNode.Set(map[string]string{}, node)
	}
	return claim
}

// pinDrivenOperation is the operation the pinned-volume controller raises.
func pinDrivenOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	ops := testOperation()
	ops.Labels = map[string]string{simplyblockv1alpha2.PinnedVolumeLabel: "pv-hash"}
	return ops
}

func TestAPinnedVolumeIsNotMovedToAnotherNode(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api,
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		pinnedClaim("data-0", testSourceID))

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed: the volume is pinned elsewhere",
			ops.Status.Phase, ops.Status.Message)
	}
	if !strings.Contains(ops.Status.Message, "pinned") {
		t.Errorf("message = %q, want it to say the volume is pinned", ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("a migration was created %d times for a pinned volume", api.creates)
	}
	if got := lockOn(t, r); got != "" {
		t.Errorf("the refused operation left the volume locked by %q", got)
	}
}

func TestAMoveToThePinnedNodeRuns(t *testing.T) {
	r := testReconciler(t, idleSubsystem(),
		pinDrivenOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		pinnedClaim("data-0", testTargetID))

	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running: the target is the node the volume is pinned to",
			ops.Status.Phase, ops.Status.Message)
	}
}

func TestAPinDrivenMoveWhosePinWasRemovedLeavesTheVolume(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api,
		pinDrivenOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		pinnedClaim("data-0", ""))

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q (%s), want Aborted: the pin it was raised for is gone",
			ops.Status.Phase, ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("a migration was created %d times after the pin was removed", api.creates)
	}
}

func TestAPinDrivenMoveSupersededByARepinLeavesTheVolume(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api,
		pinDrivenOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		pinnedClaim("data-0", testSourceID))

	for range 3 {
		runPass(t, r)
	}

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q (%s), want Aborted: the volume was repinned to another node",
			ops.Status.Phase, ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("a migration was created %d times toward a node the volume is no longer pinned to",
			api.creates)
	}
}

func TestAPinnedSiblingBlocksTheSubsystemsMove(t *testing.T) {
	const siblingVolume = "77777777-7777-7777-7777-777777777777"

	api := idleSubsystem()
	api.members = []lvol.Volume{
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN},
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, siblingVolume), NQN: testNQN},
	}
	r := testReconciler(t, api,
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		claimedVolume("pvc-"+siblingVolume, "data-1"),
		pinnedClaim("data-0", ""),
		pinnedClaim("data-1", testSourceID))

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed: a sibling on the subsystem is pinned elsewhere",
			ops.Status.Phase, ops.Status.Message)
	}
	if !strings.Contains(ops.Status.Message, "pvc-"+siblingVolume) {
		t.Errorf("message = %q, want it to name the pinned sibling", ops.Status.Message)
	}
	if api.creates != 0 {
		t.Errorf("a migration was created %d times for a subsystem with a pinned member", api.creates)
	}
}

func TestAnUnpinnedVolumeMovesAsBefore(t *testing.T) {
	r := testReconciler(t, idleSubsystem(),
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"),
		pinnedClaim("data-0", ""))

	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running", ops.Status.Phase, ops.Status.Message)
	}
}
