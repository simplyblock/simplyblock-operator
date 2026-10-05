// What a refused migration create does to the operation.
//
// The control plane answers a create it will never accept with a 400 naming
// the reason, a create it cannot accept yet with a 409, and a create whose
// outcome is unknown with a timeout or a 5xx. Only the last two are worth
// retrying, and a 400 retried until the step's deadline hands whatever raised
// the operation a failure fifteen minutes late and with the reason gone.

package volume

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// refusingCreate is a control plane that answers the create with the given
// status and body.
func refusingCreate(status int, body string) *fakeControlPlane {
	api := idleSubsystem()
	api.createErr = &controlplane.StatusError{Op: "create migration", StatusCode: status, Body: body}
	return api
}

// Regression: 2026-10-05-drain-target-memory — a create the control plane
// refused outright was retried until Validating's deadline, so the drain that
// raised it learned of the refusal fifteen minutes later and without the
// control plane's reason, and raised the next move against the same target.
func TestARefusedCreateFailsTheOperationAtOnce(t *testing.T) {
	const reason = "Cannot migrate to node " + testTargetID + ": it serves as the fallback source"
	api := refusingCreate(400, `{"detail":"`+reason+`"}`)
	r := testReconciler(t, api, testWorld()...)

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed on the refusal", ops.Status.Phase, ops.Status.Message)
	}
	if !strings.Contains(ops.Status.Message, reason) {
		t.Errorf("message = %q, want the control plane's reason in it", ops.Status.Message)
	}
	if api.creates != 1 {
		t.Errorf("the create was sent %d times, want once: a refusal is not retried", api.creates)
	}
}

// A 409 is the control plane saying not yet: another migration of the
// subsystem is active, or a precondition that clears by itself is unmet. It is
// waited on in the step rather than failed.
func TestABusyControlPlaneIsWaitedOnRatherThanFailed(t *testing.T) {
	r := testReconciler(t, refusingCreate(409, `{"detail":"An active migration already exists"}`),
		testWorld()...)

	for range 3 {
		runPass(t, r)
	}

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running while the control plane is busy",
			ops.Status.Phase, ops.Status.Message)
	}
}

// Regression: 2026-10-05-drain-target-memory — the control plane refuses a
// migration onto the node a volume is already on with a 400 that names that
// node. It is the same fact the volume's hosting node states, and it ends the
// operation the same way.
func TestARefusalSayingTheVolumeIsAlreadyThereSucceeds(t *testing.T) {
	api := refusingCreate(400, `{"detail":"LVol `+testVolumeID+` is already on node `+testTargetID+
		`; cannot migrate to the same node"}`)
	r := testReconciler(t, api, testWorld()...)

	for range 3 {
		runPass(t, r)
	}

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Errorf("phase = %q (%s), want Succeeded: the volume is already on the target",
			ops.Status.Phase, ops.Status.Message)
	}
}

// Regression: 2026-10-06-pvops-400-conflict-read-as-final — some control
// planes answer a create that conflicts with an active migration of the
// subsystem with a 400 rather than a 409, in the wording the registered kind's
// client already recognizes. It clears when that migration ends, and failing
// on it handed the drain a failure that was not one.
func TestAConflictAnsweredWithA400IsWaitedOn(t *testing.T) {
	r := testReconciler(t, refusingCreate(400, `{"detail":"An active migration for `+testVolumeID+
		` already exists targeting a different node (`+testSourceID+`). Cancel it first."}`), testWorld()...)

	for range 3 {
		runPass(t, r)
	}

	if ops := operationFrom(t, r); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want Running while another migration of the subsystem is active",
			ops.Status.Phase, ops.Status.Message)
	}
}

// Regression: 2026-10-06-pvops-refusal-names-the-target — the failure message
// put the target node's UUID in front of the control plane's reason, so a drain
// reading it found the target named in every refusal and ruled the target out
// at once, whatever the control plane had actually objected to.
func TestARefusalKeepsTheControlPlanesWordsAndNamesNoTargetOfItsOwn(t *testing.T) {
	const reason = "LVol belongs to a shared NVMe-oF subsystem with 2 member(s). Use --batch to migrate the whole subsystem together."
	r := testReconciler(t, refusingCreate(400, `{"detail":"`+reason+`"}`), testWorld()...)

	for range 3 {
		runPass(t, r)
	}

	ops := operationFrom(t, r)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed on the refusal", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, reason) {
		t.Errorf("message = %q, want the control plane's reason", ops.Status.Message)
	}
	if strings.Contains(ops.Status.Message, testTargetID) {
		t.Errorf("message = %q names the target %s, which the control plane's answer did not",
			ops.Status.Message, testTargetID)
	}
}
