// The device subscription: it streams one storage node's backend devices,
// decodes and caches them, and enqueues a reconcile trigger naming the
// StorageDevice object each one mirrors. It lives here rather than in the
// controller package because retrieval, decoding, and caching are the
// subscription's concerns; writing Kubernetes objects is the reconciler's.

package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// DeviceCapacityDTO is the capacity block the control plane reports with a
// device. Only the used size is read here; the device's own size field is the
// authority on how big it is.
type DeviceCapacityDTO struct {
	SizeUsed int64 `json:"size_used"`
}

// DeviceDTO is the operator's view of a control-plane device, matching the v2
// DeviceDTO wire schema. Unknown fields are ignored on decode, so the fields the
// mirror does not publish (I/O stats, fabric addresses) cost nothing here.
type DeviceDTO struct {
	ID             string            `json:"id"`
	ClusterID      string            `json:"cluster_id"`
	StorageNodeID  string            `json:"storage_node_id"`
	Model          string            `json:"model"`
	SerialNumber   string            `json:"serial_number"`
	NVMeController string            `json:"nvme_controller"`
	PCIeAddress    string            `json:"pcie_address"`
	Status         string            `json:"status"`
	HealthCheck    *bool             `json:"health_check"`
	RetriesExhaust bool              `json:"retries_exhausted"`
	IOError        bool              `json:"io_error"`
	Size           int64             `json:"size"`
	Capacity       DeviceCapacityDTO `json:"capacity"`
}

// DeviceSubscription streams a storage node's devices (one stream per node),
// decodes them into an in-memory cache, and enqueues a reconcile trigger naming
// the affected StorageDevice object. It performs no Kubernetes writes; a
// reconciler consumes its cache ([DeviceSubscription.Lookup]) and trigger
// channel ([DeviceSubscription.Triggers]).
//
// A device object is named after the StorageNode object that owns it, and the
// control plane knows nothing of Kubernetes object names. So the subscription
// keeps the backend-node-id-to-object mapping that the StorageNode controller
// registers alongside the scope it streams, which is what lets Ingest name an
// object without reading the API — it runs on the stream goroutine and must
// never block on I/O.
//
// The mapping carries the node's namespace and not only its name, because the
// device object has to be created beside its node rather than beside the
// operator: it holds a controller reference to the node, and a namespaced owner
// in another namespace is one the garbage collector reads as absent.
type DeviceSubscription struct {
	store *cpinformer.Store[DeviceDTO]
	ch    chan event.GenericEvent

	mu    sync.Mutex
	nodes map[string]types.NamespacedName // backend node id -> StorageNode object
	// byObject indexes the cache the way the reconciler reads it: a reconcile
	// request carries an object key, and only the subscription can turn one back
	// into a device id, because only it knows the naming rule and the node
	// objects it depends on. It holds an entry for every cached device and
	// nothing else, so a miss means the control plane no longer reports the
	// device.
	byObject map[string]string // "namespace/name" -> backend device id
	synced   map[string]bool   // scopeKey -> first snapshot applied
}

// NewDeviceSubscription returns a device subscription. It is not told a
// namespace: each device object belongs in the namespace of the StorageNode that
// owns it, which RegisterNode supplies.
func NewDeviceSubscription() *DeviceSubscription {
	return &DeviceSubscription{
		store:    cpinformer.NewStore(func(d DeviceDTO) string { return d.ID }),
		ch:       make(chan event.GenericEvent, 1024),
		nodes:    map[string]types.NamespacedName{},
		byObject: map[string]string{},
		synced:   map[string]bool{},
	}
}

// Name implements cpinformer.Subscription.
func (s *DeviceSubscription) Name() string { return "device" }

// Path implements cpinformer.Subscription: devices are scoped per (cluster,
// storage node). The control plane offers no cluster-wide device stream, so one
// stream is opened per storage node rather than one per cluster.
func (s *DeviceSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/%s/devices/", scope[0], scope[1])
}

// RegisterNode records the StorageNode object that devices on the given backend
// node belong to. The StorageNode controller calls it before adding the node's
// scope, so the object is known by the time the stream delivers anything.
func (s *DeviceSubscription) RegisterNode(nodeID string, node types.NamespacedName) {
	s.mu.Lock()
	s.nodes[nodeID] = node
	s.mu.Unlock()
}

// UnregisterNode drops a node's name mapping, which stops naming objects after
// a StorageNode that is going away.
func (s *DeviceSubscription) UnregisterNode(nodeID string) {
	s.mu.Lock()
	delete(s.nodes, nodeID)
	s.mu.Unlock()
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then enqueues a reconcile trigger for each affected device's
// StorageDevice object. It performs no API I/O, so it never stalls the stream
// loop.
func (s *DeviceSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	switch ev.Kind {
	case cpinformer.EventSnapshot:
		var dtos []DeviceDTO
		if err := json.Unmarshal(ev.Data, &dtos); err != nil {
			return fmt.Errorf("decode snapshot: %w", err)
		}
		present, removed := s.store.Replace(ev.Scope, dtos)
		s.markSynced(ev.Scope)
		// Re-sync every current device, and remove any that vanished while
		// disconnected — a drive pulled from a running node looks like nothing
		// else.
		for _, id := range present {
			s.index(ev.Scope, id)
			s.enqueue(ctx, ev.Scope, id)
		}
		for _, id := range removed {
			s.enqueue(ctx, ev.Scope, id)
			s.unindex(ev.Scope, id)
		}

	case cpinformer.EventCreated, cpinformer.EventUpdated:
		var dto DeviceDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode %s: %w", ev.Kind, err)
		}
		s.store.Upsert(ev.Scope, dto)
		s.index(ev.Scope, dto.ID)
		s.enqueue(ctx, ev.Scope, dto.ID)

	case cpinformer.EventDeleted:
		var dto DeviceDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode deleted: %w", err)
		}
		if dto.ID == "" {
			return nil // physical removal carries {}; the snapshot relist covers it
		}
		s.store.Remove(ev.Scope, dto.ID)
		s.enqueue(ctx, ev.Scope, dto.ID)
		s.unindex(ev.Scope, dto.ID)
	}
	return nil
}

// index and unindex keep byObject in step with the cache. Both are no-ops while
// the device's node has no registered name, which is the same condition under
// which no trigger is emitted: without a node name there is no object name, so
// there is nothing to index and nothing to reconcile.
func (s *DeviceSubscription) index(scope cpinformer.Scope, deviceID string) {
	if key, ok := s.objectKey(scope, deviceID); ok {
		s.mu.Lock()
		s.byObject[key.String()] = deviceID
		s.mu.Unlock()
	}
}

func (s *DeviceSubscription) unindex(scope cpinformer.Scope, deviceID string) {
	if key, ok := s.objectKey(scope, deviceID); ok {
		s.mu.Lock()
		delete(s.byObject, key.String())
		s.mu.Unlock()
	}
}

func (s *DeviceSubscription) objectKey(scope cpinformer.Scope, deviceID string) (types.NamespacedName, bool) {
	node, ok := s.node(scope[1])
	if !ok {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{
		Namespace: node.Namespace,
		Name:      simplyblockv1alpha1.StorageDeviceName(node.Name, deviceID),
	}, true
}

// enqueue pushes a reconcile trigger naming the device's StorageDevice object,
// giving up only on shutdown rather than dropping it when the channel is full
// (see [cpinformer.Subscription] on why waiting is the right side to err on).
//
// A device whose node has no registered object name yields no trigger: the
// object is named after that node, and there is nothing to name it after yet.
// The node's registration is followed by the stream's snapshot, which enqueues
// everything.
func (s *DeviceSubscription) enqueue(ctx context.Context, scope cpinformer.Scope, deviceID string) {
	key, ok := s.objectKey(scope, deviceID)
	if !ok {
		return
	}
	sd := &simplyblockv1alpha1.StorageDevice{}
	sd.SetNamespace(key.Namespace)
	sd.SetName(key.Name)
	select {
	case s.ch <- event.GenericEvent{Object: sd}:
	case <-ctx.Done():
	}
}

func (s *DeviceSubscription) node(nodeID string) (types.NamespacedName, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[nodeID]
	return node, ok
}

// Triggers is the reconcile-trigger channel; the reconciler attaches it via
// source.Channel. Each event names the StorageDevice object to reconcile.
func (s *DeviceSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached device that the named StorageDevice object mirrors,
// with the scope it belongs to, or ok=false if the control plane no longer
// reports it. It takes an object key rather than a device id because that is
// what a reconcile request carries, and the namespace is part of the identity:
// two namespaces may each hold a StorageNode of the same name.
func (s *DeviceSubscription) Lookup(key types.NamespacedName) (cpinformer.Scope, DeviceDTO, bool) {
	s.mu.Lock()
	deviceID, ok := s.byObject[key.String()]
	s.mu.Unlock()
	if !ok {
		return nil, DeviceDTO{}, false
	}
	return s.store.Find(deviceID)
}

// Synced reports whether a scope has received its initial snapshot; the
// reconciler must not treat an unsynced scope's cache as authoritative (i.e.
// must not delete an object just because the device is absent from a cold
// cache).
func (s *DeviceSubscription) Synced(scope cpinformer.Scope) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[scope.Key()]
}

func (s *DeviceSubscription) markSynced(scope cpinformer.Scope) {
	s.mu.Lock()
	s.synced[scope.Key()] = true
	s.mu.Unlock()
}

var _ cpinformer.Subscription = (*DeviceSubscription)(nil)
