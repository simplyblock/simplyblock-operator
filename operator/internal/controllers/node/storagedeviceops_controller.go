// The reconciler for StorageDeviceOps, which today means two actions: restarting
// one device rather than the storage node it sits in, and failing one device so
// the cluster stops trusting it.
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
// design-storagedevice.md §6 is the specification, and §6's other three actions
// are blocked on control-plane verbs that do not exist — the TODO beside their
// constants in storagedeviceops_types.go names each one.

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
	"github.com/simplyblock/simplyblock-operator/internal/controllers/stepclaim"
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
	DeviceRemovalRequested   = "DeviceRemovalRequested"
	DeviceFailRequested      = "DeviceFailRequested"
	DeviceAbortRefused       = "AbortRefused"
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

	// RemoveDevice takes one device out of the data path, leaving it in its
	// slot. It is the first of the two calls a failure is made of.
	RemoveDevice(ctx context.Context, clusterID, nodeID, deviceID string) error

	// FailDevice declares one removed device untrustworthy, so the cluster
	// rebuilds the redundancy it held elsewhere.
	FailDevice(ctx context.Context, clusterID, nodeID, deviceID string) error
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
		// reaching here means an older CRD served the object. What a user needs
		// to know is that the action is not available yet rather than that a
		// value was rejected; which capability it waits on is the TODO beside
		// the action's constant.
		return ctrl.Result{}, r.fail(ctx, ops, fmt.Sprintf(
			"action %q is not one this operator performs; it waits on a control-plane "+
				"capability the v2 API does not offer", ops.Spec.Action))
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
		blocked, err := r.abortBlocked(ctx, ops, device, machine.CanAbort())
		if err != nil {
			return ctrl.Result{}, err
		}
		if blocked == "" {
			return ctrl.Result{}, r.abort(ctx, ops, device, current)
		}
		// Refused, and the operation runs on rather than stopping here. An
		// operation halted by a refused abort would be stranded in the step it
		// was refused in: a failure would leave the device out of the data path
		// and never failed, which is the state the refusal exists to avoid, and
		// either action would hold the device's lock until somebody noticed.
		//
		// The refusal is an event rather than the status message, because the
		// message belongs to the step the operation is still running and two
		// writers would alternate it every pass. Repeating the event is what the
		// recorder aggregates.
		r.event(ops, corev1.EventTypeWarning, DeviceAbortRefused,
			fmt.Sprintf("the abort asked for in step %s was refused: %s", current, blocked))
	}

	if machine.TimeoutReached() {
		expired := deviceTimeoutMessage(ops.Spec.Action, current, device.Name)
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

// abortBlocked is why an abort cannot be honored, or the empty string when it
// can.
//
// The graph answers for the step, and a failure asks one question more. Its
// removal is issued inside the step that declares the abort edge, so a pass that
// died between the call and the transition leaves the operation in an abortable
// step with the device already out of the data path. Reading the device is what
// tells that apart from a step that has issued nothing, and this operator has no
// call that puts a removed device back.
func (r *StorageDeviceOpsReconciler) abortBlocked(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
	graphAllows bool,
) (string, error) {
	if !graphAllows {
		return deviceAbortRefusal(ops.Spec.Action), nil
	}
	if ops.Spec.Action != simplyblockv1alpha2.StorageDeviceOpsActionFail {
		return "", nil
	}

	cluster, node, id, err := deviceAddress(device)
	if err != nil {
		// A device with no backend identity is one nothing was issued against,
		// because every step refuses it first.
		return "", nil
	}
	current, err := r.API.Device(ctx, cluster, node, id)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		// A device the control plane no longer holds is one no unwind can
		// reach, so there is nothing for the abort to strand.
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read device %s to decide the abort: %w", device.Name, err)
	}

	if deviceIsInService(current.Status) {
		return "", nil
	}
	return fmt.Sprintf("device %s already reports %q, so the removal has happened and "+
		"this operator has no call that puts it back", device.Name, current.Status), nil
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
	case stepDeviceRemoving:
		return r.removeFromDataPath(ctx, ops, device)
	case stepDeviceFailing:
		return r.requestFailure(ctx, ops, device)
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

	cluster, node, id, err := deviceAddress(device)
	if err != nil {
		return false, err
	}

	// The record travels in the claim's patch rather than in a write of its
	// own. A write that rereads and retries on a conflict lands for a pass
	// holding a stale copy as well, and that pass would then restart the
	// device a second time.
	before := device.Status.DeviceStatus
	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.RestartDevice(ctx, cluster, node, id); err != nil {
			return refuseDevice(
				"the control plane refused to restart device %s: %v", device.Name, err)
		}
		return nil
	}, func() { ops.Status.DeviceStatusBefore = before })
	if err != nil || !claimed {
		return false, err
	}
	r.event(ops, corev1.EventTypeNormal, DeviceRestartRequested,
		fmt.Sprintf("asked the control plane to restart device %s", device.Name))
	return true, nil
}

// deviceAddress is the three identifiers every device call carries, or the
// refusal for an object that does not report them.
func deviceAddress(
	device *simplyblockv1alpha2.StorageDevice,
) (cluster, node, id string, err error) {
	cluster, node, id = device.Status.ClusterID, device.Status.NodeID, device.Spec.DeviceID
	if cluster == "" || node == "" || id == "" {
		return "", "", "", refuseDevice(
			"device %s does not report which cluster, node, and device it is, so there is "+
				"nothing to address the operation to", device.Name)
	}
	return cluster, node, id, nil
}

// removeFromDataPath takes the device out of the data path, which is the first
// of the two calls a failure is made of: the control plane refuses to fail a
// device that is still serving.
//
// What it decides from is the status the control plane reports rather than a
// record of its own, which is what makes the step resumable. A pass that died
// between the call and its record finds the device already removed and issues
// nothing, where a written record would have been the thing that did not
// survive.
func (r *StorageDeviceOpsReconciler) removeFromDataPath(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (bool, error) {
	cluster, node, id, err := deviceAddress(device)
	if err != nil {
		return false, err
	}

	current, err := r.API.Device(ctx, cluster, node, id)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return false, refuseDevice(
			"device %s is no longer held by the control plane, so there is nothing to fail",
			device.Name)
	case err != nil:
		return false, fmt.Errorf("read device %s before removing it: %w", device.Name, err)
	}

	// A device that is already failed is not failed again. The action records a
	// decision somebody made, and reporting that the decision was carried out
	// when nothing was issued hides the likelier reading: that the operation
	// names a device somebody else has already dealt with.
	if deviceIsFailed(current.Status) {
		return false, refuseDevice(
			"device %s already reports %q, so the failure this operation asks for has "+
				"already happened", device.Name, current.Status)
	}

	if current.Status == cpDeviceRemoved {
		// Out of the data path already, by an earlier pass of this step or by
		// somebody's hand. Issuing the removal again is a call the control plane
		// refuses, and the step's work is done either way.
		return true, r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
			status.DeviceStatusBefore = current.Status
		})
	}

	// The status before travels in the claim's patch, so that no write that
	// rereads on a conflict runs between this pass choosing the step and
	// claiming it.
	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.RemoveDevice(ctx, cluster, node, id); err != nil {
			return refuseDevice(
				"the control plane refused to remove device %s from the data path: %v",
				device.Name, err)
		}
		return nil
	}, func() { ops.Status.DeviceStatusBefore = current.Status })
	if err != nil || !claimed {
		return false, err
	}
	r.event(ops, corev1.EventTypeNormal, DeviceRemovalRequested,
		fmt.Sprintf("took device %s out of the data path, before failing it", device.Name))
	return true, nil
}

// requestFailure declares the removed device untrustworthy, which is the second
// of the two calls and the irreversible one.
func (r *StorageDeviceOpsReconciler) requestFailure(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (bool, error) {
	cluster, node, id, err := deviceAddress(device)
	if err != nil {
		return false, err
	}

	current, err := r.API.Device(ctx, cluster, node, id)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return false, refuseDevice(
			"device %s is no longer held by the control plane, so the failure cannot be "+
				"issued against it", device.Name)
	case err != nil:
		return false, fmt.Errorf("read device %s before failing it: %w", device.Name, err)
	}

	if deviceIsFailed(current.Status) {
		// The call landed on a pass that died before recording it.
		return true, nil
	}
	if current.Status != cpDeviceRemoved {
		return false, refuseDevice(
			"device %s reports %q, where the control plane fails only a device it already "+
				"holds as removed", device.Name, current.Status)
	}

	claimed, err := r.once(ctx, ops, func() error {
		if err := r.API.FailDevice(ctx, cluster, node, id); err != nil {
			return refuseDevice(
				"the control plane refused to fail device %s: %v", device.Name, err)
		}
		return nil
	})
	if err != nil || !claimed {
		return false, err
	}
	r.event(ops, corev1.EventTypeNormal, DeviceFailRequested,
		fmt.Sprintf("asked the control plane to fail device %s", device.Name))
	return true, nil
}

// await waits for the control plane to report the device back in service.
//
// What it waits for is the status the device reports rather than an absence:
// a restart takes the device out and brings it back, and a check that only
// looked for it being gone would finish on the way down.
// What it waits for is the status the action asked for, which differs by action:
// a restart ends in service and a failure ends out of it.
func (r *StorageDeviceOpsReconciler) await(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageDeviceOps,
	device *simplyblockv1alpha2.StorageDevice,
) (bool, error) {
	cluster, node, id, err := deviceAddress(device)
	if err != nil {
		return false, err
	}

	current, err := r.API.Device(ctx, cluster, node, id)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		// A device the control plane has stopped holding did not reach what the
		// action asked for, which is a finding rather than a wait: §5.2 deletes
		// the object when the node stops reporting it, and this operation would
		// otherwise sit until its deadline describing a device that is gone.
		return false, refuseDevice(
			"device %s is no longer held by the control plane, so %s",
			device.Name, deviceLostDuringWait(ops.Spec.Action))
	case err != nil:
		return false, fmt.Errorf("read device %s back: %w", device.Name, err)
	}

	if !deviceReachedItsOutcome(ops.Spec.Action, current.Status) {
		return false, r.note(ctx, ops, fmt.Sprintf(
			"device %s reports %q; waiting for it to reach what the %s asked for",
			device.Name, current.Status, ops.Spec.Action))
	}
	return true, nil
}

// deviceReachedItsOutcome reports whether the device is where the action asked it
// to end up.
//
// The spellings it compares are the control plane's own and are the ones the
// mirror already names in storagedevice_controller.go. They are compared rather
// than mapped, for the reason status.deviceStatus keeps them: a vocabulary
// translated here would be one these waits could not express, and the set of
// statuses is the backend's to grow.
func deviceReachedItsOutcome(
	act simplyblockv1alpha2.StorageDeviceOpsAction, status string,
) bool {
	if act == simplyblockv1alpha2.StorageDeviceOpsActionFail {
		return deviceIsFailed(status)
	}
	return deviceIsInService(status)
}

// deviceIsInService reads the control plane's own vocabulary for a device that
// is serving.
func deviceIsInService(status string) bool {
	return status == cpDeviceOnline
}

// deviceIsFailed accepts both spellings of a device that has been failed.
//
// The rebuild the failure starts moves the device from the first to the second
// by itself, and a wait that accepted only the first would time out on a device
// that had got further than the operation asked for.
func deviceIsFailed(status string) bool {
	return status == cpDeviceFailed || status == cpDeviceFailedAndMigrated
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

// deviceTimeoutMessage says what a step outliving its deadline means, which
// differs by step: a call the control plane did not answer, or a device that did
// not reach the state the call asked for.
func deviceTimeoutMessage(
	act simplyblockv1alpha2.StorageDeviceOpsAction, step deviceStep, name string,
) string {
	switch step {
	case stepDeviceRemoving:
		return fmt.Sprintf("the control plane did not take device %s out of the data path "+
			"within %s", name, removingDeviceDeadline)
	case stepDeviceFailing:
		return fmt.Sprintf("the control plane did not accept the failure of device %s "+
			"within %s", name, requestingDeviceDeadline)
	case stepDeviceAwaiting:
		if act == simplyblockv1alpha2.StorageDeviceOpsActionFail {
			return fmt.Sprintf("device %s was not reported failed within %s of the control "+
				"plane accepting it", name, awaitingFailureDeadline)
		}
		return fmt.Sprintf("device %s did not come back within %s of being restarted",
			name, awaitingDeviceDeadline)
	default:
		return fmt.Sprintf("the control plane did not accept the restart of device %s "+
			"within %s", name, requestingDeviceDeadline)
	}
}

// deviceNarration is what an action calls itself, so the status message and the
// events read as the operation rather than as the one action this kind began
// with.
type deviceNarration struct {
	starting  string
	succeeded string
}

func narrateDevice(
	act simplyblockv1alpha2.StorageDeviceOpsAction, name string,
) deviceNarration {
	switch act {
	case simplyblockv1alpha2.StorageDeviceOpsActionRestart:
		return deviceNarration{
			starting:  fmt.Sprintf("restarting device %s", name),
			succeeded: fmt.Sprintf("device %s was restarted and is back in service", name),
		}
	case simplyblockv1alpha2.StorageDeviceOpsActionFail:
		return deviceNarration{
			starting: fmt.Sprintf("failing device %s", name),
			succeeded: fmt.Sprintf("device %s was failed; the cluster is rebuilding the "+
				"redundancy it held and has stopped reading from it", name),
		}
	default:
		return deviceNarration{
			starting:  fmt.Sprintf("running %s on device %s", act, name),
			succeeded: fmt.Sprintf("%s finished on device %s", act, name),
		}
	}
}

// deviceAbortRefusal says what an unabortable step has already done, which is
// the half of the refusal a user can act on.
func deviceAbortRefusal(act simplyblockv1alpha2.StorageDeviceOpsAction) string {
	if act == simplyblockv1alpha2.StorageDeviceOpsActionFail {
		return "the device is out of the data path and this operator has no call that " +
			"puts it back"
	}
	return "the restart has been issued and nothing recalls one"
}

// deviceLostDuringWait says what a device vanishing mid-wait means for the
// action that was waiting.
func deviceLostDuringWait(act simplyblockv1alpha2.StorageDeviceOpsAction) string {
	if act == simplyblockv1alpha2.StorageDeviceOpsActionFail {
		return "the failure it was asked to confirm cannot be"
	}
	return "the restart did not bring it back"
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
	budget, declared := initialDeviceDeadlines[statemachine.Action(ops.Spec.Action)]
	if !declared {
		budget = requestingDeviceDeadline
	}
	now := metav1.Now()
	deadline := metav1.NewTime(now.Add(budget))
	words := narrateDevice(ops.Spec.Action, device.Name)
	r.event(ops, corev1.EventTypeNormal, DeviceOperationStarted, words.starting)

	return ctrl.Result{RequeueAfter: deviceOpsRetry}, r.writeStatus(ctx, ops,
		func(status *simplyblockv1alpha2.StorageDeviceOpsStatus) {
			status.Phase = simplyblockv1alpha2.StorageDeviceOpsPhaseRunning
			status.StartedAt = &now
			status.Step.State = string(initial)
			status.Step.Deadline = &deadline
			status.Message = words.starting
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
	message := narrateDevice(ops.Spec.Action, device.Name).succeeded
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
	message := fmt.Sprintf("aborted in step %s; nothing had been issued", current)
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
	// The first attempt starts from the caller's object rather than from a
	// read. That object carries every write this pass made, including a claim
	// on the step, which the cache may not have seen yet. Read from the cache,
	// it would patch against the version before the claim and conflict until
	// the cache caught up. Only a conflict means somebody else wrote, and only
	// then does an attempt read the object again, and so does the first one
	// when the caller's object was never read and carries no version to patch
	// against.
	reread := ops.ResourceVersion == ""
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := *ops.DeepCopy()
		if reread {
			if err := r.Get(ctx, client.ObjectKeyFromObject(ops), &fresh); err != nil {
				return err
			}
		}
		reread = true
		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		change(&fresh.Status)
		fresh.Status.ObservedGeneration = fresh.Generation
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		// The version travels back with the status, because the next write in
		// this pass starts from ops and patches against it.
		fresh.Status.DeepCopyInto(&ops.Status)
		ops.ResourceVersion = fresh.ResourceVersion
		return nil
	})
	if err != nil {
		return fmt.Errorf("record the operation's status: %w", err)
	}
	return nil
}

// once makes call under a claim on the operation's current step, applies also
// in the claim's own patch, and reports whether this pass made the call. A pass
// that loses the claim made no call: either another pass holds a live claim on
// the step, or this pass read the operation at a version a newer write has
// replaced. Either way it waits, and the next pass reads again.
func (r *StorageDeviceOpsReconciler) once(
	ctx context.Context, ops *simplyblockv1alpha2.StorageDeviceOps,
	call func() error, also ...func(),
) (bool, error) {
	return statemachine.WithClaim(ctx, ops.Status.Step, claimLease,
		stepclaim.Writer(r.Client, ops, &ops.Status.Step, also...), call)
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
