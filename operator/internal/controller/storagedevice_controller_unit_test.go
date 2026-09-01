// Tests for the StorageDevice mirror: the projection from a control-plane
// device status to a typed phase, and the create, update, and delete paths of
// the reconciler that publishes it.

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	sdCluster = "11111111-1111-1111-1111-111111111111"
	sdNodeID  = "44444444-4444-4444-4444-444444444444"
	sdDevice  = "5e0000a1-3b2c-4d5e-9f01-2a3b4c5d6e7f"
	sdNodeCR  = "production-7f3a9c"
)

func sdScope() cpinformer.Scope { return cpinformer.Scope{sdCluster, sdNodeID} }

func sdName() string { return simplyblockv1alpha1.StorageDeviceName(sdNodeCR, sdDevice) }

// fakeDeviceCache is a static DeviceCache for reconciler tests.
type fakeDeviceCache struct {
	synced  bool
	devices map[string]subscriptions.DeviceDTO
}

func (f *fakeDeviceCache) Triggers() <-chan event.GenericEvent { return nil }
func (f *fakeDeviceCache) Synced(cpinformer.Scope) bool        { return f.synced }
func (f *fakeDeviceCache) Lookup(objectName string) (cpinformer.Scope, subscriptions.DeviceDTO, bool) {
	dto, ok := f.devices[objectName]
	if !ok {
		return nil, subscriptions.DeviceDTO{}, false
	}
	return sdScope(), dto, true
}

func sdNodeObject() *simplyblockv1alpha1.StorageNode {
	return &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: sdNodeCR},
		Status:     simplyblockv1alpha1.StorageNodeStatus{UUID: sdNodeID},
	}
}

// sdReconciler builds its client directly rather than through newTestClient,
// because the mirror resolves a device's node through a field index and the
// fake client only answers a MatchingFields query for an index it was given.
func sdReconciler(t *testing.T, cache DeviceCache, objs ...client.Object) *StorageDeviceReconciler {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.StorageDevice{}).
		WithIndex(&simplyblockv1alpha1.StorageNode{}, StorageNodeUUIDIndex, IndexStorageNodeUUID).
		WithObjects(objs...).
		Build()
	return &StorageDeviceReconciler{Client: c, Scheme: scheme, Devices: cache}
}

func sdReq() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "sb", Name: sdName()}}
}

func getSD(t *testing.T, c client.Client) (*simplyblockv1alpha1.StorageDevice, error) {
	t.Helper()
	var sd simplyblockv1alpha1.StorageDevice
	err := c.Get(context.Background(), sdReq().NamespacedName, &sd)
	return &sd, err
}

// The control plane's own vocabulary is wider than the operator's five phases,
// and the grouping is the whole of what the phase adds over the raw string.
func TestDevicePhaseFromStatus(t *testing.T) {
	cases := []struct {
		name string
		dto  subscriptions.DeviceDTO
		want simplyblockv1alpha1.StorageDevicePhase
	}{
		{"online", subscriptions.DeviceDTO{Status: "online"}, simplyblockv1alpha1.StorageDevicePhaseOnline},
		{"journal device is serving", subscriptions.DeviceDTO{Status: "JM_DEV"}, simplyblockv1alpha1.StorageDevicePhaseOnline},
		{"failed", subscriptions.DeviceDTO{Status: "failed"}, simplyblockv1alpha1.StorageDevicePhaseFailed},
		{"failed and migrated is still failed", subscriptions.DeviceDTO{Status: "failed_and_migrated"}, simplyblockv1alpha1.StorageDevicePhaseFailed},
		{"removed", subscriptions.DeviceDTO{Status: "removed"}, simplyblockv1alpha1.StorageDevicePhaseRemoved},
		// Serving and should not be: the definition of Degraded.
		{"read only", subscriptions.DeviceDTO{Status: "read_only"}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		{"cannot allocate", subscriptions.DeviceDTO{Status: "cannot_allocate"}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		{"new is not yet in the layout", subscriptions.DeviceDTO{Status: "new"}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		{"unavailable", subscriptions.DeviceDTO{Status: "unavailable"}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		// An online device whose health the control plane reports as bad is
		// serving and should not be, which is Degraded rather than Online.
		{"failing health check", subscriptions.DeviceDTO{Status: "online", HealthCheck: ptr.To(false)}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		{"io error", subscriptions.DeviceDTO{Status: "online", IOError: true}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		{"retries exhausted", subscriptions.DeviceDTO{Status: "online", RetriesExhaust: true}, simplyblockv1alpha1.StorageDevicePhaseDegraded},
		// A health check that does not apply is not a failing one.
		{"health check not applicable", subscriptions.DeviceDTO{Status: "online", HealthCheck: nil}, simplyblockv1alpha1.StorageDevicePhaseOnline},
		{"unrecognized status", subscriptions.DeviceDTO{Status: "something-new"}, simplyblockv1alpha1.StorageDevicePhaseUnknown},
		{"empty status", subscriptions.DeviceDTO{}, simplyblockv1alpha1.StorageDevicePhaseUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := devicePhase(tc.dto); got != tc.want {
				t.Errorf("devicePhase(%+v) = %q, want %q", tc.dto, got, tc.want)
			}
		})
	}
}

func TestDeviceRoleFromStatus(t *testing.T) {
	if got := deviceRole(subscriptions.DeviceDTO{Status: "JM_DEV"}); got != simplyblockv1alpha1.StorageDeviceRoleJournal {
		t.Errorf("journal device role = %q", got)
	}
	if got := deviceRole(subscriptions.DeviceDTO{Status: "online"}); got != simplyblockv1alpha1.StorageDeviceRoleStorage {
		t.Errorf("storage device role = %q", got)
	}
}

func TestStorageDeviceReconcileCreatesAndUpdates(t *testing.T) {
	cache := &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{
		sdName(): {
			ID: sdDevice, ClusterID: sdCluster, StorageNodeID: sdNodeID,
			Status: "online", Size: 3840755982336,
			Model: "SAMSUNG MZQL23T8HCLS-00A07", SerialNumber: "S4J9NX0R500123",
			PCIeAddress: "0000:5e:00.0", NVMeController: "nvme0",
			Capacity: subscriptions.DeviceCapacityDTO{SizeUsed: 1920377991168},
		},
	}}
	r := sdReconciler(t, cache, sdNodeObject())

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sd, err := getSD(t, r.Client)
	if err != nil {
		t.Fatalf("expected mirror object: %v", err)
	}
	if sd.Spec.NodeRef != sdNodeCR || sd.Spec.DeviceID != sdDevice {
		t.Errorf("spec = %+v", sd.Spec)
	}
	if sd.Status.Phase != simplyblockv1alpha1.StorageDevicePhaseOnline || sd.Status.DeviceStatus != "online" {
		t.Errorf("status = %+v", sd.Status)
	}
	if sd.Status.Capacity == nil || *sd.Status.Capacity.TotalBytes != 3840755982336 || *sd.Status.Capacity.UsedBytes != 1920377991168 {
		t.Errorf("capacity = %+v", sd.Status.Capacity)
	}
	if sd.Status.Hardware == nil || sd.Status.Hardware.PCIAddress != "0000:5e:00.0" || sd.Status.Hardware.SerialNumber != "S4J9NX0R500123" {
		t.Errorf("hardware = %+v", sd.Status.Hardware)
	}
	if sd.Status.ClusterID != sdCluster || sd.Status.NodeID != sdNodeID {
		t.Errorf("scope not recorded: %+v", sd.Status)
	}

	// The node owns its devices, so deleting the node garbage-collects them.
	owners := sd.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "StorageNode" || owners[0].Name != sdNodeCR {
		t.Errorf("owner references = %+v", owners)
	}

	dto := cache.devices[sdName()]
	dto.Status = "failed"
	cache.devices[sdName()] = dto
	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sd, _ = getSD(t, r.Client)
	if sd.Status.Phase != simplyblockv1alpha1.StorageDevicePhaseFailed || sd.Status.DeviceStatus != "failed" {
		t.Errorf("status not updated: %+v", sd.Status)
	}
}

// A device on a node whose object is not there yet cannot be named after it, so
// the mirror waits rather than creating an ownerless object.
func TestStorageDeviceReconcileWaitsForItsNode(t *testing.T) {
	cache := &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{
		sdName(): {ID: sdDevice, ClusterID: sdCluster, StorageNodeID: sdNodeID, Status: "online"},
	}}
	r := sdReconciler(t, cache) // no StorageNode object

	res, err := r.Reconcile(context.Background(), sdReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the owning node is absent")
	}
	if _, err := getSD(t, r.Client); !apierrors.IsNotFound(err) {
		t.Errorf("no object may be created without its node: %v", err)
	}
}

func TestStorageDeviceReconcileDeletesWhenGoneAndSynced(t *testing.T) {
	existing := &simplyblockv1alpha1.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: sdName()},
		Spec:       simplyblockv1alpha1.StorageDeviceSpec{NodeRef: sdNodeCR, DeviceID: sdDevice},
		Status:     simplyblockv1alpha1.StorageDeviceStatus{ClusterID: sdCluster, NodeID: sdNodeID},
	}
	// Device absent from the cache, scope synced → the drive is gone and so is
	// its object.
	r := sdReconciler(t, &fakeDeviceCache{synced: true, devices: map[string]subscriptions.DeviceDTO{}}, existing)

	if _, err := r.Reconcile(context.Background(), sdReq()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getSD(t, r.Client); !apierrors.IsNotFound(err) {
		t.Errorf("expected object deleted, got %v", err)
	}
}

func TestStorageDeviceReconcileWaitsForSyncBeforeDeleting(t *testing.T) {
	existing := &simplyblockv1alpha1.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: sdName()},
		Spec:       simplyblockv1alpha1.StorageDeviceSpec{NodeRef: sdNodeCR, DeviceID: sdDevice},
		Status:     simplyblockv1alpha1.StorageDeviceStatus{ClusterID: sdCluster, NodeID: sdNodeID},
	}
	// Absent from a NOT-yet-synced cache is an absence of information rather
	// than information, so the object must be left alone.
	r := sdReconciler(t, &fakeDeviceCache{synced: false, devices: map[string]subscriptions.DeviceDTO{}}, existing)

	res, err := r.Reconcile(context.Background(), sdReq())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the scope is not synced")
	}
	if _, err := getSD(t, r.Client); err != nil {
		t.Errorf("object must not be deleted before sync: %v", err)
	}
}
