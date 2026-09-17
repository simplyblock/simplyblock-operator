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
var everyDeviceStep = []string{"Awaiting", "Requesting"}

// deviceStepCELRule is the rule as StorageDeviceOpsStatus declares it.
const deviceStepCELRule = "!has(self.state) || self.state in ['Requesting','Awaiting']"

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
	if _, ok := declared[actionDeviceRestart]; !ok {
		t.Error("Restart is in the Enum marker and no graph declares it")
	}
	if len(declared) != 1 {
		t.Errorf("%d graphs are declared and the Enum marker admits one action; an action "+
			"with a graph and no enum member is one nobody can ask for", len(declared))
	}
}

// Every blocked action has a dependency row naming the endpoint it waits on, and
// no row names an action that is already available. The list is the ask of
// another team, so an action that shipped and left its row behind would be an
// ask for something that exists.
func TestEveryBlockedActionNamesTheEndpointItWaitsOn(t *testing.T) {
	blocked := map[simplyblockv1alpha2.StorageDeviceOpsAction]bool{
		simplyblockv1alpha2.StorageDeviceOpsActionSelfTest: true,
		simplyblockv1alpha2.StorageDeviceOpsActionFail:     true,
		simplyblockv1alpha2.StorageDeviceOpsActionReplace:  true,
		simplyblockv1alpha2.StorageDeviceOpsActionMigrate:  true,
	}

	for _, dependency := range simplyblockv1alpha2.ExternalDependencies() {
		if !blocked[dependency.Action] {
			t.Errorf("%s has a dependency row and is not blocked", dependency.Action)
		}
		delete(blocked, dependency.Action)
		if dependency.Endpoint == "" || dependency.Because == "" {
			t.Errorf("%s's row names no endpoint or no reason, so it is not an ask "+
				"anybody can act on", dependency.Action)
		}
		if _, declared := storageDeviceOpsGraphs()[statemachine.Action(dependency.Action)]; declared {
			t.Errorf("%s is declared as blocked and has a graph", dependency.Action)
		}
	}
	for action := range blocked {
		t.Errorf("%s is specified by §6 and has neither a graph nor a dependency row, so "+
			"nothing records why it is missing", action)
	}
}

// Awaiting cannot be aborted, because the control plane has accepted the restart
// and nothing recalls one. Requesting can, because the step is persisted before
// the call.
func TestOnlyTheStepBeforeTheCallIsAbortable(t *testing.T) {
	unabortable := UnabortableDeviceSteps()
	if !slices.Contains(unabortable, stepDeviceAwaiting) {
		t.Error("Awaiting is abortable, so an abort there records a stop that did not " +
			"happen while the device restarts anyway")
	}
	if slices.Contains(unabortable, stepDeviceRequesting) {
		t.Error("Requesting is not abortable, so an operation cannot be called off before " +
			"it has done anything")
	}
}

// restartCounter is a control plane that counts restarts and reports whichever
// status it is told to.
type restartCounter struct {
	restarts int
	status   string
	missing  bool
	refuse   error
}

func (c *restartCounter) Device(
	context.Context, string, string, string,
) (controlplane.Device, error) {
	if c.missing {
		return controlplane.Device{}, errs.ErrNotFound
	}
	return controlplane.Device{ID: deviceBackendID, Status: c.status}, nil
}

func (c *restartCounter) RestartDevice(context.Context, string, string, string) error {
	if c.refuse != nil {
		return c.refuse
	}
	c.restarts++
	return nil
}

func deviceObject() *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{Name: deviceObjectName, Namespace: deviceOpsNamespace},
		Spec: simplyblockv1alpha2.StorageDeviceSpec{
			NodeRef: "worker-1", DeviceID: deviceBackendID,
		},
		Status: simplyblockv1alpha2.StorageDeviceStatus{
			ClusterID: deviceClusterID, NodeID: deviceNodeID, DeviceStatus: "online",
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

type deviceWorld struct {
	t   *testing.T
	c   client.Client
	r   *StorageDeviceOpsReconciler
	api *restartCounter
}

func newDeviceWorld(t *testing.T, api *restartCounter, objects ...client.Object) *deviceWorld {
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

	return &deviceWorld{t: t, c: c, api: api, r: &StorageDeviceOpsReconciler{
		Client: c, Scheme: scheme, API: api,
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
	api := &restartCounter{status: "online"}
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
	w := newDeviceWorld(t, &restartCounter{status: "restarting"},
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

	api := &restartCounter{status: "online"}
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
	api := &restartCounter{status: "online"}
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
	api := &restartCounter{status: "online", refuse: errors.New("device is busy")}
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
	w := newDeviceWorld(t, &restartCounter{status: "online"}, ops)

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

// An action no graph declares is failed with the endpoint it waits on, so the
// person who asked learns which capability is missing rather than that a value
// was rejected.
func TestAnUnavailableActionNamesWhatItWaitsOn(t *testing.T) {
	for _, action := range []simplyblockv1alpha2.StorageDeviceOpsAction{
		simplyblockv1alpha2.StorageDeviceOpsActionSelfTest,
		simplyblockv1alpha2.StorageDeviceOpsActionFail,
		simplyblockv1alpha2.StorageDeviceOpsActionReplace,
		simplyblockv1alpha2.StorageDeviceOpsActionMigrate,
	} {
		t.Run(string(action), func(t *testing.T) {
			got := unavailableAction(action)
			if !strings.Contains(got, "/api/v2/") {
				t.Errorf("the refusal names no endpoint: %q", got)
			}
			if !strings.Contains(got, string(action)) {
				t.Errorf("the refusal does not name the action: %q", got)
			}
		})
	}
}

// A device whose object does not say which backend device it is has nothing to
// address a restart to, and saying so beats posting to a path built from empty
// strings.
func TestADeviceWithNoBackendIdentityIsRefused(t *testing.T) {
	device := deviceObject()
	device.Status.ClusterID = ""
	api := &restartCounter{status: "online"}
	w := newDeviceWorld(t, api, deviceOperation(), device)

	ops, _ := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if api.restarts != 0 {
		t.Error("a restart was addressed to a device with no backend identity")
	}
}
