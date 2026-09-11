// The StorageDevice mirror: it turns what the control plane reports about a
// storage node's devices into one Kubernetes object per device. Nothing declares
// a device, so this reconciler owns the whole creation path as well as the
// update and delete ones, which is what makes it different from the reconcilers
// that converge a user's spec.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// StorageNodeUUIDIndex indexes StorageNode objects by the backend node id in
// their status. The mirror resolves a device's node by that id on every
// reconcile, and there is one device object per physical drive, so a scan over
// every node in the namespace is work proportional to nodes times devices.
const StorageNodeUUIDIndex = "status.uuid"

// IndexStorageNodeUUID is the index function for [StorageNodeUUIDIndex],
// exported so that a test's client can register the same index the manager
// does. A node with no id yet is not indexed: it has nothing a device could
// match against.
func IndexStorageNodeUUID(o client.Object) []string {
	node, ok := o.(*simplyblockv1alpha1.StorageNode)
	if !ok || node.Status.UUID == "" {
		return nil
	}
	return []string{node.Status.UUID}
}

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
	Lookup(key types.NamespacedName) (cpinformer.Scope, subscriptions.DeviceDTO, bool)
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
	Scheme   *runtime.Scheme
	Devices  DeviceCache
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagedevices/status,verbs=get;update;patch

// SetupWithManager watches StorageDevice objects (for drift and to enumerate
// stale ones at startup) and the subscription's trigger stream (for
// control-plane changes). Both enqueue a StorageDevice to reconcile.
func (r *StorageDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &simplyblockv1alpha1.StorageNode{}, StorageNodeUUIDIndex, IndexStorageNodeUUID,
	); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageDevice{}).
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
	scope, dto, inCache := r.Devices.Lookup(req.NamespacedName)

	var sd simplyblockv1alpha2.StorageDevice
	err := r.Get(ctx, req.NamespacedName, &sd)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	switch {
	case inCache && len(scope) == 2:
		return r.upsert(ctx, req.NamespacedName, scope, dto)

	case exists:
		return r.unreported(ctx, &sd)

	default:
		return ctrl.Result{}, nil // nothing cached and no object — nothing to do
	}
}

// unreported handles an object whose device the control plane no longer reports.
// Whether that is information depends on who stopped talking: a synced scope on
// an online node saying nothing about a device is a device that is gone, and the
// same silence from a node nobody can reach is an absence of information.
func (r *StorageDeviceReconciler) unreported(
	ctx context.Context, sd *simplyblockv1alpha2.StorageDevice,
) (ctrl.Result, error) {
	// A cold cache has not yet said anything at all.
	if !r.Devices.Synced(cpinformer.Scope{sd.Status.ClusterID, sd.Status.NodeID}) {
		return ctrl.Result{RequeueAfter: deviceRetry}, nil
	}

	node, err := r.nodeNamed(ctx, sd.Namespace, sd.Spec.NodeRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	// An offline node reports no devices, which is not the same statement as a
	// node reporting that a device is gone: the first is an absence of
	// information and the second is information. Deleting on the first would
	// churn one object per device on every node restart.
	if node != nil && !nodeSeesItsDevices(node) {
		return ctrl.Result{}, r.markUnobservable(ctx, sd, node)
	}

	if err := r.Delete(ctx, sd); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.announceDeparture(sd, node)
	return ctrl.Result{}, nil
}

// markUnobservable moves a device whose node cannot be reached to Unknown and
// keeps the object. Unknown says the operator cannot see the device rather than
// that the device is bad, and the device takes whatever the node reports when it
// returns.
//
// A device already in a terminal phase keeps it, along with the reason it
// carries: Failed records a judgment that an unreachable node does not revoke,
// and Removed records a departure that has already happened. Unknown replaces
// Online and Degraded, which are observations of a device that was serving.
func (r *StorageDeviceReconciler) markUnobservable(
	ctx context.Context, sd *simplyblockv1alpha2.StorageDevice, node *simplyblockv1alpha1.StorageNode,
) error {
	switch sd.Status.Phase {
	case simplyblockv1alpha2.StorageDevicePhaseFailed,
		simplyblockv1alpha2.StorageDevicePhaseRemoved,
		simplyblockv1alpha2.StorageDevicePhaseUnknown:
		return nil
	}

	previous := sd.Status.Phase
	sd.Status.Phase = simplyblockv1alpha2.StorageDevicePhaseUnknown
	sd.Status.Message = fmt.Sprintf(
		"storage node %s is %s, so the device's state is not observable", node.Name, nodeState(node))
	if err := r.Status().Update(ctx, sd); err != nil {
		return err
	}
	r.announcePhase(sd, previous)
	return nil
}

// announceDeparture reports a device that stopped being reported, on the node
// rather than on the device: the device's object is being deleted at that
// moment, and an event on it is an event nobody reads.
//
// A device the control plane last reported as removed left because something
// asked it to, and a device that was serving until it vanished is a drive
// somebody pulled. What the control plane does not say is which operation
// removed it, so the two events distinguish an orderly departure from an abrupt
// one and no more than that.
func (r *StorageDeviceReconciler) announceDeparture(
	sd *simplyblockv1alpha2.StorageDevice, node *simplyblockv1alpha1.StorageNode,
) {
	if node == nil {
		return // the node is gone too, and garbage collection is the whole story
	}
	if sd.Status.DeviceStatus == cpDeviceRemoved {
		r.Recorder.Eventf(node, sd, corev1.EventTypeNormal, "DeviceRemoved", "DeviceRemoved",
			"device %s was removed from the node", sd.Spec.DeviceID)
		return
	}
	r.Recorder.Eventf(node, sd, corev1.EventTypeWarning, "DeviceDisappeared", "DeviceDisappeared",
		"device %s stopped being reported with no operation having removed it", sd.Spec.DeviceID)
}

// announcePhase reports a device that moved, on the device itself. Only a change
// is announced: a reconcile that finds the device where it left it says nothing,
// or the event stream becomes a log of how often the operator looked.
func (r *StorageDeviceReconciler) announcePhase(
	sd *simplyblockv1alpha2.StorageDevice, previous simplyblockv1alpha2.StorageDevicePhase,
) {
	if previous == sd.Status.Phase {
		return
	}
	switch sd.Status.Phase {
	case simplyblockv1alpha2.StorageDevicePhaseFailed:
		r.Recorder.Eventf(sd, nil, corev1.EventTypeWarning, "DeviceFailed", "DeviceFailed",
			"%s", sd.Status.Message)
	case simplyblockv1alpha2.StorageDevicePhaseDegraded:
		r.Recorder.Eventf(sd, nil, corev1.EventTypeWarning, "DeviceDegraded", "DeviceDegraded",
			"%s", sd.Status.Message)
	case simplyblockv1alpha2.StorageDevicePhaseUnknown:
		r.Recorder.Eventf(sd, nil, corev1.EventTypeNormal, "DeviceStateUnknown", "DeviceStateUnknown",
			"%s", sd.Status.Message)
	case simplyblockv1alpha2.StorageDevicePhaseOnline:
		// A device that was Online the first time anybody looked did not
		// recover, so discovery is not announced as a recovery. The node's
		// DeviceDiscovered event is what covers that moment.
		if previous != "" {
			r.Recorder.Eventf(sd, nil, corev1.EventTypeNormal, "DeviceOnline", "DeviceOnline",
				"%s", sd.Status.Message)
		}
	}
}

// nodeSeesItsDevices reports whether the node's device list is worth believing
// the absence of. Only the states in which the control plane is talking to the
// node qualify, and an unrecognized state does not: a status the operator does
// not know is an absence of information, the same way an unrecognized device
// status is (see [devicePhase]).
func nodeSeesItsDevices(node *simplyblockv1alpha1.StorageNode) bool {
	switch nodeState(node) {
	case utils.NodeStatusOnline, utils.NodeStatusSuspended, utils.NodeStatusRemoved:
		return true
	default:
		return false
	}
}

// nodeState is the node's control-plane status folded to lower case, or
// "unknown" when it has none yet.
func nodeState(node *simplyblockv1alpha1.StorageNode) string {
	if node.Status.Status == "" {
		return "unknown"
	}
	return strings.ToLower(node.Status.Status)
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
) (ctrl.Result, error) {
	node, err := r.nodeFor(ctx, key.Namespace, scope[1])
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == nil {
		return ctrl.Result{RequeueAfter: deviceRetry}, nil
	}

	labels, err := r.deviceLabels(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	spec := simplyblockv1alpha2.StorageDeviceSpec{NodeRef: node.Name, DeviceID: dto.ID}
	status := simplyblockv1alpha2.StorageDeviceStatus{
		Phase:        devicePhase(dto),
		DeviceStatus: dto.Status,
		Role:         deviceRole(dto),
		Capacity:     deviceCapacity(dto),
		Hardware:     deviceHardware(dto),
		ClusterID:    scope[0],
		NodeID:       scope[1],
		Message:      deviceMessage(dto),
	}

	// The mirror is enqueued by its own writes and, independently, by the
	// control-plane stream, which does not wait for the informer cache to catch
	// up. A reconcile can therefore start from a cached object older than the one
	// the API server holds, and its write is rejected with a conflict. Read the
	// object again and write again rather than surfacing that: what the object
	// should say is computed from the control-plane cache and not from the object,
	// so a retry converges on the same result instead of losing a decision.
	//
	// The re-read is served by the same cache, so this narrows the window rather
	// than closing it; a cache that is still behind after the backoff returns the
	// conflict and the reconcile is requeued as before.
	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var sd simplyblockv1alpha2.StorageDevice
		err := r.Get(ctx, key, &sd)
		discovered := false
		switch {
		case apierrors.IsNotFound(err):
			sd = simplyblockv1alpha2.StorageDevice{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: key.Namespace, Name: key.Name, Labels: labels,
				},
				Spec: spec,
			}
			if err := controllerutil.SetControllerReference(node, &sd, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, &sd); err != nil {
				return err
			}
			discovered = true
		case err != nil:
			return err
		default:
			// The spec and the labels are both the mirror's output, so both are
			// written back when they drift. A label the mirror does not own is
			// left where it is: the object is the operator's to describe and
			// somebody else's to annotate.
			changed := applyLabels(&sd, labels)
			if sd.Spec != spec {
				sd.Spec = spec
				changed = true
			}
			if changed {
				if err := r.Update(ctx, &sd); err != nil {
					return err
				}
			}
		}

		previous := sd.Status.Phase
		status.ObservedGeneration = sd.Generation
		// The lock is a StorageDeviceOps's to take and release. The mirror
		// rebuilds the rest of the status on every device event, so carrying the
		// field over is what stops an unrelated event releasing a lock its
		// holder still believes it has.
		status.ActiveOpsRef = sd.Status.ActiveOpsRef
		if !reflect.DeepEqual(sd.Status, status) {
			sd.Status = status
			if err := r.Status().Update(ctx, &sd); err != nil {
				return err
			}
		}

		if discovered {
			r.Recorder.Eventf(node, &sd, corev1.EventTypeNormal, "DeviceDiscovered", "DeviceDiscovered",
				"device %s was found on the node and is %s", dto.ID, strings.ToLower(string(sd.Status.Phase)))
		}
		r.announcePhase(&sd, previous)
		return nil
	})
}

// deviceLabels are the labels a device object carries, resolved from its node.
// The cluster is only reachable through the node's set: a StorageNode names its
// StorageNodeSet and the set names the cluster, and neither the device nor the
// node carries the cluster's object name itself.
//
// A label whose source cannot be resolved is left out rather than guessed. An
// absent label is a selector that matches nothing, and a wrong one is a selector
// that matches the wrong devices.
func (r *StorageDeviceReconciler) deviceLabels(
	ctx context.Context, node *simplyblockv1alpha1.StorageNode,
) (map[string]string, error) {
	labels := map[string]string{simplyblockv1alpha2.DeviceLabelNode: node.Name}
	if worker := node.Labels[simplyblockv1alpha2.DeviceLabelWorker]; worker != "" {
		labels[simplyblockv1alpha2.DeviceLabelWorker] = worker
	}
	if node.Spec.StorageNodeSetRef == "" {
		return labels, nil
	}

	var set simplyblockv1alpha1.StorageNodeSet
	key := client.ObjectKey{Namespace: node.Namespace, Name: node.Spec.StorageNodeSetRef}
	switch err := r.Get(ctx, key, &set); {
	case apierrors.IsNotFound(err):
		return labels, nil
	case err != nil:
		return nil, err
	}
	if set.Spec.ClusterName != "" {
		labels[simplyblockv1alpha2.DeviceLabelCluster] = set.Spec.ClusterName
	}
	return labels, nil
}

// applyLabels writes the mirror's labels onto sd and reports whether anything
// changed. Only the keys the mirror owns are touched, so a label somebody else
// put on the object survives a reconcile.
func applyLabels(sd *simplyblockv1alpha2.StorageDevice, owned map[string]string) bool {
	changed := false
	for key, value := range owned {
		if sd.Labels[key] == value {
			continue
		}
		if sd.Labels == nil {
			sd.Labels = map[string]string{}
		}
		sd.Labels[key] = value
		changed = true
	}
	return changed
}

// deviceCapacity is the device's size, or nil when the control plane reports
// none. A zero is the size of nothing, so publishing one would say a drive is
// zero bytes rather than that nobody reported how big it is.
func deviceCapacity(dto subscriptions.DeviceDTO) *simplyblockv1alpha2.DeviceCapacity {
	if dto.Size <= 0 {
		return nil
	}
	return &simplyblockv1alpha2.DeviceCapacity{TotalBytes: ptr.To(dto.Size)}
}

// deviceHardware identifies the part, or nil when the control plane reports
// nothing that does. Which fields are populated is what says how a device is
// attached, so an empty group would say a device was reported with no
// identifying marks rather than that there is nothing to say.
func deviceHardware(dto subscriptions.DeviceDTO) *simplyblockv1alpha2.DeviceHardware {
	hardware := simplyblockv1alpha2.DeviceHardware{
		PCIAddress:     dto.PCIeAddress,
		SerialNumber:   dto.SerialNumber,
		Model:          dto.Model,
		NVMeController: dto.NVMeController,
	}
	if hardware == (simplyblockv1alpha2.DeviceHardware{}) {
		return nil
	}
	return &hardware
}

// deviceMessage is the one sentence status.message carries: why the phase is
// what it is, in terms of what the control plane said. It names the signal
// rather than restating the phase, because three different things make a serving
// device degraded and they are not fixed the same way.
func deviceMessage(dto subscriptions.DeviceDTO) string {
	switch dto.Status {
	case cpDeviceOnline, cpDeviceJournal:
		if trouble := deviceTrouble(dto); trouble != "" {
			return "the device is serving and reporting trouble: " + trouble
		}
		if dto.Status == cpDeviceJournal {
			return "the control plane reports the device serving as a journal device"
		}
		return "the control plane reports the device online"
	case cpDeviceNew:
		return "the control plane reports the device new, so it is not yet part of the layout"
	case cpDeviceUnavailable, cpDeviceReadOnly, cpDeviceCannotAllocate:
		return fmt.Sprintf(
			"the control plane reports status %q, which is serving and should not be", dto.Status)
	case cpDeviceRemoved:
		return "the control plane reports the device removed from the node"
	case cpDeviceFailed, cpDeviceFailedAndMigrated:
		return fmt.Sprintf(
			"the control plane reports status %q, so the cluster is running with less redundancy "+
				"than it thinks until the device is replaced", dto.Status)
	default:
		return fmt.Sprintf(
			"the control plane reports status %q, which this operator does not recognize", dto.Status)
	}
}

// deviceTrouble names what an otherwise-serving device is reporting, or "" when
// it is reporting nothing. All three are listed when all three hold: they are
// separate signals and a device showing every one of them is worse than a device
// showing one.
func deviceTrouble(dto subscriptions.DeviceDTO) string {
	var signals []string
	if dto.HealthCheck != nil && !*dto.HealthCheck {
		signals = append(signals, "its health check is failing")
	}
	if dto.IOError {
		signals = append(signals, "an I/O error")
	}
	if dto.RetriesExhaust {
		signals = append(signals, "exhausted retries")
	}
	return strings.Join(signals, ", ")
}

// nodeNamed returns the StorageNode of the given object name, or nil when it is
// gone. It is the lookup the unreported path needs: an object that outlived its
// device still names its node in its own spec, and there is no device left to
// resolve a backend id from.
func (r *StorageDeviceReconciler) nodeNamed(
	ctx context.Context, namespace, name string,
) (*simplyblockv1alpha1.StorageNode, error) {
	var node simplyblockv1alpha1.StorageNode
	switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &node); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &node, nil
}

// nodeFor returns the StorageNode carrying the given backend node id, or nil
// when none does. A nil node is not an error: the node's own object may not have
// been created yet, or may already be on its way out.
func (r *StorageDeviceReconciler) nodeFor(ctx context.Context, namespace, nodeID string) (*simplyblockv1alpha1.StorageNode, error) {
	var nodes simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &nodes,
		client.InNamespace(namespace),
		client.MatchingFields{StorageNodeUUIDIndex: nodeID},
	); err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return nil, nil
	}
	return &nodes.Items[0], nil
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
func devicePhase(dto subscriptions.DeviceDTO) simplyblockv1alpha2.StorageDevicePhase {
	switch dto.Status {
	case cpDeviceOnline, cpDeviceJournal:
		if deviceUnhealthy(dto) {
			return simplyblockv1alpha2.StorageDevicePhaseDegraded
		}
		return simplyblockv1alpha2.StorageDevicePhaseOnline
	case cpDeviceNew, cpDeviceUnavailable, cpDeviceReadOnly, cpDeviceCannotAllocate:
		return simplyblockv1alpha2.StorageDevicePhaseDegraded
	case cpDeviceRemoved:
		return simplyblockv1alpha2.StorageDevicePhaseRemoved
	case cpDeviceFailed, cpDeviceFailedAndMigrated:
		return simplyblockv1alpha2.StorageDevicePhaseFailed
	default:
		return simplyblockv1alpha2.StorageDevicePhaseUnknown
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
func deviceRole(dto subscriptions.DeviceDTO) simplyblockv1alpha2.StorageDeviceRole {
	if dto.Status == cpDeviceJournal {
		return simplyblockv1alpha2.StorageDeviceRoleJournal
	}
	return simplyblockv1alpha2.StorageDeviceRoleStorage
}

// the device subscription satisfies the read surface this reconciler needs.
var _ DeviceCache = (*subscriptions.DeviceSubscription)(nil)
