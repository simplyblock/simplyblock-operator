// The reconciler for StorageDeviceOps, which today means one action: restarting
// one device rather than the storage node it sits in.
//
// The operation holds its device's lock for as long as it runs, which is
// status.activeOpsRef on the StorageDevice — the field design-storagedevice.md
// §4.2 declared empty against this kind arriving. Two operations on one device
// would be two restarts of one controller, and the second would be issued
// against a device that is halfway through the first.
//
// Nothing here blocks. A step that is not finished requeues, and the step it is
// on is in the status, so a controller restart resumes rather than restarts.
//
// design-storagedevice.md §6 is the specification, and §6's other four actions
// are blocked on control-plane verbs that do not exist
// (v1alpha2.ExternalDependencies).

package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// deviceOpsFinalizer holds an operation until it has released its device's
	// lock. A delete arriving mid-restart would otherwise leave the device
	// pointing at an operation nobody can read, and the next operation waiting
	// on a holder that does not exist.
	deviceOpsFinalizer = "storage.simplyblock.io/storagedeviceops-finalizer"

	// deviceOpsRetry is how often a waiting operation looks again.
	deviceOpsRetry = 10 * time.Second
)

// The reasons a device operation emits.
const (
	DeviceOperationStarted   = "OperationStarted"
	DeviceOperationSucceeded = "OperationSucceeded"
	DeviceOperationFailed    = "OperationFailed"
	DeviceOperationAborted   = "OperationAborted"
	DeviceRestartRequested   = "DeviceRestartRequested"
	DeviceStepDeadlineGone   = "StepDeadlineExceeded"
)

// DeviceClient is what the operation asks of the control plane. It is an
// interface rather than the client so the reconciler is testable against a
// control plane that refuses, which is half of what these steps are about.
type DeviceClient interface {
	// Device reads one device, wrapping errs.ErrNotFound for one the control
	// plane does not hold.
	Device(ctx context.Context, clusterID, nodeID, deviceID string) (controlplane.Device, error)

	// RestartDevice recycles one device in place.
	RestartDevice(ctx context.Context, clusterID, nodeID, deviceID string) error
}

// StorageDeviceOpsReconciler runs operations against one device.
type StorageDeviceOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// API is the control plane the operation issues its call to.
	API DeviceClient
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedeviceops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedeviceops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedeviceops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices/status,verbs=get;update;patch

// Reconcile advances one device operation by one step.
func (r *StorageDeviceOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ops simplyblockv1alpha2.StorageDeviceOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &ops)
	}
	if !controllerutil.ContainsFinalizer(&ops, deviceOpsFinalizer) {
		controllerutil.AddFinalizer(&ops, deviceOpsFinalizer)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// Terminal and staying that way: the operation is the audit record now.
	switch ops.Status.Phase {
	case simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageDeviceOpsPhaseFailed,
		simplyblockv1alpha2.StorageDeviceOpsPhaseAborted:
		return ctrl.Result{}, nil
	}

	device, err := r.target(ctx, &ops)
	if err != nil {
		var refusal *deviceRefusal
		if errors.As(err, &refusal) {
			return ctrl.Result{}, r.fail(ctx, &ops, refusal.Error())
		}
		return ctrl.Result{}, err
	}

	held, err := r.acquireLock(ctx, &ops, device)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !held {
		return ctrl.Result{RequeueAfter: deviceOpsRetry}, r.note(ctx, &ops,
			fmt.Sprintf("waiting for %s to finish with device %s",
				device.Status.ActiveOpsRef, device.Name))
	}

	return r.advance(ctx, &ops, device)
}

// advance runs the action's graph forward by at most one step.
func (r *StorageDeviceOpsReconciler) advance(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (ctrl.Result, error) {
	graph, declared := storageDeviceOpsGraphs()[statemachine.Action(ops.Spec.Action)]
	if !declared {
		// Admission's enum admits only the actions this operator performs, so
		// reaching here means an older CRD served the object. The message names
		// the dependency rather than the enum, because what a user needs to
		// know is that the action is not available yet.
		return ctrl.Result{}, r.fail(ctx, ops, unavailableAction(ops.Spec.Action))
	}

	machine, err := statemachine.NewFromSnapshot(ctx, graph,
		statemachine.FromKube[deviceStep](ops.Status.Step))
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ops,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	if ops.Status.Step.State == "" {
		return r.begin(ctx, ops, device, machine.CurrentState())
	}

	current := machine.CurrentState()
	if ops.Spec.Abort {
		if !machine.CanAbort() {
			return ctrl.Result{RequeueAfter: deviceOpsRetry}, r.note(ctx, ops, fmt.Sprintf(
				"an abort was asked for and step %s cannot be stopped; the restart has been "+
					"issued and nothing recalls one", current))
		}
		return ctrl.Result{}, r.abort(ctx, ops, device, current)
	}

	if machine.TimeoutReached() {
		expired := deviceTimeoutMessage(current, device.Name)
		r.event(ops, corev1.EventTypeWarning, DeviceStepDeadlineGone, expired)
		return ctrl.Result{}, r.fail(ctx, ops, expired)
	}

	done, err := r.performStep(ctx, ops, device, current)
	if err != nil {
		var refusal *deviceRefusal
		if errors.As(err, &refusal) {
			return ctrl.Result{}, r.fail(ctx, ops, refusal.Error())
		}
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: deviceOpsRetry}, nil
	}

	if machine.IsTerminal() {
		return ctrl.Result{}, r.succeed(ctx, ops, device)
	}

	next, ok := nextDeviceStep(machine)
	if !ok {
		return ctrl.Result{}, r.fail(ctx, ops,
			fmt.Sprintf("step %s declares no successor and is not terminal", current))
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	return ctrl.Result{RequeueAfter: deviceOpsRetry}, r.enter(ctx, ops, next,
		statemachine.ToKube(machine.Snapshot()).Deadline)
}

// performStep runs one step and reports whether it has finished.
func (r *StorageDeviceOpsReconciler) performStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
	current deviceStep,
) (bool, error) {
	switch current {
	case stepDeviceRequesting:
		return r.request(ctx, ops, device)
	case stepDeviceAwaiting:
		return r.await(ctx, ops, device)
	default:
		return false, fmt.Errorf("step %s belongs to no operation this operator performs", current)
	}
}

// request issues the restart.
//
// It is issued once per operation and not once per pass. The step is persisted
// before the call, so re-entering it means the previous pass died between the
// write and the call — and a restart issued twice is a device recycled twice,
// which is the failure the write-ahead record exists to avoid rather than one
// to make idempotent.
func (r *StorageDeviceOpsReconciler) request(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (bool, error) {
	if ops.Status.StartedAt != nil && ops.Status.DeviceStatusBefore != "" {
		// The call was issued on an earlier pass and its record survived.
		return true, nil
	}

	cluster, node, id := device.Status.ClusterID, device.Status.NodeID, device.Spec.DeviceID
	if cluster == "" || node == "" || id == "" {
		return false, refuseDevice(
			"device %s does not report which cluster, node, and device it is, so there is "+
				"nothing to address the restart to", device.Name)
	}

	before := device.Status.DeviceStatus
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
		status.DeviceStatusBefore = before
	}); err != nil {
		return false, err
	}

	if err := r.API.RestartDevice(ctx, cluster, node, id); err != nil {
		return false, refuseDevice(
			"the control plane refused to restart device %s: %v", device.Name, err)
	}
	r.event(ops, corev1.EventTypeNormal, DeviceRestartRequested,
		fmt.Sprintf("asked the control plane to restart device %s", device.Name))
	return true, nil
}

// await waits for the control plane to report the device back in service.
//
// What it waits for is the status the device reports rather than an absence:
// a restart takes the device out and brings it back, and a check that only
// looked for it being gone would finish on the way down.
func (r *StorageDeviceOpsReconciler) await(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (bool, error) {
	current, err := r.API.Device(ctx,
		device.Status.ClusterID, device.Status.NodeID, device.Spec.DeviceID)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		// A device the control plane has stopped holding did not come back
		// from the restart, which is a finding rather than a wait: §5.2 deletes
		// the object when the node stops reporting it, and this operation would
		// otherwise sit until its deadline describing a device that is gone.
		return false, refuseDevice(
			"device %s is no longer held by the control plane, so the restart did not "+
				"bring it back", device.Name)
	case err != nil:
		return false, fmt.Errorf("read device %s back: %w", device.Name, err)
	}

	if !deviceIsInService(current.Status) {
		return false, r.note(ctx, ops, fmt.Sprintf(
			"device %s reports %q; waiting for it to come back", device.Name, current.Status))
	}
	return true, nil
}

// deviceIsInService reads the control plane's own vocabulary for a device that
// is serving.
//
// The spelling is the control plane's and is compared rather than mapped, for
// the reason status.deviceStatus keeps it: a vocabulary translated here would be
// one this wait could not express, and the set of statuses is the backend's to
// grow.
func deviceIsInService(status string) bool {
	return status == "online"
}

// target resolves the StorageDevice the operation names.
func (r *StorageDeviceOpsReconciler) target(
	ctx context.Context, ops *simplyblockv1alpha2.StorageDeviceOps,
) (*simplyblockv1alpha2.StorageDevice, error) {
	var device simplyblockv1alpha2.StorageDevice
	key := client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.DeviceRef}
	err := r.Get(ctx, key, &device)
	switch {
	case apierrors.IsNotFound(err):
		return nil, refuseDevice(
			"spec.deviceRef names StorageDevice %q and there is none by that name in %s",
			ops.Spec.DeviceRef, ops.Namespace)
	case err != nil:
		return nil, err
	}
	return &device, nil
}

// unavailableAction is what an operation naming an action this operator cannot
// perform is failed with. It names the endpoint rather than the enum, because
// the useful fact is which capability is missing.
func unavailableAction(action simplyblockv1alpha2.StorageDeviceOpsAction) string {
	for _, dependency := range simplyblockv1alpha2.ExternalDependencies() {
		if dependency.Action == action {
			return fmt.Sprintf("action %s is not available: it needs %s, which the control "+
				"plane does not offer", action, dependency.Endpoint)
		}
	}
	return fmt.Sprintf("action %q is not one this operator performs", action)
}

// deviceTimeoutMessage says what a step outliving its deadline means, which
// differs by step: one is a control plane that did not answer, the other is a
// device that did not come back.
func deviceTimeoutMessage(step deviceStep, name string) string {
	if step == stepDeviceAwaiting {
		return fmt.Sprintf("device %s did not come back within %s of being restarted",
			name, awaitingDeviceDeadline)
	}
	return fmt.Sprintf("the control plane did not accept the restart of device %s within %s",
		name, requestingDeviceDeadline)
}

// nextDeviceStep is the step that follows the current one. The graph is a line,
// so the first edge is the only edge.
func nextDeviceStep(machine *statemachine.Machine[deviceStep]) (deviceStep, bool) {
	for next := range machine.AllowedTransitions() {
		return next, true
	}
	return machine.CurrentState(), false
}

// deviceRefusal is a step's own refusal: a state the operation cannot proceed
// from, rather than an error to retry against the backoff.
//
// It carries no reason of its own. Every refusal here ends the operation, so the
// event reason is OperationFailed in each case, and a field that only ever held
// one value would suggest a choice nobody makes. A refusal that needed a reason
// of its own would be one that did something other than fail.
type deviceRefusal struct {
	message string
}

func (e *deviceRefusal) Error() string { return e.message }

func refuseDevice(format string, args ...any) error {
	return &deviceRefusal{message: fmt.Sprintf(format, args...)}
}

// begin records that the operation has started, in the step the graph begins at
// and with that step's budget.
func (r *StorageDeviceOpsReconciler) begin(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
	initial deviceStep,
) (ctrl.Result, error) {
	now := metav1.Now()
	deadline := metav1.NewTime(now.Add(initialDeviceDeadline))
	r.event(ops, corev1.EventTypeNormal, DeviceOperationStarted,
		fmt.Sprintf("restarting device %s", device.Name))

	return ctrl.Result{RequeueAfter: deviceOpsRetry}, r.writeStatus(ctx, ops,
		func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
			status.Phase = simplyblockv1alpha2.StorageDeviceOpsPhaseRunning
			status.StartedAt = &now
			status.Step.State = string(initial)
			status.Step.Deadline = &deadline
			status.Message = fmt.Sprintf("restarting device %s", device.Name)
		})
}

// enter records the step the machine has moved into, with the deadline its entry
// hook set.
func (r *StorageDeviceOpsReconciler) enter(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	next deviceStep,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
		status.Step.State = string(next)
		status.Step.Deadline = deadline
	})
}

// note records what the operation is waiting on, without moving it.
func (r *StorageDeviceOpsReconciler) note(
	ctx context.Context, ops *simplyblockv1alpha2.StorageDeviceOps, message string,
) error {
	if ops.Status.Message == message {
		return nil
	}
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
		status.Message = message
	})
}

// succeed ends an operation that reached the end of its graph, and releases the
// device.
func (r *StorageDeviceOpsReconciler) succeed(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) error {
	if err := r.releaseLock(ctx, ops, device); err != nil {
		return err
	}
	message := fmt.Sprintf("device %s was restarted and is back in service", device.Name)
	r.event(ops, corev1.EventTypeNormal, DeviceOperationSucceeded, message)

	now := metav1.Now()
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
		status.Phase = simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded
		status.CompletedAt = &now
		status.Step.Deadline = nil
		status.Message = message
	})
}

// abort stops an operation in a step that declares the edge, and releases the
// device.
func (r *StorageDeviceOpsReconciler) abort(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
	current deviceStep,
) error {
	if err := r.releaseLock(ctx, ops, device); err != nil {
		return err
	}
	message := fmt.Sprintf("aborted in step %s; no restart had been issued", current)
	r.event(ops, corev1.EventTypeNormal, DeviceOperationAborted, message)

	now := metav1.Now()
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
		status.Phase = simplyblockv1alpha2.StorageDeviceOpsPhaseAborted
		status.CompletedAt = &now
		status.Step.Deadline = nil
		status.Message = message
	})
}

// fail ends an operation with a reason, and releases whatever it holds.
//
// The release is best effort and the failure is recorded either way: an
// operation that could not let go of its device is a worse thing to hide than to
// report, and the next operation's stale-lock check is what recovers from it.
func (r *StorageDeviceOpsReconciler) fail(
	ctx context.Context, ops *simplyblockv1alpha2.StorageDeviceOps, message string,
) error {
	if device, err := r.target(ctx, ops); err == nil {
		if err := r.releaseLock(ctx, ops, device); err != nil {
			logf.FromContext(ctx).Error(err, "could not release the device lock of a failing "+
				"operation", "operation", ops.Name, "device", device.Name)
		}
	}
	r.event(ops, corev1.EventTypeWarning, DeviceOperationFailed, message)

	now := metav1.Now()
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
		status.Phase = simplyblockv1alpha2.StorageDeviceOpsPhaseFailed
		status.CompletedAt = &now
		status.Step.Deadline = nil
		status.Message = message
	})
}

// finalize releases the device and lets a deleted operation go.
func (r *StorageDeviceOpsReconciler) finalize(
	ctx context.Context, ops *simplyblockv1alpha2.StorageDeviceOps,
) error {
	if !controllerutil.ContainsFinalizer(ops, deviceOpsFinalizer) {
		return nil
	}
	if device, err := r.target(ctx, ops); err == nil {
		if err := r.releaseLock(ctx, ops, device); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(ops, deviceOpsFinalizer)
	return r.Update(ctx, ops)
}

// writeStatus applies a change to the operation's status, retrying a write that
// lost a race.
func (r *StorageDeviceOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	change func(*simplyblockv1alpha2.StorageDeviceOpsStatus),
) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageDeviceOps
		if err := r.Get(ctx, client.ObjectKeyFromObject(ops), &fresh); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		change(&fresh.Status)
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		fresh.Status.DeepCopyInto(&ops.Status)
		return nil
	})
	if err != nil {
		return fmt.Errorf("record the operation's status: %w", err)
	}
	return nil
}

// event records something about the operation, on the operation.
func (r *StorageDeviceOpsReconciler) event(
	ops *simplyblockv1alpha2.StorageDeviceOps, eventType, reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(ops, nil, eventType, reason, reason, "%s", message)
}

// SetupWithManager registers the controller.
func (r *StorageDeviceOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageDeviceOps{}).
		Named("storagedeviceops").
		Complete(r)
}
