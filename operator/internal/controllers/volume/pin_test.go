// What a storage-node pin on a claim does to an operation moving its volume.
//
// A pin is the user's statement of where a volume lives. The pinned-volume
// controller moves the volume when the pin changes and leaves it where it is
// when the pin is removed; no other operation may move it, and that includes
// moving it as the sibling of a volume on the same NVMe-oF subsystem.

package volume

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

// Regression: 2026-10-07-pvops-pinned-volume-moved — an operation moved a
// volume pinned to another node. It fails at Pending, takes no lock, and
// creates no migration.
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

// The move the pinned-volume controller raises after a repin targets the new
// pin, and runs.
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

// Regression: 2026-10-07-pvops-pinned-volume-moved — a pin-driven move whose
// pin was removed before it started still moved the volume. An unpin leaves the
// volume where it is.
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

// Regression: 2026-10-07-pvops-pinned-volume-moved — a pin-driven move whose
// pin changed again before it started moved the volume toward the old pin.
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

// Regression: 2026-10-07-pvops-pinned-volume-moved — the control plane moves
// the whole subsystem, so a sibling pinned to another node moved with the
// named volume.
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

// A volume whose claim carries no pin moves as before.
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

// Regression: 2026-10-07-pvops-late-member-pin — the pins were read only while
// the operation was Pending, but the control plane lets a volume join the
// subsystem until the migration is activated, so a sibling that joined later
// pinned to another node moved with the subsystem at cutover.
func TestASiblingPinnedElsewhereThatJoinedLateFailsTheOperationBeforeTheCopy(t *testing.T) {
	const siblingVolume = "77777777-7777-7777-7777-777777777777"

	api := idleSubsystem()
	r := testReconciler(t, api,
		testOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"), pinnedClaim("data-0", ""),
		claimedVolume("pvc-"+siblingVolume, "data-1"), pinnedClaim("data-1", testSourceID))

	runPass(t, r)
	api.members = []lvol.Volume{
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID), NQN: testNQN},
		{ID: lvol.NewVolumeHandle(testClusterID, testPoolID, siblingVolume), NQN: testNQN},
	}
	for range 4 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, step = %q (%s), want Failed: a member pinned elsewhere joined the subsystem",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.continues != 0 {
		t.Errorf("the copy was started %d times for a subsystem with a member pinned elsewhere", api.continues)
	}
	if api.cancels == 0 {
		t.Error("the migration was not taken back")
	}
}

// Regression: 2026-10-07-pvops-pin-from-reused-claim-name — a retained
// volume's claimRef can name a claim that was deleted, and a new, unrelated
// claim can reuse the name. That claim's pin is not this volume's.
func TestAPinOnAClaimThatOnlySharesTheNameDoesNotHoldTheVolume(t *testing.T) {
	pv := claimedVolume(testPVName, "data-0")
	pv.Spec.ClaimRef.UID = "uid-deleted-claim"
	claim := pinnedClaim("data-0", testSourceID)
	claim.UID = "uid-new-claim"
	r := testReconciler(t, idleSubsystem(), testOperation(), testClusterObject(), testNodeObject(), pv, claim)

	runPass(t, r)

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running: the pinned claim is not the one the volume was bound to",
			ops.Status.Phase, ops.Status.Message)
	}
}

// A pin removed while the pin-driven move validates withdraws the move: the
// migration is taken back before the copy, and the volume stays where it is.
func TestAPinRemovedWhileTheMoveValidatesWithdrawsIt(t *testing.T) {
	api := idleSubsystem()
	r := testReconciler(t, api,
		pinDrivenOperation(), testClusterObject(), testNodeObject(),
		claimedVolume(testPVName, "data-0"), pinnedClaim("data-0", testTargetID))

	runPass(t, r)
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: consumerNamespace, Name: "data-0"}, &claim); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	claim.Annotations = nil
	if err := r.Update(context.Background(), &claim); err != nil {
		t.Fatalf("remove the pin: %v", err)
	}
	for range 4 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q, step = %q (%s), want Aborted: the pin was removed before the copy",
			ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
	if api.continues != 0 {
		t.Errorf("the copy was started %d times after the pin was removed", api.continues)
	}
}
