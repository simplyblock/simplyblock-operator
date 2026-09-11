// Tests for what the mirror publishes about a device beyond its identity: the
// labels that make the kind usable in an incident, the message that says why the
// phase is what it is, the events it announces a change with, and the Unknown
// rule that keeps an unreachable node's devices rather than deleting them.
//
// They live beside storagedevice_controller_unit_test.go rather than in it
// because that file is about the mirror's create, update, and delete paths,
// which are the same three whatever the device turns out to be.

package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// sdNodeSet is the StorageNodeSet the test node belongs to. The cluster label's
// value is only reachable through it: a StorageNode names its set, and the set
// names the cluster.
func sdNodeSet() *simplyblockv1alpha1.StorageNodeSet {
	return &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: "production-set"},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: "production"},
	}
}

// sdNodeWithStatus is the owning node in a given control-plane state, wired to
// its set and carrying the worker label the device copies.
func sdNodeWithStatus(status string) *simplyblockv1alpha1.StorageNode {
	node := sdNodeObject()
	node.Labels = map[string]string{simplyblockv1alpha2.DeviceLabelWorker: "worker-3"}
	node.Spec.StorageNodeSetRef = "production-set"
	node.Status.Status = status
	return node
}

// existingDevice is a mirror object already in the phase given, as a previous
// reconcile would have left it.
func existingDevice(phase simplyblockv1alpha2.StorageDevicePhase, cpStatus string) *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: sdName()},
		Spec:       simplyblockv1alpha2.StorageDeviceSpec{NodeRef: sdNodeCR, DeviceID: sdDevice},
		Status: simplyblockv1alpha2.StorageDeviceStatus{
			Phase: phase, DeviceStatus: cpStatus,
			ClusterID: sdCluster, NodeID: sdNodeID,
		},
	}
}

// drainReasons returns every event reason the recorder holds, so a test can
// assert both what was announced and what was not.
func drainReasons(rec *events.FakeRecorder) []string {
	var reasons []string
	for {
		select {
		case e := <-rec.Events:
			reasons = append(reasons, e)
		default:
			return reasons
		}
	}
}

func announced(rec *events.FakeRecorder, reason string) bool {
	for _, e := range drainReasons(rec) {
		if strings.Contains(e, reason) {
			return true
		}
	}
	return false
}

// The three labels of design §4.3. Somebody holding a failed drive knows the
// machine they pulled it from and nothing else, so the worker label is the one
// that has to be there.
func TestTheMirrorLabelsADeviceWithItsClusterNodeAndWorker(t *testing.T) {
	r := sdReconciler(t, sdOnlineCache(), sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	want := map[string]string{
		simplyblockv1alpha2.DeviceLabelCluster: "production",
		simplyblockv1alpha2.DeviceLabelNode:    sdNodeCR,
		simplyblockv1alpha2.DeviceLabelWorker:  "worker-3",
	}
	for key, value := range want {
		if got := sd.Labels[key]; got != value {
			t.Errorf("label %s = %q, want %q", key, got, value)
		}
	}
}

// A label that was stripped by hand is put back: the labels are the mirror's
// output, not a user's annotation of it.
func TestTheMirrorRestoresALabelSomebodyRemoved(t *testing.T) {
	stripped := existingDevice(simplyblockv1alpha2.StorageDevicePhaseOnline, "online")
	stripped.Labels = map[string]string{"unrelated": "kept"}

	r := sdReconciler(t, sdOnlineCache(), sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet(), stripped)
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sd.Labels[simplyblockv1alpha2.DeviceLabelWorker] != "worker-3" {
		t.Errorf("worker label not restored: %v", sd.Labels)
	}
	if sd.Labels["unrelated"] != "kept" {
		t.Errorf("a label the mirror does not own was dropped: %v", sd.Labels)
	}
}

// status.message is the reason the phase is what it is. An operator that
// publishes Degraded and no reason has told nobody anything.
func TestTheMirrorSaysWhyTheDeviceIsDegraded(t *testing.T) {
	cache := sdOnlineCache()
	dto := cache.devices[sdName()]
	dto.IOError = true
	cache.devices[sdName()] = dto

	r := sdReconciler(t, cache, sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sd.Status.Phase != simplyblockv1alpha2.StorageDevicePhaseDegraded {
		t.Fatalf("phase = %q", sd.Status.Phase)
	}
	if sd.Status.Message == "" {
		t.Fatal("a degraded device published no message")
	}
	// The message has to name the signal, or it is decoration: three different
	// things make a serving device degraded and they are fixed differently.
	if !strings.Contains(strings.ToLower(sd.Status.Message), "i/o error") {
		t.Errorf("message does not name the signal: %q", sd.Status.Message)
	}
}

func TestTheMirrorSaysWhyTheDeviceIsUnrecognized(t *testing.T) {
	cache := sdOnlineCache()
	dto := cache.devices[sdName()]
	dto.Status = "something-new"
	cache.devices[sdName()] = dto

	r := sdReconciler(t, cache, sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(sd.Status.Message, "something-new") {
		t.Errorf("an unrecognized status must be quoted back: %q", sd.Status.Message)
	}
}

// A device found for the first time is announced on the node, because at that
// moment the device's own object is being created and an event on it is an event
// nobody reads (design §8.1).
func TestDiscoveryIsAnnouncedOnTheNode(t *testing.T) {
	r := sdReconciler(t, sdOnlineCache(), sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !announced(r.Recorder.(*events.FakeRecorder), "DeviceDiscovered") {
		t.Error("no DeviceDiscovered event")
	}
}

// A phase change is announced once, on the change. A reconcile that finds the
// device where it left it announces nothing, or the event stream is a poll log.
func TestAPhaseChangeIsAnnouncedOnceOnTheDevice(t *testing.T) {
	cache := sdOnlineCache()
	dto := cache.devices[sdName()]
	dto.Status = cpDeviceFailed
	cache.devices[sdName()] = dto

	r := sdReconciler(t, cache,
		sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet(),
		existingDevice(simplyblockv1alpha2.StorageDevicePhaseOnline, "online"))

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec := r.Recorder.(*events.FakeRecorder)
	if !announced(rec, "DeviceFailed") {
		t.Error("a device that failed was not announced")
	}

	// Nothing moved on the second pass, so nothing is announced.
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reasons := drainReasons(rec); len(reasons) != 0 {
		t.Errorf("a settled device announced %v", reasons)
	}
}

func TestRecoveryIsAnnouncedButDiscoveryIsNot(t *testing.T) {
	r := sdReconciler(t, sdOnlineCache(),
		sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet(),
		existingDevice(simplyblockv1alpha2.StorageDevicePhaseDegraded, "read_only"))

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !announced(r.Recorder.(*events.FakeRecorder), "DeviceOnline") {
		t.Error("a device that recovered was not announced")
	}

	// A device that was Online the first time anybody looked did not recover.
	fresh := sdReconciler(t, sdOnlineCache(), sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())
	if _, err := fresh.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if announced(fresh.Recorder.(*events.FakeRecorder), "DeviceOnline") {
		t.Error("a newly discovered device must not be announced as recovered")
	}
}

// Design §5.2: an offline node reports no devices, which is not the same
// statement as a node reporting that a device is gone. The first is an absence
// of information and the second is information.
func TestAnUnreachableNodesDevicesBecomeUnknownAndAreKept(t *testing.T) {
	r := sdReconciler(t, &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{}},
		sdNodeWithStatus(utils.NodeStatusUnreachable), sdNodeSet(),
		existingDevice(simplyblockv1alpha2.StorageDevicePhaseOnline, "online"))

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("the objects of an unreachable node must be kept: %v", err)
	}
	if sd.Status.Phase != simplyblockv1alpha2.StorageDevicePhaseUnknown {
		t.Errorf("phase = %q, want Unknown", sd.Status.Phase)
	}
	if !strings.Contains(sd.Status.Message, sdNodeCR) {
		t.Errorf("the message must name the node that cannot be seen: %q", sd.Status.Message)
	}
	if !announced(r.Recorder.(*events.FakeRecorder), "DeviceStateUnknown") {
		t.Error("no DeviceStateUnknown event")
	}
}

// A restart is the case that made the rule worth having: deleting and rebuilding
// would churn one object per device on every node restart.
func TestARestartingNodesDevicesAreKept(t *testing.T) {
	r := sdReconciler(t, &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{}},
		sdNodeWithStatus(utils.NodeStatusInRestart), sdNodeSet(),
		existingDevice(simplyblockv1alpha2.StorageDevicePhaseOnline, "online"))

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getSD(t, r.Client); err != nil {
		t.Errorf("a restarting node's devices must survive the restart: %v", err)
	}
}

// Failed is terminal, and an unreachable node does not revoke the judgment that
// put the device there.
func TestUnknownDoesNotOverwriteATerminalPhase(t *testing.T) {
	failed := existingDevice(simplyblockv1alpha2.StorageDevicePhaseFailed, cpDeviceFailed)
	failed.Status.Message = "the control plane reports the device failed"

	r := sdReconciler(t, &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{}},
		sdNodeWithStatus(utils.NodeStatusUnreachable), sdNodeSet(), failed)

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sd.Status.Phase != simplyblockv1alpha2.StorageDevicePhaseFailed {
		t.Errorf("phase = %q, want the terminal Failed kept", sd.Status.Phase)
	}
	if sd.Status.Message != "the control plane reports the device failed" {
		t.Errorf("the reason for a terminal phase was replaced: %q", sd.Status.Message)
	}
}

// An online node that stops reporting a device is information: the drive is
// gone. Which of the two events it gets is what says whether anybody asked for
// it.
func TestAPulledDriveIsDeletedAndWarnedAbout(t *testing.T) {
	r := sdReconciler(t, &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{}},
		sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet(),
		existingDevice(simplyblockv1alpha2.StorageDevicePhaseOnline, "online"))

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getSD(t, r.Client); !apierrors.IsNotFound(err) {
		t.Errorf("expected the object deleted, got %v", err)
	}
	rec := r.Recorder.(*events.FakeRecorder)
	reasons := drainReasons(rec)
	if !containsReason(reasons, "DeviceDisappeared") {
		t.Errorf("a drive nobody removed must be a warning, got %v", reasons)
	}
}

// A device the control plane last reported as removed left on purpose, so its
// disappearance is not a warning.
func TestARemovedDeviceIsDeletedWithoutAWarning(t *testing.T) {
	r := sdReconciler(t, &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{}},
		sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet(),
		existingDevice(simplyblockv1alpha2.StorageDevicePhaseRemoved, "removed"))

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	reasons := drainReasons(r.Recorder.(*events.FakeRecorder))
	if !containsReason(reasons, "DeviceRemoved") {
		t.Errorf("expected DeviceRemoved, got %v", reasons)
	}
	if containsReason(reasons, "DeviceDisappeared") {
		t.Errorf("a removal was reported as a disappearance: %v", reasons)
	}
}

func containsReason(reasons []string, reason string) bool {
	for _, r := range reasons {
		if strings.Contains(r, reason) {
			return true
		}
	}
	return false
}

// The lock is written by a StorageDeviceOps and by nothing else. The mirror
// rebuilds the whole status on every device event, so a mirror that does not
// carry the field over releases a lock its holder still believes it has.
func TestTheMirrorDoesNotClearTheOperationLock(t *testing.T) {
	held := existingDevice(simplyblockv1alpha2.StorageDevicePhaseOnline, "online")
	held.Status.ActiveOpsRef = "restart-7f3a9c"

	r := sdReconciler(t, sdOnlineCache(), sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet(), held)
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sd.Status.ActiveOpsRef != "restart-7f3a9c" {
		t.Errorf("activeOpsRef = %q, want the lock left alone", sd.Status.ActiveOpsRef)
	}
}

// A zero is the reading of an empty device rather than the absence of a reading,
// so a device the control plane reports no size for carries no capacity block at
// all. The alternative publishes a drive of zero bytes, which is a size nothing
// has.
func TestADeviceWithNoReportedSizeCarriesNoCapacity(t *testing.T) {
	cache := sdOnlineCache()
	dto := cache.devices[sdName()]
	dto.Size = 0
	cache.devices[sdName()] = dto

	r := sdReconciler(t, cache, sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sd.Status.Capacity != nil {
		t.Errorf("capacity = %+v, want it absent", sd.Status.Capacity)
	}
}

// The same for the hardware group: which of its fields are populated is what says
// how a device is attached, and an empty block says a device was reported with no
// identifying marks rather than nothing having been asked.
func TestADeviceWithNoHardwareFieldsCarriesNoHardware(t *testing.T) {
	cache := &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{
		sdName(): {ID: sdDevice, ClusterID: sdCluster, StorageNodeID: sdNodeID, Status: "online", Size: 4096},
	}}

	r := sdReconciler(t, cache, sdNodeWithStatus(utils.NodeStatusOnline), sdNodeSet())
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sd.Status.Hardware != nil {
		t.Errorf("hardware = %+v, want it absent", sd.Status.Hardware)
	}
	if sd.Status.Capacity == nil {
		t.Error("a reported size must still be published")
	}
}
