// The operations: the lock, the drain, the abort line, and the two steps that
// are more than the call they make.
//
// Preflight and Verifying are the pair worth testing hardest. Preflight is what
// stops an upgrade that has nothing to do, and Verifying is what makes an
// upgrade more than an image bump: without it a rollout that started and failed
// back is a Succeeded operation against a control plane running the old version.

package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// anotherOperation is the name the lock tests use for an operation that is not
// the one under test, so the assertions read as "somebody else's" rather than as
// a string repeated four times.
const anotherOperation = "somebody-elses-operation"

// opsFor builds one operation against the singleton.
func opsFor(action simplyblockv1alpha2.ControlPlaneOpsAction) *simplyblockv1alpha2.ControlPlaneOps {
	return &simplyblockv1alpha2.ControlPlaneOps{
		ObjectMeta: metav1.ObjectMeta{Name: "an-operation", Namespace: testNamespace},
		Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: SingletonName,
			Action:          action,
		},
	}
}

// Every action declares a graph, and every graph is a line. The reconciler takes
// the first edge as the only edge, so a graph that branched would silently pick
// one.
func TestEveryActionsGraphIsALine(t *testing.T) {
	graphs := opsGraphs()

	for _, act := range []simplyblockv1alpha2.ControlPlaneOpsAction{
		simplyblockv1alpha2.ControlPlaneOpsActionRestart,
		simplyblockv1alpha2.ControlPlaneOpsActionUpgrade,
		simplyblockv1alpha2.ControlPlaneOpsActionBackup,
	} {
		graph, declared := graphs[action(act)]
		if !declared {
			t.Fatalf("%s declares no graph, so the action cannot run at all", act)
		}
		terminals := 0
		for step, state := range graph.States {
			switch len(state.To) {
			case 0:
				terminals++
			case 1:
			default:
				t.Errorf("%s: step %s declares %d successors, want one", act, step, len(state.To))
			}
		}
		if terminals != 1 {
			t.Errorf("%s declares %d terminal steps, want exactly one", act, terminals)
		}
		if _, ok := opsInitialDeadlines[action(act)]; !ok {
			t.Errorf("%s has no initial deadline, so its first step cannot time out", act)
		}
	}
}

// Every abortable step is one some graph declares. The table sits beside the
// graphs rather than in them, so a step renamed in one and not the other would
// make an abort silently unreachable.
func TestEveryAbortableStepIsAStepSomeGraphDeclares(t *testing.T) {
	declared := map[string]bool{}
	for _, state := range statemachine.DeclaredMultiStates(opsGraphs()) {
		declared[state] = true
	}
	for step := range abortableSteps {
		if !declared[string(step)] {
			t.Errorf("%s is abortable and no graph declares it", step)
		}
	}
}

// An abort is honored only before anything has been changed. A step that has
// rolled a Deployment or written an image onto the entity carries on, because
// stopping there would leave a rollout half-done with nothing driving it either
// way.
func TestAnAbortIsRefusedOnceARolloutHasStarted(t *testing.T) {
	refused := []opsStep{stepRestarting, stepApplying, stepAwaiting, stepVerifying}
	for _, step := range refused {
		if abortable(step) {
			t.Errorf("%s is abortable, and an abort there leaves a rollout half-done", step)
		}
	}
	for _, step := range []opsStep{stepDraining, stepPreflight, stepRequesting} {
		if !abortable(step) {
			t.Errorf("%s is not abortable, and nothing has been changed at that point", step)
		}
	}
}

// An operation naming an external control plane is refused rather than run. The
// webhook catches it at creation, and this is what holds when the webhook was
// not serving.
func TestAnOperationAgainstAnExternalControlPlaneFails(t *testing.T) {
	cp := externalControlPlane("https://sb-control.example.com:5000")
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	c := newClient(t, cp, ops)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlaneOps
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ops), &after); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Errorf("status.phase = %s, want Failed", after.Status.Phase)
	}
	if !strings.Contains(after.Status.Message, "external") {
		t.Errorf("status.message = %q, want it to say why the target cannot be operated on",
			after.Status.Message)
	}
}

// An operation naming a control plane that does not exist fails with the name it
// could not resolve, rather than retrying against an object that will never
// appear.
func TestAnOperationWithNoTargetFails(t *testing.T) {
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	ops.Spec.ControlPlaneRef = "not-the-singleton"
	c := newClient(t, ops)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlaneOps
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ops), &after); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Errorf("status.phase = %s, want Failed", after.Status.Phase)
	}
	if !strings.Contains(after.Status.Message, "not-the-singleton") {
		t.Errorf("status.message = %q, want it to name what could not be resolved",
			after.Status.Message)
	}
}

// A second operation stays Pending and asks again rather than failing. The other
// operation will finish, and failing here would make the order two people
// applied two objects in decide which of them runs.
func TestASecondOperationWaitsForTheLock(t *testing.T) {
	cp := managedControlPlane()
	cp.Status.ActiveOpsRef = anotherOperation
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	c := newClient(t, cp, ops)
	recorder := &recordingRecorder{}
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t), Recorder: recorder}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("the operation was not requeued, so it would never take the lock when it frees")
	}

	var after simplyblockv1alpha2.ControlPlaneOps
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ops), &after); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	if after.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhasePending {
		t.Errorf("status.phase = %s, want Pending", after.Status.Phase)
	}
	if recorder.count(OperationQueued) == 0 {
		t.Error("no OperationQueued event: nothing says why the operation is not running")
	}

	var target simplyblockv1alpha2.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cp), &target); err != nil {
		t.Fatalf("read the control plane back: %v", err)
	}
	if target.Status.ActiveOpsRef != anotherOperation {
		t.Errorf("activeOpsRef = %q, want the lock left with its holder",
			target.Status.ActiveOpsRef)
	}
}

// Deleting an operation while it holds the lock releases the lock. Without it
// the control plane would be locked against every later operation with nothing
// left in the cluster to say why.
func TestDeletingARunningOperationReleasesTheLock(t *testing.T) {
	ctx := context.Background()
	cp := managedControlPlane()
	cp.Status.ActiveOpsRef = "an-operation"

	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	ops.Finalizers = []string{FinalizerControlPlaneOps}
	now := metav1.Now()
	ops.DeletionTimestamp = &now
	ops.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning

	c := newClient(t, cp, ops)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var target simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &target); err != nil {
		t.Fatalf("read the control plane back: %v", err)
	}
	if target.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released with the operation",
			target.Status.ActiveOpsRef)
	}
}

// The lock is released only by the operation holding it. An operation that never
// acquired it, or whose lock was taken over, must not clear somebody else's.
func TestAnOperationDoesNotReleaseSomebodyElsesLock(t *testing.T) {
	ctx := context.Background()
	cp := managedControlPlane()
	cp.Status.ActiveOpsRef = anotherOperation
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)

	c := newClient(t, cp, ops)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.releaseLock(ctx, ops, cp); err != nil {
		t.Fatalf("releaseLock: %v", err)
	}

	var target simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &target); err != nil {
		t.Fatalf("read the control plane back: %v", err)
	}
	if target.Status.ActiveOpsRef != anotherOperation {
		t.Errorf("activeOpsRef = %q, want somebody else's lock untouched",
			target.Status.ActiveOpsRef)
	}
}

// A scoped restart drains only when it names something depended on. Recycling an
// exporter interrupts nothing, so a restart of it has nothing to wait for; the
// task runner is the case that draws the line, and it skips the drain because
// its queue makes the interruption a delay rather than a lost operation.
func TestARestartDrainsOnlyWhenItNamesSomethingEssential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		components []string
		wantDrain  bool
	}{
		{"the whole control plane", nil, true},
		{"the management API", []string{ComponentWebAPI}, true},
		{"the database", []string{ComponentFDBCluster}, true},
		{"the task runner", []string{ComponentTasks}, false},
		{"the exporter", []string{ComponentFDBExporter}, false},
		{"a non-essential set", []string{ComponentTasks, ComponentMinio}, false},
		{"a set including an essential one", []string{ComponentTasks, ComponentWebAPI}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
			if tc.components != nil {
				ops.Spec.Restart = &simplyblockv1alpha2.RestartSpec{Components: tc.components}
			}

			if got := drainsFirst(ops); got != tc.wantDrain {
				t.Errorf("drainsFirst = %v, want %v", got, tc.wantDrain)
			}
		})
	}
}

// An upgrade always drains, whatever it names, because it rolls the management
// API by definition.
func TestAnUpgradeAlwaysDrains(t *testing.T) {
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	if !drainsFirst(ops) {
		t.Error("an upgrade skipped the drain, and it rolls the management API by definition")
	}
}

// The drain holds while another operation is running, and names what it is
// holding on so somebody reading the object knows what to wait for.
func TestTheDrainHoldsOnOperationsInFlight(t *testing.T) {
	ctx := context.Background()
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	inFlight := &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-node-add", Namespace: testNamespace},
		Status: simplyblockv1alpha2.StorageNodeOpsStatus{
			Phase: simplyblockv1alpha2.StorageNodeOpsPhaseRunning,
		},
	}

	c := newClient(t, ops, inFlight)
	recorder := &recordingRecorder{}
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t), Recorder: recorder}

	done, held, err := r.drain(ctx, ops)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if done {
		t.Fatal("the drain finished while a node operation was still running")
	}
	if !strings.Contains(held, "StorageNodeOps/a-node-add") {
		t.Errorf("held on %q, want it to name the operation in flight", held)
	}
	if recorder.count(OperationsInFlight) == 0 {
		t.Error("no OperationsInFlight event: nothing says why the restart is waiting")
	}
}

// A terminal operation elsewhere does not hold the drain: what is being waited
// for is work in flight, and a finished operation has none.
func TestTheDrainDoesNotHoldOnFinishedOperations(t *testing.T) {
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	finished := &simplyblockv1alpha2.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: "a-finished-add", Namespace: testNamespace},
		Status: simplyblockv1alpha2.StorageNodeOpsStatus{
			Phase: simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded,
		},
	}

	c := newClient(t, ops, finished)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	done, held, err := r.drain(context.Background(), ops)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !done {
		t.Errorf("the drain held on a finished operation: %s", held)
	}
}

// Preflight refuses an upgrade naming the image already running, because rolling
// a Deployment to its current image produces no change to verify.
func TestPreflightRefusesAnUpgradeToTheRunningImage(t *testing.T) {
	cp := managedControlPlane()
	cp.Status.Phase = simplyblockv1alpha2.ControlPlanePhaseAvailable

	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{Image: testImage}

	r := &ControlPlaneOpsReconciler{Client: newClient(t, cp, ops), Scheme: testScheme(t)}

	_, _, err := r.preflight(context.Background(), ops, cp)
	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Fatalf("preflight returned %v, want a terminal failure", err)
	}
	if !strings.Contains(fatal.Error(), testImage) {
		t.Errorf("the refusal is %q, want it to name the image already running", fatal.Error())
	}
}

// Preflight holds rather than fails while the control plane is not Available, so
// that what the upgrade verifies afterward is a change rather than a recovery.
func TestPreflightHoldsWhileTheControlPlaneIsNotAvailable(t *testing.T) {
	cp := managedControlPlane()
	cp.Status.Phase = simplyblockv1alpha2.ControlPlanePhaseUnavailable

	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{
		Image: "quay.io/simplyblock-io/simplyblock:26.3.0",
	}

	r := &ControlPlaneOpsReconciler{Client: newClient(t, cp, ops), Scheme: testScheme(t)}

	done, held, err := r.preflight(context.Background(), ops, cp)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if done {
		t.Fatal("preflight passed against an Unavailable control plane")
	}
	if !strings.Contains(held, string(simplyblockv1alpha2.ControlPlanePhaseUnavailable)) {
		t.Errorf("held on %q, want it to name the phase it is waiting to leave", held)
	}
}

// Applying an upgrade writes the image onto the entity rather than onto the
// Deployment, because the entity re-applies its workloads from its own spec on
// every pass: a patched Deployment would be reconciled back to the old image.
func TestAnUpgradeWritesTheImageOntoTheEntity(t *testing.T) {
	ctx := context.Background()
	const next = "quay.io/simplyblock-io/simplyblock:26.3.0"

	cp := managedControlPlane()
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{Image: next}

	c := newClient(t, cp, ops)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	if _, _, err := r.applyUpgrade(ctx, ops, cp); err != nil {
		t.Fatalf("applyUpgrade: %v", err)
	}

	var after simplyblockv1alpha2.ControlPlane
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), &after); err != nil {
		t.Fatalf("read the control plane back: %v", err)
	}
	if got := managedImage(&after); got != next {
		t.Errorf("spec.source.managed.image = %q, want %q", got, next)
	}
}

// Verifying fails the operation when the reported version disagrees with what
// was asked for, which is what separates an upgrade that completed from a
// rollout that failed back.
func TestVerifyingFailsOnAVersionThatDisagrees(t *testing.T) {
	cp := managedControlPlane()
	cp.Status.Endpoint = "http://simplyblock-webappapi.simplyblock.svc.cluster.local:5000"

	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{
		Image: "quay.io/simplyblock-io/simplyblock:26.3.0",
	}

	recorder := &recordingRecorder{}
	r := &ControlPlaneOpsReconciler{
		Client:   newClient(t, cp, ops),
		Scheme:   testScheme(t),
		Recorder: recorder,
		Prober:   &stubProber{ready: true, version: "26.2.8"},
	}

	_, _, err := r.verify(context.Background(), ops, cp)
	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Fatalf("verify returned %v, want a terminal failure", err)
	}
	if !strings.Contains(fatal.Error(), "26.2.8") {
		t.Errorf("the failure is %q, want it to name what the control plane reported",
			fatal.Error())
	}
	if recorder.count(VersionMismatch) == 0 {
		t.Error("no VersionMismatch event on a rollout that failed back")
	}
}

// Verifying passes when the reported version is the one asked for.
func TestVerifyingPassesOnTheVersionThatWasAskedFor(t *testing.T) {
	cp := managedControlPlane()
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{
		Image: "quay.io/simplyblock-io/simplyblock:26.3.0",
	}

	r := &ControlPlaneOpsReconciler{
		Client: newClient(t, cp, ops),
		Scheme: testScheme(t),
		Prober: &stubProber{ready: true, version: "26.3.0"},
	}

	done, held, err := r.verify(context.Background(), ops, cp)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !done {
		t.Errorf("verify held on %q against the version it asked for", held)
	}
}

// A control plane that serves no version endpoint passes verification rather
// than failing it, and says so: failing every upgrade on a deployment that
// cannot answer would make the action unusable, and the record of the operation
// has to carry what was and was not verified.
func TestVerifyingPassesAndSaysSoWhenNoVersionIsServed(t *testing.T) {
	ctx := context.Background()
	cp := managedControlPlane()
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{
		Image: "quay.io/simplyblock-io/simplyblock:26.3.0",
	}

	c := newClient(t, cp, ops)
	r := &ControlPlaneOpsReconciler{
		Client: c, Scheme: testScheme(t),
		Prober: &stubProber{ready: true, version: ""},
	}

	done, _, err := r.verify(ctx, ops, cp)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !done {
		t.Fatal("verify held on a control plane that serves no version endpoint")
	}

	var after simplyblockv1alpha2.ControlPlaneOps
	if err := c.Get(ctx, client.ObjectKeyFromObject(ops), &after); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	if !strings.Contains(after.Status.Message, "not verified") {
		t.Errorf("status.message = %q, want it to record that the version was not verified",
			after.Status.Message)
	}
}

// A digest-pinned image carries no version to compare against, so the comparison
// is skipped rather than failed: what is pinned by digest is not claimed to be
// any particular version.
func TestADigestPinnedImageIsNotComparedAgainstAVersion(t *testing.T) {
	for _, tc := range []struct {
		image   string
		version string
		want    bool
	}{
		{"quay.io/simplyblock-io/simplyblock:26.3.0", "26.3.0", true},
		{"quay.io/simplyblock-io/simplyblock:26.3.0", "26.2.8", false},
		{"quay.io/simplyblock-io/simplyblock:26.3.0@sha256:" + strings.Repeat("a", 64), "26.2.8", true},
	} {
		if got := imageStates(tc.image, tc.version); got != tc.want {
			t.Errorf("imageStates(%q, %q) = %v, want %v", tc.image, tc.version, got, tc.want)
		}
	}
}

// A Restart naming something this control plane cannot roll is refused rather
// than skipped. An operation that reported success while recycling nothing is
// worse than one that says the name was wrong.
//
// The FoundationDB cluster is the case worth stating: it is a component of the
// control plane, so a check against the component table admits it, and it is not
// rolled by a pod-template annotation, so the recycle would do nothing and the
// wait that follows would pass against a healthy database.
func TestARestartNamingSomethingItCannotRollFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
	}{
		{"a workload of another deployment", "simplyblock-graylog"},
		{"the database, which no annotation rolls", ComponentFDBCluster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := managedControlPlane()
			ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
			ops.Spec.Restart = &simplyblockv1alpha2.RestartSpec{Components: []string{tc.scope}}

			r := &ControlPlaneOpsReconciler{Client: newClient(t, cp, ops), Scheme: testScheme(t)}

			_, _, err := r.restart(context.Background(), ops, cp)
			var fatal *terminalStepError
			if !errors.As(err, &fatal) {
				t.Fatalf("restart returned %v, want a terminal failure", err)
			}
			if !strings.Contains(fatal.Error(), tc.scope) {
				t.Errorf("the refusal is %q, want it to name the component", fatal.Error())
			}
		})
	}
}

// A scoped restart stamps only what it named. Recycling the whole control plane
// to restart one wedged component would interrupt everything else for nothing.
func TestAScopedRestartRecyclesOnlyWhatItNamed(t *testing.T) {
	ctx := context.Background()
	cp := managedControlPlane()
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	ops.Spec.Restart = &simplyblockv1alpha2.RestartSpec{Components: []string{ComponentTasks}}

	c := newClient(t, cp, ops,
		deployment(ComponentTasks, 1, 1),
		deployment(ComponentWebAPI, 2, 2),
	)
	r := &ControlPlaneOpsReconciler{Client: c, Scheme: testScheme(t)}

	if _, _, err := r.restart(ctx, ops, cp); err != nil {
		t.Fatalf("restart: %v", err)
	}

	if !restarted(t, c, ComponentTasks) {
		t.Errorf("%s was not recycled although the operation named it", ComponentTasks)
	}
	if restarted(t, c, ComponentWebAPI) {
		t.Errorf("%s was recycled although the operation did not name it", ComponentWebAPI)
	}
}

// restarted reports whether a Deployment's pod template carries the restart
// stamp, which is what a rolling restart is: the Deployment controller sees a
// changed template and rolls it.
func restarted(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var d appsv1.Deployment
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	if err := c.Get(context.Background(), key, &d); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	_, stamped := d.Spec.Template.Annotations[restartedAtAnnotation]
	return stamped
}
