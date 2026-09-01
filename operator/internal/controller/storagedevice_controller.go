// The StorageDevice mirror: it turns what the control plane reports about a
// storage node's devices into one Kubernetes object per device. Nothing declares
// a device, so this reconciler owns the whole creation path as well as the
// update and delete ones, which is what makes it different from the reconcilers
// that converge a user's spec.

package controller

import (
	"context"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// deviceRetry is how long the mirror waits before looking again at something
// that is not wrong, only not ready: a node object that has not appeared yet, or
// a scope whose first snapshot is still in flight.
const deviceRetry = time.Second

// Control-plane device statuses, in the control plane's own spelling. They are
// listed here rather than shared with the subscription because grouping them
// into phases is this file's whole job.
const (
	cpDeviceOnline            = "online"
	cpDeviceJournal           = "JM_DEV"
	cpDeviceNew               = "new"
	cpDeviceUnavailable       = "unavailable"
	cpDeviceReadOnly          = "read_only"
	cpDeviceCannotAllocate    = "cannot_allocate"
	cpDeviceRemoved           = "removed"
	cpDeviceFailed            = "failed"
	cpDeviceFailedAndMigrated = "failed_and_migrated"
)

// DeviceCache is the read surface the reconciler needs from the device
// subscription: a trigger stream (each event naming a StorageDevice object), a
// by-object-name lookup of desired state, and a per-scope synced check. It keeps
// the reconciler independent of how devices are retrieved and cached.
type DeviceCache interface {
	// Triggers is the reconcile-trigger stream; each event names a StorageDevice.
	Triggers() <-chan event.GenericEvent
	// Lookup returns the device the named object mirrors and its scope, or
	// ok=false if the control plane no longer reports it.
	Lookup(objectName string) (cpinformer.Scope, subscriptions.DeviceDTO, bool)
	// Synced reports whether a scope's initial snapshot has been applied.
	Synced(scope cpinformer.Scope) bool
}

// StorageDeviceReconciler mirrors control-plane devices into StorageDevice
// objects. It is triggered per-object — by the subscription (a device changed)
// and by the object itself (drift and startup) — reads desired state from the
// cache rather than from the control-plane API, and writes through a workqueue,
// so the SSE stream is unaffected by API latency or write failures.
type StorageDeviceReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Devices DeviceCache
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices/status,verbs=get;update;patch

// SetupWithManager watches StorageDevice objects (for drift and to enumerate
// stale ones at startup) and the subscription's trigger stream (for
// control-plane changes). Both enqueue a StorageDevice to reconcile.
func (r *StorageDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageDevice{}).
		Named("storagedevice").
		WatchesRawSource(source.Channel(r.Devices.Triggers(), &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// Reconcile converges one StorageDevice toward the cache's view of its
// control-plane device: create or update while the device is reported, delete
// once it is not. An object whose device is absent from a not-yet-synced scope
// is left alone, because a cold cache is an absence of information rather than
// information.
func (r *StorageDeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	scope, dto, inCache := r.Devices.Lookup(req.Name)

	var sd simplyblockv1alpha1.StorageDevice
	err := r.Get(ctx, req.NamespacedName, &sd)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	switch {
	case inCache && len(scope) == 2:
		return r.upsert(ctx, req.NamespacedName, scope, dto, exists, &sd)

	case exists:
		// The device is gone from the control plane, but only a synced scope can
		// say so: the drive was pulled, or removed by an operation, and either
		// way the object records hardware that is no longer there.
		if !r.Devices.Synced(cpinformer.Scope{sd.Status.ClusterID, sd.Status.NodeID}) {
			return ctrl.Result{RequeueAfter: deviceRetry}, nil
		}
		if err := r.Delete(ctx, &sd); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil

	default:
		return ctrl.Result{}, nil // nothing cached and no object — nothing to do
	}
}

// upsert creates or updates the mirror object at key to reflect dto. The owning
// StorageNode has to exist first: it is what the object is named after and what
// garbage-collects it, so an object without one would be named for a node that
// is not there and would outlive the node it belongs to.
func (r *StorageDeviceReconciler) upsert(
	ctx context.Context,
	key client.ObjectKey,
	scope cpinformer.Scope,
	dto subscriptions.DeviceDTO,
	exists bool,
	sd *simplyblockv1alpha1.StorageDevice,
) (ctrl.Result, error) {
	node, err := r.nodeFor(ctx, key.Namespace, scope[1])
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == nil {
		return ctrl.Result{RequeueAfter: deviceRetry}, nil
	}

	spec := simplyblockv1alpha1.StorageDeviceSpec{NodeRef: node.Name, DeviceID: dto.ID}
	status := simplyblockv1alpha1.StorageDeviceStatus{
		Phase:        devicePhase(dto),
		DeviceStatus: dto.Status,
		Role:         deviceRole(dto),
		Capacity: &simplyblockv1alpha1.DeviceCapacity{
			TotalBytes: ptr.To(dto.Size),
			UsedBytes:  ptr.To(dto.Capacity.SizeUsed),
		},
		Hardware: &simplyblockv1alpha1.DeviceHardware{
			PCIAddress:     dto.PCIeAddress,
			SerialNumber:   dto.SerialNumber,
			Model:          dto.Model,
			NVMeController: dto.NVMeController,
		},
		ClusterID: scope[0],
		NodeID:    scope[1],
	}

	if !exists {
		*sd = simplyblockv1alpha1.StorageDevice{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec:       spec,
		}
		if err := controllerutil.SetControllerReference(node, sd, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, sd); err != nil {
			return ctrl.Result{}, err
		}
	} else if sd.Spec != spec {
		sd.Spec = spec
		if err := r.Update(ctx, sd); err != nil {
			return ctrl.Result{}, err
		}
	}

	status.ObservedGeneration = sd.Generation
	if !reflect.DeepEqual(sd.Status, status) {
		sd.Status = status
		return ctrl.Result{}, r.Status().Update(ctx, sd)
	}
	return ctrl.Result{}, nil
}

// nodeFor returns the StorageNode carrying the given backend node id, or nil
// when none does. A nil node is not an error: the node's own object may not have
// been created yet, or may already be on its way out.
func (r *StorageDeviceReconciler) nodeFor(ctx context.Context, namespace, nodeID string) (*simplyblockv1alpha1.StorageNode, error) {
	var nodes simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range nodes.Items {
		if nodes.Items[i].Status.UUID == nodeID {
			return &nodes.Items[i], nil
		}
	}
	return nil, nil
}

// devicePhase groups the control plane's device vocabulary into the operator's
// five phases. The grouping is the whole of what the phase adds over
// status.deviceStatus, which keeps the original string.
//
// Degraded is "serving and should not be," so it covers a device the control
// plane still reads from while it reports errors, a device that is present but
// not yet part of the layout, and a device that has gone read-only. Failed is
// terminal and means the cluster is running with less redundancy than it thinks.
// Unknown is an absence of information: a status the operator does not
// recognize is not evidence that the device is bad.
func devicePhase(dto subscriptions.DeviceDTO) simplyblockv1alpha1.StorageDevicePhase {
	switch dto.Status {
	case cpDeviceOnline, cpDeviceJournal:
		if deviceUnhealthy(dto) {
			return simplyblockv1alpha1.StorageDevicePhaseDegraded
		}
		return simplyblockv1alpha1.StorageDevicePhaseOnline
	case cpDeviceNew, cpDeviceUnavailable, cpDeviceReadOnly, cpDeviceCannotAllocate:
		return simplyblockv1alpha1.StorageDevicePhaseDegraded
	case cpDeviceRemoved:
		return simplyblockv1alpha1.StorageDevicePhaseRemoved
	case cpDeviceFailed, cpDeviceFailedAndMigrated:
		return simplyblockv1alpha1.StorageDevicePhaseFailed
	default:
		return simplyblockv1alpha1.StorageDevicePhaseUnknown
	}
}

// deviceUnhealthy reports whether a device the control plane still counts as
// serving is reporting trouble. A nil health check means the check does not
// apply — the owning node is neither online nor down — which is not a failing
// one.
func deviceUnhealthy(dto subscriptions.DeviceDTO) bool {
	return (dto.HealthCheck != nil && !*dto.HealthCheck) || dto.IOError || dto.RetriesExhaust
}

// deviceRole reports what the device carries. The control plane says so through
// the device's status rather than through a field of its own: a journal device
// reports JM_DEV and everything else is a storage device.
func deviceRole(dto subscriptions.DeviceDTO) simplyblockv1alpha1.StorageDeviceRole {
	if dto.Status == cpDeviceJournal {
		return simplyblockv1alpha1.StorageDeviceRoleJournal
	}
	return simplyblockv1alpha1.StorageDeviceRoleStorage
}

// the device subscription satisfies the read surface this reconciler needs.
var _ DeviceCache = (*subscriptions.DeviceSubscription)(nil)
