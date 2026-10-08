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
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// An abort is honored only before anything has been changed. A step that has
// rolled a Deployment or written an image onto the entity carries on, because
// stopping there would leave a rollout half-done with nothing driving it either
// way.
//
// Both halves are written out rather than derived. The graphs are the only place
// this is declared now, so a step quietly gaining or losing it would otherwise
// change what an abort does with nothing disagreeing.
func TestAnAbortIsRefusedOnceARolloutHasStarted(t *testing.T) {
	unabortable := UnabortableSteps()
	for _, step := range []opsStep{stepRestarting, stepApplying, stepAwaiting, stepVerifying} {
		if !slices.Contains(unabortable, step) {
			t.Errorf("%s is abortable, and an abort there leaves a rollout half-done", step)
		}
	}
	for _, step := range []opsStep{stepDraining, stepPreflight, stepRequesting} {
		if slices.Contains(unabortable, step) {
			t.Errorf("%s is not abortable, and nothing has been changed at that point", step)
		}
	}
}

// An operation naming a remote control plane is refused rather than run. The
// webhook catches it at creation, and this is what holds when the webhook was
// not serving.
func TestAnOperationAgainstARemoteControlPlaneFails(t *testing.T) {
	cp := managedControlPlane("https://sb-control.example.com:5000")
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
	if !strings.Contains(after.Status.Message, "does not host") {
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

// A second operation stays Pending and asks again rather than failing, so the
// outcome does not depend on the order two objects were applied in.
func TestASecondOperationWaitsForTheLock(t *testing.T) {
	cp := localControlPlane()
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

// Deleting an operation while it holds the lock releases the lock, so the
// control plane is never left locked by an object that no longer exists.
func TestDeletingARunningOperationReleasesTheLock(t *testing.T) {
	ctx := context.Background()
	cp := localControlPlane()
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
	cp := localControlPlane()
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
	cp := localControlPlane()
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
	cp := localControlPlane()
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
// every pass.
func TestAnUpgradeWritesTheImageOntoTheEntity(t *testing.T) {
	ctx := context.Background()
	const next = "quay.io/simplyblock-io/simplyblock:26.3.0"

	cp := localControlPlane()
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
	if got := localImage(&after); got != next {
		t.Errorf("spec.source.local.image = %q, want %q", got, next)
	}
}

// webAPIAt builds the management API's Deployment as the rollout controller
// would report it: the image its pod template carries, and how far the roll has
// got.
func webAPIAt(image string, updated, ready int32, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	const replicas int32 = 2
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ComponentWebAPI, Namespace: testNamespace, Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "webappapi", Image: image}},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           replicas,
			UpdatedReplicas:    updated,
			ReadyReplicas:      ready,
		},
	}
	for _, m := range mutate {
		m(d)
	}
	return d
}

// upgradeTo builds an Upgrade operation naming image.
func upgradeTo(image string) *simplyblockv1alpha2.ControlPlaneOps {
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade)
	ops.Spec.Upgrade = &simplyblockv1alpha2.UpgradeSpec{Image: image}
	return ops
}

// Verifying passes once the management API's Deployment carries the requested
// image on every replica and all of them are ready.
func TestVerifyingPassesOnACompletedRollout(t *testing.T) {
	mountedCA(t)
	const next = "quay.io/simplyblock-io/simplyblock:26.3.0"
	cp := localControlPlane()
	ops := upgradeTo(next)

	r := &ControlPlaneOpsReconciler{
		Client: newClient(t, cp, ops, webAPIAt(next, 2, 2)),
		Scheme: testScheme(t),
		Prober: &stubProber{ready: true},
	}

	done, held, err := r.verify(context.Background(), ops, cp)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !done {
		t.Errorf("verify held on %q after the rollout completed", held)
	}
}

// Verifying holds, and does not pass, while the Deployment still carries the old
// image. The entity re-applies its workloads asynchronously after Applying, so
// a Deployment that is fully ready on the old image is the state Verifying is
// most likely to meet first, and treating it as a finished rollout would report
// an upgrade that has not started.
func TestVerifyingHoldsWhileTheDeploymentStillCarriesTheOldImage(t *testing.T) {
	mountedCA(t)
	cp := localControlPlane()
	ops := upgradeTo("quay.io/simplyblock-io/simplyblock:26.3.0")

	r := &ControlPlaneOpsReconciler{
		Client: newClient(t, cp, ops, webAPIAt(testImage, 2, 2)),
		Scheme: testScheme(t),
		Prober: &stubProber{ready: true},
	}

	done, held, err := r.verify(context.Background(), ops, cp)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if done {
		t.Fatal("verify passed against a Deployment still on the old image")
	}
	if !strings.Contains(held, testImage) {
		t.Errorf("held on %q, want it to name the image the Deployment still carries", held)
	}
}

// Verifying holds while replicas are still being replaced, however many already
// run the new image.
func TestVerifyingHoldsWhileReplicasAreStillBeingReplaced(t *testing.T) {
	mountedCA(t)
	const next = "quay.io/simplyblock-io/simplyblock:26.3.0"
	cp := localControlPlane()
	ops := upgradeTo(next)

	for name, d := range map[string]*appsv1.Deployment{
		"one of two updated":      webAPIAt(next, 1, 1),
		"updated but not ready":   webAPIAt(next, 2, 1),
		"generation not observed": webAPIAt(next, 2, 2, func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 1 }),
	} {
		r := &ControlPlaneOpsReconciler{
			Client: newClient(t, cp, ops, d),
			Scheme: testScheme(t),
			Prober: &stubProber{ready: true},
		}
		done, _, err := r.verify(context.Background(), ops, cp)
		if err != nil {
			t.Fatalf("%s: verify: %v", name, err)
		}
		if done {
			t.Errorf("%s: verify passed before the rollout finished", name)
		}
	}
}

// A rollout whose Deployment reports ProgressDeadlineExceeded fails the
// operation, and says which image could not roll.
func TestAStalledRolloutFailsTheOperation(t *testing.T) {
	const next = "quay.io/simplyblock-io/simplyblock:does-not-exist"
	cp := localControlPlane()
	ops := upgradeTo(next)
	stalled := webAPIAt(next, 1, 1, func(d *appsv1.Deployment) {
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded",
		}}
	})

	r := &ControlPlaneOpsReconciler{
		Client: newClient(t, cp, ops, stalled),
		Scheme: testScheme(t),
		Prober: &stubProber{ready: true},
	}

	// Awaiting is where a bad image sits, so it is where the failure is raised, and
	// Verifying must agree should it ever meet the same Deployment.
	for name, step := range map[string]func(context.Context, *simplyblockv1alpha2.ControlPlaneOps, *simplyblockv1alpha2.ControlPlane) (bool, string, error){
		"await":  r.await,
		"verify": r.verify,
	} {
		_, _, err := step(context.Background(), ops, cp)
		var fatal *terminalStepError
		if !errors.As(err, &fatal) {
			t.Fatalf("%s returned %v, want a terminal failure", name, err)
		}
		if !strings.Contains(fatal.Error(), next) {
			t.Errorf("%s: the failure is %q, want it to name the image", name, fatal.Error())
		}
	}
}

// A Deployment that is slow but still progressing is not a failure.
func TestARolloutStillProgressingIsNotAFailure(t *testing.T) {
	const next = "quay.io/simplyblock-io/simplyblock:26.3.0"
	cp := localControlPlane()
	ops := upgradeTo(next)
	rolling := webAPIAt(next, 1, 1, func(d *appsv1.Deployment) {
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionTrue,
			Reason: "ReplicaSetUpdated",
		}}
	})

	r := &ControlPlaneOpsReconciler{
		Client: newClient(t, cp, ops, rolling),
		Scheme: testScheme(t),
		Prober: &stubProber{ready: true},
	}
	if _, _, err := r.await(context.Background(), ops, cp); err != nil {
		t.Errorf("await failed a rollout that is still progressing: %v", err)
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
			cp := localControlPlane()
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
	cp := localControlPlane()
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

// A Restart is not finished while the recycled Deployment is still rolling. Right
// after the restart stamp is written the old pod is still Ready, so the ready
// count alone reports success before a single replacement has started.
func TestARestartAwaitsTheRolloutItStarted(t *testing.T) {
	cp := localControlPlane()
	ops := opsFor(simplyblockv1alpha2.ControlPlaneOpsActionRestart)
	ops.Spec.Restart = &simplyblockv1alpha2.RestartSpec{Components: []string{ComponentWebAPI}}

	for name, tc := range map[string]struct {
		deploy *appsv1.Deployment
		done   bool
	}{
		"stamp written, old pod still ready": {
			webAPIAt(testImage, 0, 2, func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 1 }), false},
		"replacement rolling": {webAPIAt(testImage, 1, 1), false},
		"rolled and ready":    {webAPIAt(testImage, 2, 2), true},
	} {
		r := &ControlPlaneOpsReconciler{
			Client: newClient(t, cp, ops, tc.deploy),
			Scheme: testScheme(t),
		}
		done, held, err := r.await(context.Background(), ops, cp)
		if err != nil {
			t.Fatalf("%s: await: %v", name, err)
		}
		if done != tc.done {
			t.Errorf("%s: await done = %v (held on %q), want %v", name, done, held, tc.done)
		}
	}
}
