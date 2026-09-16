// The ControlPlaneOps reconciler: the lock, the three actions, and the finalizer
// that releases the lock even when the operation is deleted while holding it.
//
// One operation per control plane is the strongest form of the limit in this
// group, because a control-plane operation interrupts every controller rather
// than one cluster or one node. A second operation is admitted, sits at Pending,
// and runs when the lock frees.
//
// Every action requires a managed control plane, since each acts on something
// the operator installed. The webhook refuses one naming an external control
// plane at creation; this reconciler repeats the check, because an object may
// have been created while the webhook was not serving.
//
// design-controlplane.md §6 and §7 are the specification.

package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// FinalizerControlPlaneOps is what guarantees the lock is released even when
	// the operation is deleted while it holds one, so that the control plane is
	// never left locked by an object that no longer exists.
	FinalizerControlPlaneOps = "storage.simplyblock.io/controlplaneops-finalizer"

	// opsRetry is how long an operation waits before looking again at something
	// it cannot hurry: a lock another operation holds, or work in flight it is
	// draining behind.
	opsRetry = 15 * time.Second

	// opsAdvance is how long a pass that moved the machine forward waits.
	opsAdvance = time.Second

	// restartedAtAnnotation is what recycles a workload. Writing a new value
	// into the pod template is what a rolling restart is: the Deployment
	// controller sees a changed template and rolls it, which is the same
	// mechanism `kubectl rollout restart` uses.
	restartedAtAnnotation = "storage.simplyblock.io/restartedAt"
)

// ControlPlaneOpsReconciler reconciles a ControlPlaneOps.
type ControlPlaneOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Prober is what Verifying re-reads the version with. It is the same
	// interface the entity's reconciler uses, for the same reason.
	Prober Prober
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplaneops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplaneops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplaneops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops;storagenodeops;storagepoolops,verbs=get;list;watch

// Reconcile advances one operation by at most one step.
func (r *ControlPlaneOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ops simplyblockv1alpha2.ControlPlaneOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &ops)
	}

	// A terminal operation is a record rather than a task. Re-reconciling one
	// does nothing at all, including nothing to its target: an operation that
	// finished has already released the lock, and taking it again to release it
	// again is how an unrelated operation loses one it legitimately holds.
	if terminalOps(ops.Status.Phase) {
		return ctrl.Result{}, r.ensureFinalizer(ctx, &ops)
	}
	if err := r.ensureFinalizer(ctx, &ops); err != nil {
		return ctrl.Result{}, err
	}

	target := &simplyblockv1alpha2.ControlPlane{}
	key := client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.ControlPlaneRef}
	switch err := r.Get(ctx, key, target); {
	case apierrors.IsNotFound(err):
		return r.finish(ctx, &ops, nil, simplyblockv1alpha2.ControlPlaneOpsPhaseFailed,
			fmt.Sprintf("no ControlPlane %q in namespace %s", ops.Spec.ControlPlaneRef, ops.Namespace))
	case err != nil:
		return ctrl.Result{}, err
	}

	// The webhook refuses this at creation. It is repeated here because an
	// object may carry a source the webhook was not serving to check, and
	// because spec.source is immutable so the answer cannot have changed since.
	if !isLocal(target) {
		return r.failAndRelease(ctx, &ops, target,
			fmt.Sprintf("ControlPlane %q names a control plane this cluster does not host, "+
				"and every action of this kind acts on something the operator installed",
				target.Name))
	}

	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseRunning {
		acquired, result, err := r.acquireLock(ctx, &ops, target)
		if err != nil || !acquired {
			return result, err
		}
	}

	return r.advance(ctx, &ops, target)
}

// acquireLock takes the target's status.activeOpsRef, which is the mutual
// exclusion every Ops kind in this group uses. An operation that finds the lock
// held by another stays Pending and asks again: the other operation finishes,
// and the wait is what keeps the outcome independent of the order two objects
// were applied in.
func (r *ControlPlaneOpsReconciler) acquireLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, ctrl.Result, error) {
	if held := target.Status.ActiveOpsRef; held != "" && held != ops.Name {
		if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhasePending {
			if err := r.setPhase(ctx, ops, simplyblockv1alpha2.ControlPlaneOpsPhasePending,
				fmt.Sprintf("waiting for operation %q to release the control plane", held)); err != nil {
				return false, ctrl.Result{}, err
			}
		}
		r.emit(ops, corev1.EventTypeNormal, OperationQueued,
			fmt.Sprintf("control plane %q is held by operation %q", target.Name, held))
		return false, ctrl.Result{RequeueAfter: opsRetry}, nil
	}

	// The optimistic lock is what makes this a lock at all: two reconcilers that
	// both saw a free field patch the same resourceVersion, and one of them gets
	// a 409 and comes back to find the field taken.
	base := target.DeepCopy()
	target.Status.ActiveOpsRef = ops.Name
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Status().Patch(ctx, target, patch); err != nil {
		return false, ctrl.Result{RequeueAfter: opsRetry}, nil //nolint:nilerr // a lost race is retried, not failed
	}

	now := metav1.Now()
	opsBase := ops.DeepCopy()
	ops.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	ops.Status.StartedAt = &now
	ops.Status.Message = ""
	ops.Status.ObservedGeneration = ops.Generation
	if err := r.Status().Patch(ctx, ops, client.MergeFrom(opsBase)); err != nil {
		return false, ctrl.Result{}, err
	}
	r.emit(ops, corev1.EventTypeNormal, OperationStarted,
		fmt.Sprintf("acquired control plane %q and started %s", target.Name, ops.Spec.Action))
	return true, ctrl.Result{Requeue: true}, nil
}

// advance runs the action's machine forward by at most one step.
func (r *ControlPlaneOpsReconciler) advance(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (ctrl.Result, error) {
	machine, err := opsGraphs().FromSnapshot(ctx, action(ops.Spec.Action),
		statemachine.FromKube[opsStep](ops.Status.Step))
	if err != nil {
		// An unrecognized step or action is a downgrade, a hand-edited object,
		// or a rename that shipped without a conversion, and none of them
		// resolve by reconciling again.
		return r.failAndRelease(ctx, ops, target,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}

	current := machine.CurrentState()

	if ops.Spec.Abort {
		return r.unwind(ctx, ops, target, current)
	}

	if machine.TimeoutReached() {
		return r.failAndRelease(ctx, ops, target,
			fmt.Sprintf("step %s outlived its deadline", current))
	}

	done, held, err := r.perform(ctx, ops, target, current)
	switch {
	case err != nil:
		var fatal *terminalStepError
		if errorsAs(err, &fatal) {
			return r.failAndRelease(ctx, ops, target, fatal.Error())
		}
		return ctrl.Result{}, err
	case !done:
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, held)
	}

	next, ok := nextOpsStep(machine)
	if !ok {
		if err := r.releaseLock(ctx, ops, target); err != nil {
			return ctrl.Result{}, err
		}
		return r.finish(ctx, ops, target, simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded,
			fmt.Sprintf("%s finished", ops.Spec.Action))
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: opsAdvance}, r.recordStep(ctx, ops, next, snapshot.Deadline)
}

// terminalStepError is a step failure nothing recovers from by reconciling
// again: a precondition that cannot become true, or a verification that
// disagreed.
type terminalStepError struct{ message string }

func (e *terminalStepError) Error() string { return e.message }

// perform does what one step is for, and reports whether the step is finished. A
// step that is not finished returns what it is holding on, which lands in
// status.message.
func (r *ControlPlaneOpsReconciler) perform(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
	current opsStep,
) (done bool, held string, err error) {
	switch current {
	case stepPreflight:
		return r.preflight(ctx, ops, target)
	case stepDraining:
		return r.drain(ctx, ops)
	case stepRestarting:
		return r.restart(ctx, ops, target)
	case stepApplying:
		return r.applyUpgrade(ctx, ops, target)
	case stepAwaiting:
		return r.await(ctx, ops, target)
	case stepVerifying:
		return r.verify(ctx, ops, target)
	case stepRequesting:
		return r.requestBackup(ctx, ops, target)
	default:
		return false, "", &terminalStepError{message: fmt.Sprintf("unknown step %q", current)}
	}
}

// preflight reads live state, which admission cannot. It holds until the control
// plane is Available, and it fails when the requested image is the one already
// running, because rolling a Deployment to its current image produces no change
// to verify.
func (r *ControlPlaneOpsReconciler) preflight(
	_ context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	if ops.Spec.Upgrade == nil || ops.Spec.Upgrade.Image == "" {
		return false, "", &terminalStepError{
			message: "action Upgrade needs spec.upgrade.image, which names the version to move to",
		}
	}
	if target.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseAvailable {
		return false, fmt.Sprintf(
			"the control plane is %s; an upgrade starts from Available so that what it verifies "+
				"afterward is a change rather than a recovery", target.Status.Phase), nil
	}
	if localImage(target) == ops.Spec.Upgrade.Image {
		return false, "", &terminalStepError{message: fmt.Sprintf(
			"the control plane already runs %s, so there is no rollout to verify",
			ops.Spec.Upgrade.Image)}
	}
	return true, "", nil
}

// drain holds while any operation in the namespace is still running.
//
// The management API is what every controller in the operator talks to, so
// replacing or restarting it mid-flight fails whatever is in flight. It does not
// cancel them, because an operation canceled to make a restart convenient is a
// worse outcome than a restart that waited.
//
// A scoped restart drains only when it names something depended on: the list is
// empty, or it names a component the phase table marks essential. Recycling an
// exporter interrupts nothing, so a restart of it has nothing to wait for.
func (r *ControlPlaneOpsReconciler) drain(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (bool, string, error) {
	if !drainsFirst(ops) {
		return true, "", nil
	}

	inFlight, err := r.operationsInFlight(ctx, ops)
	if err != nil {
		return false, "", err
	}
	if len(inFlight) == 0 {
		return true, "", nil
	}

	message := fmt.Sprintf(
		"%d operations are still running in %s, and recycling the management API would fail "+
			"whatever they have in flight: %v", len(inFlight), ops.Namespace, inFlight)
	r.emit(ops, corev1.EventTypeNormal, OperationsInFlight, message)
	return false, message, nil
}

// drainsFirst reports whether this operation has to wait for in-flight work.
func drainsFirst(ops *simplyblockv1alpha2.ControlPlaneOps) bool {
	if ops.Spec.Action != simplyblockv1alpha2.ControlPlaneOpsActionRestart {
		return true
	}
	scope := restartScope(ops)
	if len(scope) == 0 {
		return true
	}
	essential := essentialComponents()
	for _, name := range scope {
		if essential[name] {
			return true
		}
	}
	return false
}

// restartScope is the components a Restart names, or every restartable one when
// it names none.
func restartScope(ops *simplyblockv1alpha2.ControlPlaneOps) []string {
	if ops.Spec.Restart == nil {
		return nil
	}
	return ops.Spec.Restart.Components
}

// operationsInFlight lists the operations in the namespace that are still
// Running. It reads the three kinds whose work a control-plane restart would
// interrupt, which is every Ops kind that reaches the control plane over HTTP.
func (r *ControlPlaneOpsReconciler) operationsInFlight(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) ([]string, error) {
	var running []string
	inNamespace := client.InNamespace(ops.Namespace)

	var clusters simplyblockv1alpha2.StorageClusterOpsList
	if err := r.List(ctx, &clusters, inNamespace); err != nil {
		return nil, err
	}
	for _, item := range clusters.Items {
		if item.Status.Phase == simplyblockv1alpha2.StorageClusterOpsPhaseRunning {
			running = append(running, "StorageClusterOps/"+item.Name)
		}
	}

	var nodes simplyblockv1alpha2.StorageNodeOpsList
	if err := r.List(ctx, &nodes, inNamespace); err != nil {
		return nil, err
	}
	for _, item := range nodes.Items {
		if item.Status.Phase == simplyblockv1alpha2.StorageNodeOpsPhaseRunning {
			running = append(running, "StorageNodeOps/"+item.Name)
		}
	}

	var pools simplyblockv1alpha2.StoragePoolOpsList
	if err := r.List(ctx, &pools, inNamespace); err != nil {
		return nil, err
	}
	for _, item := range pools.Items {
		if item.Status.Phase == simplyblockv1alpha2.StoragePoolOpsPhaseRunning {
			running = append(running, "StoragePoolOps/"+item.Name)
		}
	}

	slices.Sort(running)
	return running, nil
}

// restart recycles the workloads the operation named, by writing a new value
// into their pod templates.
//
// A component the table does not know is refused rather than skipped: an
// operation that reported success while recycling nothing is worse than one that
// says the name was wrong.
func (r *ControlPlaneOpsReconciler) restart(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	scope := restartScope(ops)
	for _, name := range scope {
		if !restartable(name) {
			return false, "", &terminalStepError{message: fmt.Sprintf(
				"%q is not a workload this control plane can recycle", name)}
		}
	}

	stamp := metav1.Now().UTC().Format(time.RFC3339)
	for _, component := range restartableComponents() {
		if len(scope) > 0 && !slices.Contains(scope, component.name) {
			continue
		}
		if err := r.recycle(ctx, target.Namespace, component, stamp); err != nil {
			return false, "", err
		}
	}
	return true, "", nil
}

// recycle writes the restart stamp onto one workload's pod template.
func (r *ControlPlaneOpsReconciler) recycle(
	ctx context.Context, namespace string, component component, stamp string,
) error {
	key := client.ObjectKey{Namespace: namespace, Name: component.name}

	switch component.kind {
	case kindDeployment:
		var d appsv1.Deployment
		if err := r.Get(ctx, key, &d); err != nil {
			// A workload that is not there is one the entity's next pass
			// re-applies, and recycling it is not this operation's problem.
			return client.IgnoreNotFound(err)
		}
		base := d.DeepCopy()
		stampTemplate(&d.Spec.Template, stamp)
		return r.Patch(ctx, &d, client.MergeFrom(base))

	case kindStatefulSet:
		var s appsv1.StatefulSet
		if err := r.Get(ctx, key, &s); err != nil {
			return client.IgnoreNotFound(err)
		}
		base := s.DeepCopy()
		stampTemplate(&s.Spec.Template, stamp)
		return r.Patch(ctx, &s, client.MergeFrom(base))

	default:
		// The FoundationDBCluster is not restartable this way, and
		// restartableComponents already excluded it.
		return nil
	}
}

func stampTemplate(template *corev1.PodTemplateSpec, stamp string) {
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[restartedAtAnnotation] = stamp
}

// applyUpgrade writes the new image onto the entity, which is what rolls the
// Deployment: the entity re-applies its workloads on every pass, and the image
// it applies is the one in its own spec.
//
// Writing it to the spec rather than to the Deployment is what keeps the entity
// describing what is running: the entity's next pass re-applies its workloads
// from the spec.
func (r *ControlPlaneOpsReconciler) applyUpgrade(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	// Preflight read this block, and the CEL rule on the spec freezes it after
	// admission, so reaching here without one means an object written before that
	// rule shipped. It is a terminal failure rather than a panic.
	if ops.Spec.Upgrade == nil || ops.Spec.Upgrade.Image == "" {
		return false, "", &terminalStepError{
			message: "spec.upgrade.image is gone, so there is no version to move to",
		}
	}

	base := target.DeepCopy()
	target.Spec.Source.Local.Image = ops.Spec.Upgrade.Image
	if err := r.Patch(ctx, target, client.MergeFrom(base)); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// await waits for what the previous step asked for: the recycled pods coming
// back, or the backup reporting a snapshot.
func (r *ControlPlaneOpsReconciler) await(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	if ops.Spec.Action == simplyblockv1alpha2.ControlPlaneOpsActionBackup {
		return r.awaitBackup(ctx, ops, target)
	}

	components, err := observe(ctx, r.Client, target.Namespace)
	if err != nil {
		return false, "", err
	}
	scope := restartScope(ops)
	for _, status := range components {
		if len(scope) > 0 && !slices.Contains(scope, status.Name) {
			continue
		}
		if status.Desired > 0 && status.Ready < status.Desired {
			return false, fmt.Sprintf("%s has %d of %d replicas ready",
				status.Name, status.Ready, status.Desired), nil
		}
	}
	return true, "", nil
}

// verify is what makes an upgrade more than an image bump. It re-probes
// readiness, compares the reported version against what was asked for, and fails
// the operation when they disagree.
//
// A control plane that reports no version at all is a control plane whose
// /_meta/version read does not exist yet, which design-controlplane.md §8
// records as a prerequisite. The step passes there rather than failing, and says
// so in the message, so the record of the operation carries what was and was not
// verified.
func (r *ControlPlaneOpsReconciler) verify(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	endpoint := target.Status.Endpoint
	if endpoint == "" {
		endpoint = localEndpoint(target.Namespace)
	}

	prober := r.Prober
	if prober == nil {
		prober = &HTTPProber{}
	}
	if ok, reason := prober.Ready(ctx, endpoint); !ok {
		return false, fmt.Sprintf("the control plane is not answering yet: %s", reason), nil
	}

	reported, err := prober.Version(ctx, endpoint)
	if err != nil {
		return false, fmt.Sprintf("the version could not be read: %v", err), nil
	}
	if reported == "" {
		return true, "", r.note(ctx, ops,
			"the rollout finished; the version was not verified because the control plane "+
				"does not serve a version endpoint")
	}
	if ops.Spec.Upgrade == nil {
		return false, "", &terminalStepError{
			message: "spec.upgrade.image is gone, so there is nothing to verify the rollout against",
		}
	}
	if !imageStates(ops.Spec.Upgrade.Image, reported) {
		r.emit(ops, corev1.EventTypeWarning, VersionMismatch, fmt.Sprintf(
			"the control plane reports %s and the upgrade asked for %s",
			reported, ops.Spec.Upgrade.Image))
		return false, "", &terminalStepError{message: fmt.Sprintf(
			"the rollout finished with the control plane reporting %s rather than %s, which is "+
				"a rollout that failed back rather than an upgrade that completed",
			reported, ops.Spec.Upgrade.Image)}
	}
	return true, "", nil
}

// imageStates reports whether an image reference names the version the control
// plane reports. The comparison is against the image's tag rather than the whole
// reference, because a version endpoint answers with a version and an image
// carries a registry and a repository in front of it.
//
// The digest is cut off before the tag is read, and the order is the whole of
// the correctness: a digest carries its own colon, so reading the tag from the
// last colon of repo:tag@sha256:… yields the hex digest rather than the tag.
//
// A digest-pinned image is then not compared at all. What is pinned by digest is
// not claimed to be any particular version, so an upgrade to one verifies that
// the rollout finished and stops there.
func imageStates(image, version string) bool {
	reference := image
	if at := strings.LastIndexByte(reference, '@'); at >= 0 {
		return true
	}

	colon := strings.LastIndexByte(reference, ':')
	if colon < 0 {
		// No tag at all, which the spec's pattern does not admit. There is
		// nothing to compare, so the rollout finishing is the whole of the
		// verification.
		return true
	}
	return reference[colon+1:] == version
}

// requestBackup creates or triggers the FoundationDBBackup.
//
// The object outlives the operation and is not owned by it: deleting the record
// of a backup having been taken must not delete the backup's configuration. An
// operation that finds one already configured triggers a snapshot on it rather
// than applying a second beside it.
func (r *ControlPlaneOpsReconciler) requestBackup(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	if ops.Spec.Backup == nil || ops.Spec.Backup.BlobStore == "" {
		return false, "", &terminalStepError{
			message: "action Backup needs spec.backup.blobStore, which names where the backup goes",
		}
	}

	name := ops.Spec.Backup.BackupName
	if name == "" {
		name = ComponentFDBCluster
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(fdbBackupGVK)
	key := client.ObjectKey{Namespace: target.Namespace, Name: name}
	err := r.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(err):
		backup := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{
				"clusterName": ComponentFDBCluster,
				"blobStoreConfiguration": map[string]any{
					"accountName": ops.Spec.Backup.BlobStore,
				},
			},
		}}
		backup.SetGroupVersionKind(fdbBackupGVK)
		backup.SetName(name)
		backup.SetNamespace(target.Namespace)
		if err := r.Create(ctx, backup); err != nil {
			return false, "", err
		}
		r.emit(ops, corev1.EventTypeNormal, BackupRequested,
			fmt.Sprintf("created FoundationDBBackup %s", name))

	case err != nil:
		return false, "", err

	default:
		// Running is what tells the FoundationDB operator to take and keep
		// taking snapshots. An object already in that state needs nothing
		// written to it, and writing anyway would restart a backup mid-flight.
		state, _, _ := unstructured.NestedString(existing.Object, "spec", "backupState")
		if state != "Running" {
			base := existing.DeepCopy()
			if err := unstructured.SetNestedField(existing.Object, "Running", "spec", "backupState"); err != nil {
				return false, "", err
			}
			if err := r.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
				return false, "", err
			}
		}
		r.emit(ops, corev1.EventTypeNormal, BackupTriggered,
			fmt.Sprintf("triggered the FoundationDBBackup %s that was already configured", name))
	}

	return true, "", r.recordBackupRef(ctx, ops, name)
}

// awaitBackup completes when the backup reports that it is running, which is
// what a FoundationDBBackup says when its agents have started and it is taking
// snapshots.
func (r *ControlPlaneOpsReconciler) awaitBackup(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) (bool, string, error) {
	backup := &unstructured.Unstructured{}
	backup.SetGroupVersionKind(fdbBackupGVK)
	key := client.ObjectKey{Namespace: target.Namespace, Name: ops.Status.BackupRef}
	if err := r.Get(ctx, key, backup); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("FoundationDBBackup %s has not appeared yet",
				ops.Status.BackupRef), nil
		}
		return false, "", err
	}

	running, _, _ := unstructured.NestedBool(backup.Object, "status", "running")
	if !running {
		return false, fmt.Sprintf("FoundationDBBackup %s has not started taking snapshots yet",
			ops.Status.BackupRef), nil
	}
	return true, "", nil
}

// unwind honors spec.abort where the graph allows it, and reports an abort that
// arrived too late rather than half-undoing the work.
//
// The refusal is the point. A step past the drain has rolled a Deployment or
// written an image onto the entity, and this operation is what drives that
// rollout to completion.
func (r *ControlPlaneOpsReconciler) unwind(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
	current opsStep,
) (ctrl.Result, error) {
	if !abortable(current) {
		// Not a failure of the operation: it carries on. What the user asked for
		// cannot be done, and saying so is the whole of the response.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, fmt.Sprintf(
			"the abort arrived at step %s, which has already started a rollout that nothing "+
				"else would finish; the operation is running on", current))
	}

	// Nothing before the drain performs a side effect, so there is nothing to
	// unwind: the abort is the lock being released and a terminal phase.
	if err := r.releaseLock(ctx, ops, target); err != nil {
		return ctrl.Result{}, err
	}
	r.emit(ops, corev1.EventTypeNormal, OperationAborted,
		fmt.Sprintf("aborted at step %s, before anything was changed", current))
	return r.finish(ctx, ops, target, simplyblockv1alpha2.ControlPlaneOpsPhaseAborted,
		fmt.Sprintf("aborted at step %s", current))
}

// enterInitialStep sets the first step's deadline, which a machine born already
// in that step would otherwise never get.
func (r *ControlPlaneOpsReconciler) enterInitialStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	machine *statemachine.Machine[opsStep],
) (ctrl.Result, error) {
	budget, ok := opsInitialDeadlines[action(ops.Spec.Action)]
	if !ok {
		budget = requestingDeadline
	}
	stepDeadline := metav1.NewTime(time.Now().Add(budget))
	return ctrl.Result{RequeueAfter: opsAdvance},
		r.recordStep(ctx, ops, machine.CurrentState(), &stepDeadline)
}

// nextOpsStep is the step that follows the current one. Every graph in this
// package is a line, so the first edge is the only edge.
func nextOpsStep(machine *statemachine.Machine[opsStep]) (opsStep, bool) {
	for next := range machine.AllowedTransitions() {
		return next, true
	}
	return machine.CurrentState(), false
}

// releaseLock clears the target's lock, and only when this operation is the one
// holding it. The guard is the whole of the safety: an operation that never
// acquired the lock, or whose lock was taken over, must not clear somebody
// else's.
func (r *ControlPlaneOpsReconciler) releaseLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
) error {
	if target == nil || target.Status.ActiveOpsRef != ops.Name {
		return nil
	}
	base := target.DeepCopy()
	target.Status.ActiveOpsRef = ""
	if err := r.Status().Patch(ctx, target, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("release the lock on control plane %s/%s: %w",
			target.Namespace, target.Name, err)
	}
	return nil
}

func (r *ControlPlaneOpsReconciler) failAndRelease(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
	message string,
) (ctrl.Result, error) {
	if err := r.releaseLock(ctx, ops, target); err != nil {
		return ctrl.Result{}, err
	}
	r.emit(ops, corev1.EventTypeWarning, OperationFailed, message)
	return r.finish(ctx, ops, target, simplyblockv1alpha2.ControlPlaneOpsPhaseFailed, message)
}

// reconcileDeletion releases the lock the operation may still hold, then lets the
// object go. This is the path that matters most: an operation deleted while
// Running has taken a lock that nothing else would ever clear.
func (r *ControlPlaneOpsReconciler) reconcileDeletion(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, FinalizerControlPlaneOps) {
		return ctrl.Result{}, nil
	}

	target := &simplyblockv1alpha2.ControlPlane{}
	key := client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.ControlPlaneRef}
	switch err := r.Get(ctx, key, target); {
	case apierrors.IsNotFound(err):
		// The control plane went first, so there is no lock left to release.
	case err != nil:
		return ctrl.Result{}, err
	default:
		if err := r.releaseLock(ctx, ops, target); err != nil {
			return ctrl.Result{}, err
		}
	}

	base := ops.DeepCopy()
	controllerutil.RemoveFinalizer(ops, FinalizerControlPlaneOps)
	if err := r.Patch(ctx, ops, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("release the finalizer on operation %s/%s: %w",
			ops.Namespace, ops.Name, err)
	}
	return ctrl.Result{}, nil
}

func (r *ControlPlaneOpsReconciler) ensureFinalizer(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps,
) error {
	if controllerutil.ContainsFinalizer(ops, FinalizerControlPlaneOps) {
		return nil
	}
	base := ops.DeepCopy()
	controllerutil.AddFinalizer(ops, FinalizerControlPlaneOps)
	return r.Patch(ctx, ops, client.MergeFrom(base))
}

// finish writes a terminal phase and the time it was reached, and records the
// operation in the two cumulative series.
func (r *ControlPlaneOpsReconciler) finish(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
	phase simplyblockv1alpha2.ControlPlaneOpsPhase,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	base := ops.DeepCopy()
	ops.Status.Phase = phase
	ops.Status.Message = message
	ops.Status.CompletedAt = &now
	ops.Status.ObservedGeneration = ops.Generation
	if err := r.Status().Patch(ctx, ops, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	if phase == simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded {
		r.emit(ops, corev1.EventTypeNormal, OperationSucceeded, message)
	}
	recordOperation(ops, target, phase)
	return ctrl.Result{}, nil
}

// recordOperation adds one terminal operation to the counter, and its duration
// to the histogram when the operation got far enough to have one. An operation
// that never started has no duration to report, and reporting zero would drag
// the distribution toward a value nothing took.
func recordOperation(
	ops *simplyblockv1alpha2.ControlPlaneOps,
	target *simplyblockv1alpha2.ControlPlane,
	phase simplyblockv1alpha2.ControlPlaneOpsPhase,
) {
	namespace := ops.Namespace
	if target != nil {
		namespace = target.Namespace
	}
	act := string(ops.Spec.Action)
	controlPlaneOperationsTotal.WithLabelValues(namespace, act, string(phase)).Inc()
	if ops.Status.StartedAt != nil && ops.Status.CompletedAt != nil {
		seconds := ops.Status.CompletedAt.Sub(ops.Status.StartedAt.Time).Seconds()
		controlPlaneOperationDuration.WithLabelValues(namespace, act).Observe(seconds)
	}
}

// recordStep persists the step and its deadline before the side effect that step
// performs.
func (r *ControlPlaneOpsReconciler) recordStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	next opsStep,
	stepDeadline *metav1.Time,
) error {
	base := ops.DeepCopy()
	ops.Status.Phase = simplyblockv1alpha2.ControlPlaneOpsPhaseRunning
	ops.Status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: stepDeadline}
	ops.Status.ObservedGeneration = ops.Generation
	return r.Status().Patch(ctx, ops, client.MergeFrom(base))
}

func (r *ControlPlaneOpsReconciler) recordBackupRef(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps, name string,
) error {
	if ops.Status.BackupRef == name {
		return nil
	}
	base := ops.DeepCopy()
	ops.Status.BackupRef = name
	return r.Status().Patch(ctx, ops, client.MergeFrom(base))
}

// note records what the operation is holding on, without moving it. A held step
// is correct behavior waiting on something, and the message is how somebody
// reading the object learns what.
func (r *ControlPlaneOpsReconciler) note(
	ctx context.Context, ops *simplyblockv1alpha2.ControlPlaneOps, message string,
) error {
	if ops.Status.Message == message {
		return nil
	}
	base := ops.DeepCopy()
	ops.Status.Message = message
	return r.Status().Patch(ctx, ops, client.MergeFrom(base))
}

func (r *ControlPlaneOpsReconciler) setPhase(
	ctx context.Context,
	ops *simplyblockv1alpha2.ControlPlaneOps,
	phase simplyblockv1alpha2.ControlPlaneOpsPhase,
	message string,
) error {
	base := ops.DeepCopy()
	ops.Status.Phase = phase
	ops.Status.Message = message
	ops.Status.ObservedGeneration = ops.Generation
	return r.Status().Patch(ctx, ops, client.MergeFrom(base))
}

func (r *ControlPlaneOpsReconciler) emit(
	object client.Object, eventType, reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(object, nil, eventType, reason, reason, "%s", message)
}

// terminalOps reports whether a phase is one the operation never leaves.
func terminalOps(phase simplyblockv1alpha2.ControlPlaneOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.ControlPlaneOpsPhaseSucceeded,
		simplyblockv1alpha2.ControlPlaneOpsPhaseFailed,
		simplyblockv1alpha2.ControlPlaneOpsPhaseAborted:
		return true
	default:
		return false
	}
}

// SetupWithManager registers the reconciler.
func (r *ControlPlaneOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.ControlPlaneOps{}).
		Named("controlplaneops").
		Complete(r)
}
