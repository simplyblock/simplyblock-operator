// The spine every action runs on: one step per pass, and the four ways out.
//
// Nothing here blocks. A pass asks whether the current step has finished and
// either requeues or writes the next step down, so a drain that takes an hour
// costs the controller nothing while it waits. The step is written before the
// side effect it performs, which is what makes a process dying between the two
// safe: every completion condition is a predicate over current state and every
// call is skipped when the node is already where it would put it.
//
// The four ways out are finishing, failing, being aborted where the graph allows
// it, and holding — and the last two are the ones a reader cannot tell from a
// stalled controller without an event, which is what the reasons in events.go
// exist for.
//
// design-storagenode.md §7.

package node

import (
	"context"
	"maps"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// deliveredCluster is the cluster stream's cache, holding one cluster.
type deliveredCluster struct {
	reading subscriptions.ClusterDTO
	synced  bool
}

func (d *deliveredCluster) Lookup(string) (subscriptions.ClusterDTO, bool) {
	return d.reading, d.reading.ID != ""
}

func (d *deliveredCluster) SyncedRoot() bool { return d.synced }

// clusterStatusDegraded is what the control plane reports for a cluster that is
// serving with a node missing, which is what every shutdown produces.
const clusterStatusDegraded = "degraded"

// anAdvancingOperation is an operation at the given step, with a deadline that
// has not passed.
func anAdvancingOperation(
	name string, action simplyblockv1alpha2.StorageNodeOpsAction, at step,
) *simplyblockv1alpha2.StorageNodeOps {
	ops := anOperation(name, action)
	ops.Finalizers = []string{OpsFinalizer}
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
	deadline := metav1.NewTime(time.Now().Add(time.Hour))
	ops.Status.Step = statemachine.KubeSnapshot{State: string(at), Deadline: &deadline}
	return ops
}

// pass runs one reconcile of the operation.
func pass(t *testing.T, r *StorageNodeOpsReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrlRequest(name))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}
}

// The first pass of an admitted operation puts it in its initial step with a
// deadline, and says so. A step with no deadline is a step nothing can ever time
// out, and the initial one is the step a machine is born in — so it is the one
// whose deadline nothing else would set.
func TestTheFirstPassArmsTheStepAMachineIsBornIn(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	ops.Finalizers = []string{OpsFinalizer}
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)

	pass(t, r, "a-suspend")

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Step.State != string(stepRequesting) {
		t.Errorf("step = %q, want the step a suspend starts in", got.Status.Step.State)
	}
	if got.Status.Step.Deadline == nil {
		t.Error("the first step carries no deadline, so it is the one step that cannot time out")
	}
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if !announcedReason(r, OperationStarted) {
		t.Error("nothing announced that the operation had taken the node and started")
	}
}

// A step that has finished moves the operation to the next one, and the next
// step's deadline travels with it.
func TestAFinishedStepEntersTheNextWithItsOwnDeadline(t *testing.T) {
	// A suspend whose node is already suspended finishes Requesting at once.
	api := aControlPlane().reporting(nodeStatusSuspended)
	ops := anAdvancingOperation("a-suspend",
		simplyblockv1alpha2.StorageNodeOpsActionSuspend, stepRequesting)
	r, apiClient := anOpsWorld(t, api, ops)
	lockedBy(t, apiClient, "a-suspend")

	pass(t, r, "a-suspend")

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Step.State != string(stepAwaiting) {
		t.Errorf("step = %q, want the wait that follows the request", got.Status.Step.State)
	}
	if got.Status.Step.Deadline == nil {
		t.Error("the step was entered without a deadline")
	}
}

// The last step finishing is the operation finishing, and the node is released.
func TestTheLastStepFinishingEndsTheOperation(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusSuspended)
	ops := anAdvancingOperation("a-suspend",
		simplyblockv1alpha2.StorageNodeOpsActionSuspend, stepAwaiting)
	r, apiClient := anOpsWorld(t, api, ops)
	lockedBy(t, apiClient, "a-suspend")

	pass(t, r, "a-suspend")

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is still held by %q after the operation finished", holder)
	}
}

// An abort at a step the graph declares abortable stops the operation there, and
// the node is put back into service on the way out: a node past the suspend is
// serving nothing, and leaving it that way takes capacity out of the cluster for
// as long as nobody notices.
func TestAnAbortAtAnAbortableStepStopsAndResumesTheNode(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusSuspended)
	ops := anAdvancingOperation("a-drain",
		simplyblockv1alpha2.StorageNodeOpsActionRemove, stepMigratingVolumes)
	ops.Spec.Abort = true
	r, apiClient := anOpsWorld(t, api, ops)
	r.Mover = &scriptedMover{}
	lockedBy(t, apiClient, "a-drain")

	pass(t, r, "a-drain")

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseAborted {
		t.Errorf("phase = %q, want Aborted", got.Status.Phase)
	}
	if asked := api.asked("Resume"); asked != 1 {
		t.Errorf("Resume was issued %d time(s), want the node put back into service", asked)
	}
	if !announcedReason(r, OperationAborted) {
		t.Error("nothing announced the abort")
	}
}

// An abort at a step the control plane is part-way through is refused, and
// refusing is the point: stopping there would leave nothing driving the node
// back to a state somebody can reason about. The operation carries on and says
// so, which is not a failure of it.
func TestAnAbortThatArrivedTooLateIsRefusedAndTheOperationRunsOn(t *testing.T) {
	api := aControlPlane()
	ops := anAdvancingOperation("a-relocation",
		simplyblockv1alpha2.StorageNodeOpsActionMigrate, stepPromoting)
	ops.Spec.Abort = true
	ops.Spec.Migrate = &simplyblockv1alpha2.MigrateSpec{TargetWorkerNode: opsTarget}
	r, apiClient := anOpsWorld(t, api, ops)
	lockedBy(t, apiClient, "a-relocation")

	pass(t, r, "a-relocation")

	got := operationRead(t, apiClient, "a-relocation")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseRunning {
		t.Errorf("phase = %q, want the operation still running", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("nothing says why the abort was not honored")
	}
	if asked := api.asked("Promote"); asked != 0 {
		t.Errorf("Promote was issued %d time(s) on the pass that answered the abort", asked)
	}
}

// A cluster that is mid-rebalance is a cluster whose layout an operation would
// either be refused by or succeed into inconsistently, so the operation holds
// and says which. It holds in Pending and without the node's lock: the gate is
// the admission check of §7.1, and an operation that has not been admitted has
// nothing to hold.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster — the gate ran
// after the lock was taken, so a held operation kept the node locked while it
// waited, and every operation queued behind it waited with it.
func TestAnOperationHoldsWhileItsClusterIsRebalancing(t *testing.T) {
	ops := anOperation("a-relocation", simplyblockv1alpha2.StorageNodeOpsActionMigrate)
	ops.Finalizers = []string{OpsFinalizer}
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, ops)
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: utils.ClusterStatusActive, Rebalancing: true,
	}}

	pass(t, r, "a-relocation")

	got := operationRead(t, apiClient, "a-relocation")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhasePending {
		t.Errorf("phase = %q, want a held operation still Pending", got.Status.Phase)
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is held by %q while the operation waits on the cluster", holder)
	}
	if !announcedReason(r, ClusterNotReady) {
		t.Error("nothing announced the hold, which is what tells it from a stalled controller")
	}
	if got.Status.Message == "" {
		t.Error("the operation says nothing about what it is waiting for")
	}
}

// Admission is the lock, not the phase. The lock is patched onto the node and
// the phase is written to the operation afterward, and a crash between the two
// restores an operation whose lock already names it and whose phase is still
// Pending. That operation has been admitted: asking the gate again would hold
// the lock it already owns for as long as the cluster stays unready, which is the
// queue behind it waiting on a wait.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster (review) — the
// admission check was keyed off the phase alone.
func TestAnOperationHoldingItsLockIsAdmittedAlready(t *testing.T) {
	ops := anOperation("a-relocation", simplyblockv1alpha2.StorageNodeOpsActionMigrate)
	ops.Finalizers = []string{OpsFinalizer}
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhasePending
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, ops)
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: utils.ClusterStatusActive, Rebalancing: true,
	}}
	lockedBy(t, apiClient, "a-relocation")

	pass(t, r, "a-relocation")

	got := operationRead(t, apiClient, "a-relocation")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseRunning {
		t.Errorf("phase = %q (%s), want an operation that holds its lock running",
			got.Status.Phase, got.Status.Message)
	}
	if announcedReason(r, ClusterNotReady) {
		t.Error("the gate was asked of an operation that already holds the node")
	}
}

// A cluster object that does not exist is a cluster the operation cannot run in,
// and it holds at admission saying so. It is not the same as a node that does
// not exist, which the lock fails the operation on, and reading the one as the
// other admitted the operation into a cluster it could not find.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster (review) — a
// NotFound from the gate was taken for the node's and bypassed the gate.
func TestAnOperationHoldsWhenItsClusterObjectIsMissing(t *testing.T) {
	ops := anOperation("a-relocation", simplyblockv1alpha2.StorageNodeOpsActionMigrate)
	ops.Finalizers = []string{OpsFinalizer}
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, ops)
	if err := apiClient.Delete(context.Background(), anOpsCluster()); err != nil {
		t.Fatalf("removing the cluster: %v", err)
	}

	pass(t, r, "a-relocation")

	got := operationRead(t, apiClient, "a-relocation")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhasePending {
		t.Errorf("phase = %q, want a held operation still Pending", got.Status.Phase)
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is held by %q while its cluster object is missing", holder)
	}
	if !announcedReason(r, ClusterNotReady) {
		t.Error("nothing announced the hold")
	}
	if got.Status.Message == "" {
		t.Error("the operation says nothing about the cluster it cannot find")
	}
}

// A shutdown is what makes its own cluster degraded: the node it took down is
// gone, and the cluster says so until the node is back. The operation finishes
// anyway, because the completion it is waiting on is the node being offline, and
// the cluster's reading is a consequence of that rather than a reason to wait.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster — run
// lblk_outage_matrix_k8s-20260928-054730: one Shutdown against a healthy
// four-node cluster held Running/Awaiting for hours, past its own deadline, with
// the node's lock, and every Restart queued behind it stayed Pending.
func TestAShutdownFinishesInTheClusterItsOwnShutdownDegraded(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	ops := anAdvancingOperation("a-shutdown",
		simplyblockv1alpha2.StorageNodeOpsActionShutdown, stepAwaiting)
	r, apiClient := anOpsWorld(t, api, ops)
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: clusterStatusDegraded,
	}}
	lockedBy(t, apiClient, "a-shutdown")

	pass(t, r, "a-shutdown")

	got := operationRead(t, apiClient, "a-shutdown")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded {
		t.Errorf("phase = %q (%s), want Succeeded against a node already offline",
			got.Status.Phase, got.Status.Message)
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is still held by %q after the shutdown finished", holder)
	}
}

// A restart is how a degraded cluster becomes active again, so it runs against
// one. Gating it on the cluster being active makes it unavailable exactly when
// it is needed.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster — even with the
// lock free, a Restart of the node whose absence degraded the cluster held on
// the cluster gate and never issued the restart.
func TestARestartRunsAgainstADegradedCluster(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	ops := anAdvancingOperation("a-restart",
		simplyblockv1alpha2.StorageNodeOpsActionRestart, stepRequesting)
	r, apiClient := anOpsWorld(t, api, ops)
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: clusterStatusDegraded,
	}}
	lockedBy(t, apiClient, "a-restart")

	pass(t, r, "a-restart")

	if asked := api.asked("RestartNode"); asked != 1 {
		t.Errorf("RestartNode was issued %d time(s) into a degraded cluster, want once", asked)
	}
}

// A step's deadline is reachable whatever the cluster reports. The gate is an
// admission check, and an operation that was admitted and then stalled while its
// cluster was not active is the operation a deadline exists for.
//
// Regression: 2026-09-28-shutdown-holds-lock-in-degraded-cluster — the gate
// returned before the deadline was read, so a held operation could never expire:
// the stuck shutdown was hours past its Step.Deadline and still Running.
func TestTheDeadlineIsReachableWhileTheClusterIsNotActive(t *testing.T) {
	api := aControlPlane()
	ops := anAdvancingOperation("a-relocation",
		simplyblockv1alpha2.StorageNodeOpsActionMigrate, stepAwaitingNode)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step.Deadline = &expired
	r, apiClient := anOpsWorld(t, api, ops)
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: clusterStatusDegraded,
	}}
	lockedBy(t, apiClient, "a-relocation")

	pass(t, r, "a-relocation")

	got := operationRead(t, apiClient, "a-relocation")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Errorf("phase = %q (%s), want Failed past the deadline",
			got.Status.Phase, got.Status.Message)
	}
	if !announcedReason(r, StepDeadlineExceeded) {
		t.Error("nothing announced the expiry, so a failed operation looks like a slow one")
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is still held by %q after the operation failed", holder)
	}
}

// A removal runs whatever the cluster says about itself, because a node is
// removed from an unready cluster precisely to make the cluster ready.
func TestARemovalRunsAgainstARebalancingCluster(t *testing.T) {
	api := aControlPlane()
	ops := anAdvancingOperation("a-drain",
		simplyblockv1alpha2.StorageNodeOpsActionRemove, stepValidating)
	r, apiClient := anOpsWorld(t, api, ops)
	r.Mover = &scriptedMover{}
	r.Clusters = &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: utils.ClusterStatusActive, Rebalancing: true,
	}}
	lockedBy(t, apiClient, "a-drain")

	pass(t, r, "a-drain")

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Step.State != string(stepSuspending) {
		t.Errorf("step = %q, want the drain past validation despite the rebalance",
			got.Status.Step.State)
	}
}

// A step that outlived its deadline fails the operation rather than retrying
// forever, and the node is resumed where the step it failed on left it
// suspended.
func TestAStepThatOutlivedItsDeadlineFailsTheOperation(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusSuspended)
	ops := anAdvancingOperation("a-drain",
		simplyblockv1alpha2.StorageNodeOpsActionRemove, stepMigratingVolumes)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step.Deadline = &expired
	r, apiClient := anOpsWorld(t, api, ops)
	r.Mover = &scriptedMover{}
	lockedBy(t, apiClient, "a-drain")

	pass(t, r, "a-drain")

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if !announcedReason(r, StepDeadlineExceeded) {
		t.Error("nothing announced the expiry, so a failed operation looks like a slow one")
	}
	if asked := api.asked("Resume"); asked != 1 {
		t.Errorf("Resume was issued %d time(s); a drain that failed past the suspend owes it",
			asked)
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is still held by %q after the operation failed", holder)
	}
}

// A step this operator does not recognize is a downgrade, a hand-edited object,
// or a rename that shipped without a conversion. None of them resolves by
// reconciling again, so the operation is terminal with the reason in its status.
func TestAStepThisOperatorCannotResumeEndsTheOperation(t *testing.T) {
	ops := anAdvancingOperation("a-suspend",
		simplyblockv1alpha2.StorageNodeOpsActionSuspend, step("SomethingFromAnotherVersion"))
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "a-suspend")

	pass(t, r, "a-suspend")

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("the record says nothing about why the operation could not be resumed")
	}
}

// Observing the spec is what tells a controller that has not looked from one
// that looked and declined, which on this kind is precisely what a user who set
// spec.abort needs to see.
func TestTheObservedGenerationMovesWhenTheOperationIsLookedAt(t *testing.T) {
	ops := anAdvancingOperation("a-suspend",
		simplyblockv1alpha2.StorageNodeOpsActionSuspend, stepRequesting)
	ops.Generation = 3
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "a-suspend")

	pass(t, r, "a-suspend")

	if got := operationRead(t, apiClient, "a-suspend"); got.Status.ObservedGeneration == 0 {
		t.Error("nothing records that the operation was looked at")
	}
}

// ctrlRequest names one operation of the fixtures' namespace.
func ctrlRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{
		Namespace: opsNamespace, Name: name,
	}}
}

// Regression: 2026-10-02-remove-timeout-read-as-refusal: a removal failed while
// the control plane was removing the node, and the unwind tried to resume it. A
// failure once the removal has been asked for leaves the node to the control
// plane.
func TestAFailureOnceTheRemovalWasAskedForResumesNothing(t *testing.T) {
	for _, current := range []step{stepRemoving, stepAwaitingRemoval} {
		t.Run(string(current), func(t *testing.T) {
			api := aControlPlane().reporting(nodeStatusMigratingDevices)
			ops := anAdvancingOperation("a-drain",
				simplyblockv1alpha2.StorageNodeOpsActionRemove, current)
			expired := metav1.NewTime(time.Now().Add(-time.Minute))
			ops.Status.Step.Deadline = &expired
			r, apiClient := anOpsWorld(t, api, ops)
			r.Mover = &scriptedMover{}
			lockedBy(t, apiClient, "a-drain")

			pass(t, r, "a-drain")

			got := operationRead(t, apiClient, "a-drain")
			if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
				t.Errorf("phase = %q, want Failed", got.Status.Phase)
			}
			if asked := api.asked("Resume"); asked != 0 {
				t.Errorf("Resume was issued %d time(s) against a node being removed, want none",
					asked)
			}
		})
	}
}

// anAwaitingRemoval is a Remove waiting on the control plane's removal, with a
// deadline a minute away and the progress it last recorded.
func anAwaitingRemoval(recorded *simplyblockv1alpha2.RemovalStatus) *simplyblockv1alpha2.StorageNodeOps {
	ops := anAdvancingOperation("a-drain",
		simplyblockv1alpha2.StorageNodeOpsActionRemove, stepAwaitingRemoval)
	soon := metav1.NewTime(time.Now().Add(time.Minute))
	ops.Status.Step.Deadline = &soon
	ops.Status.Removal = recorded
	return ops
}

// aNodeDevice is one of the node's StorageDevices reporting a control-plane
// status.
func aNodeDevice(name, status string) *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: opsNamespace,
			Labels: map[string]string{simplyblockv1alpha2.DeviceLabelNode: opsNodeName},
		},
		Status: simplyblockv1alpha2.StorageDeviceStatus{DeviceStatus: status},
	}
}

// deadlineReachesPast reports whether the step's deadline lies at least the
// step's budget past the given instant, less a minute of margin for the test's own
// running time.
func deadlineReachesPast(t *testing.T, got *simplyblockv1alpha2.StorageNodeOps, from time.Time) bool {
	t.Helper()
	deadline, ok := got.Status.Step.KubeDeadline()
	if !ok {
		t.Fatal("the step carries no deadline")
	}
	return !deadline.Before(from.Add(awaitingRemovalDeadline - time.Minute))
}

// Regression: 2026-10-02-awaiting-removal-progress: a removal moves a node's data
// for as long as the data takes, so one fixed budget either fails a removal that
// is still moving or waits hours on one that stopped. A change of the node's
// status is progress, and progress moves the deadline out by a whole budget.
func TestANodeStatusChangeMovesTheRemovalDeadlineOut(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingLvols)
	ops := anAwaitingRemoval(&simplyblockv1alpha2.RemovalStatus{
		NodeStatus: nodeStatusMigratingDevices,
	})
	r, apiClient := anOpsWorld(t, api, ops)
	lockedBy(t, apiClient, "a-drain")
	before := time.Now()

	pass(t, r, "a-drain")

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Removal == nil || got.Status.Removal.NodeStatus != nodeStatusMigratingLvols {
		t.Errorf("status.removal = %+v, want the node's new status recorded", got.Status.Removal)
	}
	if !deadlineReachesPast(t, got, before) {
		t.Errorf("deadline = %v, want it moved a whole budget out after the node moved on",
			got.Status.Step.Deadline)
	}
}

// Regression: 2026-10-02-awaiting-removal-progress: migrating_devices is one node
// status for the whole rebuild, and what moves during it is the devices, one at a
// time. A device changing status is progress too.
func TestADeviceStatusChangeMovesTheRemovalDeadlineOut(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingDevices)
	ops := anAwaitingRemoval(&simplyblockv1alpha2.RemovalStatus{
		NodeStatus: nodeStatusMigratingDevices,
		Devices:    map[string]string{"dev-a": "failed", "dev-b": "failed"},
	})
	r, apiClient := anOpsWorld(t, api, ops,
		aNodeDevice("dev-a", "failed_and_migrated"), aNodeDevice("dev-b", "failed"))
	lockedBy(t, apiClient, "a-drain")
	before := time.Now()

	pass(t, r, "a-drain")

	got := operationRead(t, apiClient, "a-drain")
	want := map[string]string{"dev-a": "failed_and_migrated", "dev-b": "failed"}
	if got.Status.Removal == nil || !maps.Equal(got.Status.Removal.Devices, want) {
		t.Errorf("status.removal = %+v, want the devices' statuses %v", got.Status.Removal, want)
	}
	if !deadlineReachesPast(t, got, before) {
		t.Errorf("deadline = %v, want it moved a whole budget out after a device moved on",
			got.Status.Step.Deadline)
	}
}

// The other half: a pass that finds nothing changed leaves the deadline where it
// was, which is what makes a stalled removal fail at all.
func TestARemovalThatDidNotMoveKeepsItsDeadline(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusMigratingDevices)
	ops := anAwaitingRemoval(&simplyblockv1alpha2.RemovalStatus{
		NodeStatus: nodeStatusMigratingDevices,
		Devices:    map[string]string{"dev-a": "failed"},
	})
	recorded := *ops.Status.Step.Deadline
	r, apiClient := anOpsWorld(t, api, ops, aNodeDevice("dev-a", "failed"))
	lockedBy(t, apiClient, "a-drain")

	pass(t, r, "a-drain")

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Step.Deadline == nil || got.Status.Step.Deadline.Unix() != recorded.Unix() {
		t.Errorf("deadline = %v, want %v kept for a removal that did not move",
			got.Status.Step.Deadline, recorded)
	}
}
