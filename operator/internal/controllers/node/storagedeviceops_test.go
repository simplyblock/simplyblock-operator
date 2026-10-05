// What a device operation has to get right, and the three places that have to
// agree about what its steps are.
//
// The properties worth holding are the ones a retry would otherwise paper over:
// a restart is issued once and not once per pass, a device is held by one
// operation at a time, and an action the control plane cannot serve is refused
// with the reason rather than accepted and failed.

package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	deviceOpsNamespace = "sb-system"
	deviceOpsName      = "restart-nvme0"
	deviceObjectName   = "worker-1-nvme0"
	deviceBackendID    = "b2222222-2222-4222-8222-222222222222"
	deviceClusterID    = "8ffac363-0c46-4714-a71b-f9c0b58a1269"
	deviceNodeID       = "a1111111-1111-4111-8111-111111111111"
)

// everyDeviceStep is the Enum marker's list, transcribed rather than derived, so
// the assertion compares two independent statements of the same set.
var everyDeviceStep = []string{"Awaiting", "Failing", "Removing", "Requesting"}

// deviceStepCELRule is the rule as StorageDeviceOpsStatus declares it.
const deviceStepCELRule = "!has(self.state) || self.state in " +
	"['Requesting','Awaiting','Removing','Failing']"

func TestTheDeviceStepEnumCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(storageDeviceOpsGraphs())
	want := slices.Clone(everyDeviceStep)
	slices.Sort(want)
	if diff := cmp.Diff(want, declared); diff != "" {
		t.Errorf("the graph and the Enum marker disagree (-marker +graph):\n%s", diff)
	}
}

func TestTheDeviceCELRuleCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(storageDeviceOpsGraphs())
	for _, state := range declared {
		if !strings.Contains(deviceStepCELRule, "'"+state+"'") {
			t.Errorf("status.step's CEL rule does not accept the declared step %q", state)
		}
	}
}

// The enum admits exactly the actions a graph declares. An action the API
// accepts and no graph declares is an object whose first reconcile can only
// fail, which is the thing the narrowed enum exists to prevent.
func TestTheActionEnumAdmitsOnlyWhatAGraphDeclares(t *testing.T) {
	declared := storageDeviceOpsGraphs()
	for _, action := range []statemachine.Action{actionDeviceRestart, actionDeviceFail} {
		if _, ok := declared[action]; !ok {
			t.Errorf("%s is in the Enum marker and no graph declares it", action)
		}
	}
	if len(declared) != 2 {
		t.Errorf("%d graphs are declared and the Enum marker admits two actions; an action "+
			"with a graph and no enum member is one nobody can ask for", len(declared))
	}
}

// A step is abortable while it has issued nothing, and not afterward. The two
// that have issued something are Awaiting, which follows a call the control
// plane has accepted and nothing recalls, and Failing, which follows the removal
// that took the device out of the data path.
func TestOnlyTheStepsBeforeASideEffectAreAbortable(t *testing.T) {
	unabortable := UnabortableDeviceSteps()
	for _, step := range []deviceStep{stepDeviceAwaiting, stepDeviceFailing} {
		if !slices.Contains(unabortable, step) {
			t.Errorf("%s is abortable, so an abort there records a stop that did not "+
				"happen while the operation ran on anyway", step)
		}
	}
	for _, step := range []deviceStep{stepDeviceRequesting, stepDeviceRemoving} {
		if slices.Contains(unabortable, step) {
			t.Errorf("%s is not abortable, so an operation cannot be called off before "+
				"it has done anything", step)
		}
	}
}

// deviceCalls is a control plane that counts what it was asked to do and moves
// the device's status the way the real one does: a removal takes the device out
// of the data path, and a failure is accepted only once it is out.
//
// Carrying the status rather than reporting a fixed one is what lets a test say
// that a step was skipped because the work was already done, which is the whole
// of how a failure resumes after a crash.
type deviceCalls struct {
	restarts int
	removals int
	failures int

	// issued is every verb in the order it arrived, because a failure issued
	// before its removal is accepted by a counter and refused by a cluster.
	issued []string

	status  string
	missing bool

	// frozen keeps the status where it is however many calls arrive, which is
	// the control plane that accepted the work and has not published it yet.
	frozen bool

	refuse       error
	refuseRemove error
	refuseFail   error
}

func (c *deviceCalls) Device(
	context.Context, string, string, string,
) (controlplane.Device, error) {
	if c.missing {
		return controlplane.Device{}, errs.ErrNotFound
	}
	return controlplane.Device{ID: deviceBackendID, Status: c.status}, nil
}

func (c *deviceCalls) RestartDevice(context.Context, string, string, string) error {
	if c.refuse != nil {
		return c.refuse
	}
	c.restarts++
	c.issued = append(c.issued, "restart")
	return nil
}

func (c *deviceCalls) RemoveDevice(context.Context, string, string, string) error {
	if c.refuseRemove != nil {
		return c.refuseRemove
	}
	c.removals++
	c.issued = append(c.issued, "remove")
	if !c.frozen {
		c.status = cpDeviceRemoved
	}
	return nil
}

func (c *deviceCalls) FailDevice(context.Context, string, string, string) error {
	if c.refuseFail != nil {
		return c.refuseFail
	}
	c.failures++
	c.issued = append(c.issued, "fail")
	if !c.frozen {
		c.status = cpDeviceFailed
	}
	return nil
}

func deviceObject() *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{Name: deviceObjectName, Namespace: deviceOpsNamespace},
		Spec: simplyblockv1alpha2.StorageDeviceSpec{
			NodeRef: "worker-1", DeviceID: deviceBackendID,
		},
		Status: simplyblockv1alpha2.StorageDeviceStatus{
			ClusterID: deviceClusterID, NodeID: deviceNodeID, DeviceStatus: cpDeviceOnline,
		},
	}
}

func deviceOperation() *simplyblockv1alpha2.StorageDeviceOps {
	return &simplyblockv1alpha2.StorageDeviceOps{
		ObjectMeta: metav1.ObjectMeta{Name: deviceOpsName, Namespace: deviceOpsNamespace},
		Spec: simplyblockv1alpha2.StorageDeviceOpsSpec{
			DeviceRef: deviceObjectName,
			Action:    simplyblockv1alpha2.StorageDeviceOpsActionRestart,
		},
	}
}

func failOperation() *simplyblockv1alpha2.StorageDeviceOps {
	ops := deviceOperation()
	ops.Spec.Action = simplyblockv1alpha2.StorageDeviceOpsActionFail
	return ops
}

// deviceRecorder remembers the events the operation emits, which is where a
// refused abort is recorded: the status message belongs to the step the
// operation is running, because the operation runs on.
type deviceRecorder struct {
	reasons []string
}

func (r *deviceRecorder) Eventf(
	_ runtime.Object, _ runtime.Object, _, reason, _, _ string, _ ...any,
) {
	r.reasons = append(r.reasons, reason)
}

func (r *deviceRecorder) has(reason string) bool {
	return slices.Contains(r.reasons, reason)
}

type deviceWorld struct {
	t      *testing.T
	c      client.Client
	r      *StorageDeviceOpsReconciler
	api    *deviceCalls
	events *deviceRecorder
}

func newDeviceWorld(t *testing.T, api *deviceCalls, objects ...client.Object) *deviceWorld {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("registering v1alpha2: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageDeviceOps{}, &simplyblockv1alpha2.StorageDevice{}).
		Build()

	events := &deviceRecorder{}
	return &deviceWorld{t: t, c: c, api: api, events: events, r: &StorageDeviceOpsReconciler{
		Client: c, Scheme: scheme, API: api, Recorder: events,
	}}
}

// pass reconciles once and returns the operation and the device as they stand.
func (w *deviceWorld) pass() (*simplyblockv1alpha2.StorageDeviceOps, *simplyblockv1alpha2.StorageDevice) {
	w.t.Helper()

	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName},
	}); err != nil {
		w.t.Fatalf("reconcile: %v", err)
	}
	return w.read()
}

// settle reconciles until the operation is terminal or the passes run out,
// which keeps a test about the outcome rather than about how many passes the
// finalizer and the birth cost.
func (w *deviceWorld) settle() (*simplyblockv1alpha2.StorageDeviceOps, *simplyblockv1alpha2.StorageDevice) {
	w.t.Helper()

	for range 8 {
		ops, device := w.pass()
		switch ops.Status.Phase {
		case simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded,
			simplyblockv1alpha2.StorageDeviceOpsPhaseFailed,
			simplyblockv1alpha2.StorageDeviceOpsPhaseAborted:
			return ops, device
		}
	}
	return w.read()
}

func (w *deviceWorld) read() (*simplyblockv1alpha2.StorageDeviceOps, *simplyblockv1alpha2.StorageDevice) {
	w.t.Helper()

	var ops simplyblockv1alpha2.StorageDeviceOps
	if err := w.c.Get(context.Background(),
		types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName}, &ops); err != nil {
		w.t.Fatalf("read the operation: %v", err)
	}
	var device simplyblockv1alpha2.StorageDevice
	if err := w.c.Get(context.Background(),
		types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceObjectName}, &device); err != nil {
		w.t.Fatalf("read the device: %v", err)
	}
	return &ops, &device
}

// The restart is issued once. The step is persisted before the call, so
// re-entering the step means the previous pass died between the two — and a
// restart issued twice is a device recycled twice.
func TestTheRestartIsIssuedOnce(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, deviceOperation(), deviceObject())

	w.settle()

	if api.restarts != 1 {
		t.Errorf("the device was restarted %d times, want once", api.restarts)
	}
	ops, device := w.read()
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded: %s", ops.Status.Phase, ops.Status.Message)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the device is still locked by %q after the operation finished",
			device.Status.ActiveOpsRef)
	}
}

// The operation holds the device while it runs, which is the field §4.2 declared
// empty until this kind arrived.
func TestTheOperationHoldsItsDeviceWhileItRuns(t *testing.T) {
	w := newDeviceWorld(t, &deviceCalls{status: "restarting"},
		deviceOperation(), deviceObject())

	w.pass() // the finalizer
	_, device := w.pass()
	if device.Status.ActiveOpsRef != deviceOpsName {
		t.Errorf("activeOpsRef = %q, want the running operation", device.Status.ActiveOpsRef)
	}
}

// A second operation waits rather than failing, which is what lets somebody
// restart three devices of a node in sequence without reissuing anything.
func TestASecondOperationWaitsForTheFirst(t *testing.T) {
	device := deviceObject()
	device.Status.ActiveOpsRef = "someone-else"
	holder := &simplyblockv1alpha2.StorageDeviceOps{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-else", Namespace: deviceOpsNamespace},
		Spec: simplyblockv1alpha2.StorageDeviceOpsSpec{
			DeviceRef: deviceObjectName,
			Action:    simplyblockv1alpha2.StorageDeviceOpsActionRestart,
		},
		Status: simplyblockv1alpha2.StorageDeviceOpsStatus{
			Phase: simplyblockv1alpha2.StorageDeviceOpsPhaseRunning,
		},
	}

	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, deviceOperation(), holder, device)

	w.pass() // the finalizer
	ops, held := w.pass()
	if ops.Status.Phase == simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Errorf("the second operation failed rather than waiting: %s", ops.Status.Message)
	}
	if held.Status.ActiveOpsRef != "someone-else" {
		t.Errorf("the lock moved to %q while its holder was still running",
			held.Status.ActiveOpsRef)
	}
	if api.restarts != 0 {
		t.Error("a restart was issued against a device another operation is holding")
	}
}

// A device the control plane stops holding did not come back, which is a finding
// rather than a wait: the operation would otherwise sit until its deadline
// describing a device that is gone.
func TestADeviceThatDoesNotComeBackFailsTheOperation(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, deviceOperation(), deviceObject())

	w.pass() // the finalizer
	w.pass() // begin
	w.pass() // request
	api.missing = true

	ops, _ := w.settle()
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed when the device does not come back", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "no longer held") {
		t.Errorf("the failure does not say the device is gone: %q", ops.Status.Message)
	}
}

// A control plane that refuses the restart fails the operation with what it
// said, rather than retrying against the backoff forever.
func TestARefusedRestartFailsTheOperation(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, refuse: errors.New("device is busy")}
	w := newDeviceWorld(t, api, deviceOperation(), deviceObject())

	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed on a refused restart", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "device is busy") {
		t.Errorf("the failure does not carry what the control plane said: %q", ops.Status.Message)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("a failed operation is still holding the device as %q",
			device.Status.ActiveOpsRef)
	}
}

// An operation naming a device that does not exist is failed rather than
// retried, since spec.deviceRef is immutable and the reference can never become
// resolvable.
func TestAnOperationNamingNoDeviceFails(t *testing.T) {
	ops := deviceOperation()
	ops.Spec.DeviceRef = "not-a-device"
	w := newDeviceWorld(t, &deviceCalls{status: cpDeviceOnline}, ops)

	for range 3 {
		if _, err := w.r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	var got simplyblockv1alpha2.StorageDeviceOps
	if err := w.c.Get(context.Background(),
		types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName}, &got); err != nil {
		t.Fatalf("read the operation: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed for a device that does not exist", got.Status.Phase)
	}
}

// A device whose object does not say which backend device it is has nothing to
// address a restart to, and saying so beats posting to a path built from empty
// strings.
func TestADeviceWithNoBackendIdentityIsRefused(t *testing.T) {
	device := deviceObject()
	device.Status.ClusterID = ""
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, deviceOperation(), device)

	ops, _ := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if api.restarts != 0 {
		t.Error("a restart was addressed to a device with no backend identity")
	}
}

// driveTo reconciles until the operation is in the step named, so a test about
// what an abort does in one step does not have to count the passes that reach
// it.
func (w *deviceWorld) driveTo(step deviceStep) *simplyblockv1alpha2.StorageDeviceOps {
	w.t.Helper()

	for range 8 {
		ops, _ := w.pass()
		if ops.Status.Step.State == string(step) {
			return ops
		}
	}
	w.t.Fatalf("the operation never reached step %s", step)
	return nil
}

// abort asks the running operation to stop, the way a user editing the object
// does.
func (w *deviceWorld) abort() {
	w.t.Helper()

	ops, _ := w.read()
	ops.Spec.Abort = true
	if err := w.c.Update(context.Background(), ops); err != nil {
		w.t.Fatalf("asking for the abort: %v", err)
	}
}

// A failure is two calls and they are ordered. The control plane refuses to fail
// a device that is still in the data path, so a single call would be one it
// could only refuse, and a removal issued after the failure would be a device
// taken out twice.
func TestFailRemovesTheDeviceBeforeItIsFailed(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	ops, _ := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if diff := cmp.Diff([]string{"remove", "fail"}, api.issued); diff != "" {
		t.Errorf("the calls the operation issued (-want +got):\n%s", diff)
	}
}

// A device that is already failed is not failed again. The action is the record
// of a decision somebody made, and reporting that a decision was carried out
// when nothing was issued hides the likelier reading: that the operation names
// the wrong device.
func TestFailIsRefusedForADeviceThatIsAlreadyFailed(t *testing.T) {
	api := &deviceCalls{status: cpDeviceFailed}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Errorf("phase = %s, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "failed") {
		t.Errorf("message = %q, want the device's current status in it", ops.Status.Message)
	}
	if api.removals != 0 || api.failures != 0 {
		t.Errorf("%d removals and %d failures were issued against a device that is already "+
			"failed, want none", api.removals, api.failures)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the refused operation still holds device %s", device.Name)
	}
}

// The removal is skipped for a device the control plane already holds as
// removed, which is both the operator resuming after a crash between the call
// and its record, and a device somebody removed by hand.
func TestFailSkipsTheRemovalOfADeviceAlreadyOutOfTheDataPath(t *testing.T) {
	api := &deviceCalls{status: cpDeviceRemoved}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	ops, _ := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if diff := cmp.Diff([]string{"fail"}, api.issued); diff != "" {
		t.Errorf("the calls the operation issued (-want +got):\n%s", diff)
	}
}

// The operation waits for the control plane to report the failure rather than
// treating its own call as the outcome. A wait that ended at the call would
// report a device failed on the strength of having asked.
func TestFailWaitsForTheControlPlaneToReportTheDeviceFailed(t *testing.T) {
	api := &deviceCalls{status: cpDeviceRemoved, frozen: true}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	ops, _ := w.settle()
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseRunning {
		t.Fatalf("phase = %s, want it still Running while the status has not moved",
			ops.Status.Phase)
	}
	if ops.Status.Step.State != string(stepDeviceAwaiting) {
		t.Errorf("step = %s, want Awaiting", ops.Status.Step.State)
	}

	api.status = cpDeviceFailed
	if ops, _ = w.settle(); ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded once the device reports failed",
			ops.Status.Phase, ops.Status.Message)
	}
}

// A rebuild that has already finished satisfies the wait. The control plane
// moves a failed device on to failed_and_migrated by itself, and an operation
// that only accepted the first spelling would time out on the device having got
// further than it asked for.
func TestFailAcceptsADeviceWhoseRebuildHasFinished(t *testing.T) {
	api := &deviceCalls{status: cpDeviceRemoved, frozen: true}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	w.driveTo(stepDeviceAwaiting)
	api.status = cpDeviceFailedAndMigrated

	if ops, _ := w.settle(); ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
}

// An abort before the removal stops the operation, because nothing has been
// issued.
func TestAnAbortBeforeTheRemovalStopsTheFailure(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	w.driveTo(stepDeviceRemoving)
	w.abort()
	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseAborted {
		t.Errorf("phase = %s (%s), want Aborted", ops.Status.Phase, ops.Status.Message)
	}
	if len(api.issued) != 0 {
		t.Errorf("the aborted operation issued %v, want nothing", api.issued)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the aborted operation still holds device %s", device.Name)
	}
}

// An abort after the removal is refused, because the device is out of the data
// path by then and this operator has no call that puts it back. Recording a stop
// would leave a device removed under an object that says nothing happened.
//
// **The refusal does not halt the operation, which is the half that matters.** A
// refused abort that also stopped the operation from stepping would strand the
// device in exactly the state the refusal exists to avoid: removed from the data
// path, never failed, and never rebuilt from.
func TestAnAbortAfterTheRemovalIsRefusedAndTheFailureFinishes(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	w.driveTo(stepDeviceFailing)
	w.abort()
	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded: a refused abort does not stop the "+
			"operation, and a device left removed and never failed is the state the "+
			"refusal exists to avoid", ops.Status.Phase, ops.Status.Message)
	}
	if api.removals != 1 || api.failures != 1 {
		t.Errorf("%d removals and %d failures were issued, want one of each",
			api.removals, api.failures)
	}
	if !w.events.has(DeviceAbortRefused) {
		t.Errorf("no %s event; the refusal is the operation's answer to what was asked "+
			"for, and the status message belongs to the step it is running",
			DeviceAbortRefused)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the finished operation still holds device %s", device.Name)
	}
}

// A refused removal fails the operation and issues no failure. The control plane
// refuses while a volume migration is running anywhere in the cluster, and a
// failure issued past that refusal would act on a device the cluster is still
// moving data onto.
func TestARefusedRemovalStopsTheFailure(t *testing.T) {
	api := &deviceCalls{
		status:       cpDeviceOnline,
		refuseRemove: errors.New("lvol migration tasks found on node a1111111"),
	}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Errorf("phase = %s, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "lvol migration") {
		t.Errorf("message = %q, want the control plane's own refusal in it", ops.Status.Message)
	}
	if api.failures != 0 {
		t.Errorf("%d failures were issued after the removal was refused, want none",
			api.failures)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the failed operation still holds device %s", device.Name)
	}
}

// A device that came back into service between the removal and the failure is
// refused rather than failed. Self-repair and a hand-issued restart both put a
// removed device back, and a failure issued against a serving device is one the
// control plane refuses. The operation reads the device again rather than
// trusting the step it is in.
func TestFailRefusesToFailADeviceThatCameBackIntoService(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	w.driveTo(stepDeviceFailing)
	api.status = cpDeviceOnline
	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Errorf("phase = %s (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if api.failures != 0 {
		t.Errorf("%d failures were issued against a device back in service, want none",
			api.failures)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the refused operation still holds device %s", device.Name)
	}
}

// An abort is refused once the device is out of the data path, even in the step
// that declares the edge. The removal is issued inside Removing, so a pass that
// died between the call and the transition leaves the operation in an abortable
// step with the device already removed, and recording a stop over that would
// leave the device out of the data path under an object that says nothing
// happened.
func TestAnAbortIsRefusedOnceTheDeviceIsAlreadyOutOfTheDataPath(t *testing.T) {
	api := &deviceCalls{status: cpDeviceRemoved}
	w := newDeviceWorld(t, api, failOperation(), deviceObject())

	w.driveTo(stepDeviceRemoving)
	w.abort()
	ops, _ := w.settle()

	if ops.Status.Phase == simplyblockv1alpha2.StorageDeviceOpsPhaseAborted {
		t.Fatal("the operation was aborted while its device was already removed, so the " +
			"device is out of the data path under an object that says nothing happened")
	}
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded: the refusal does not stop the operation",
			ops.Status.Phase, ops.Status.Message)
	}
	if api.failures != 1 {
		t.Errorf("%d failures were issued, want one", api.failures)
	}
	if !w.events.has(DeviceAbortRefused) {
		t.Errorf("no %s event; nothing records that the abort was refused", DeviceAbortRefused)
	}
}

// The same for a restart, where the step the graph refuses is the wait. The
// control plane has accepted the restart and nothing recalls one, so an
// operation halted here would stop watching a device that is restarting anyway
// and would hold its lock until somebody noticed.
func TestARefusedAbortLetsTheRestartFinish(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, frozen: true}
	w := newDeviceWorld(t, api, deviceOperation(), deviceObject())

	w.driveTo(stepDeviceAwaiting)
	w.abort()
	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Errorf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if !w.events.has(DeviceAbortRefused) {
		t.Errorf("no %s event; nothing records that the abort was refused",
			DeviceAbortRefused)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the finished operation still holds device %s", device.Name)
	}
}
