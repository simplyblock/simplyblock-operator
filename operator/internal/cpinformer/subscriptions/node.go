// The storage-node subscription: it streams a cluster's storage nodes, decodes
// and caches them, and enqueues a reconcile trigger naming the StorageNode
// object each one backs. It lives here rather than in the controller package
// because retrieval, decoding, and caching are the subscription's concerns.
// Writing Kubernetes objects is the reconciler's.
//
// Unlike devices, the stream is per cluster rather than per node: the control
// plane serves every node of a cluster from one route, so a scope is a cluster
// on its own and one stream covers all of its nodes.

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

// NodeDTO is the operator's view of a control-plane storage node, matching the
// fields the StorageNode reconciler publishes and no others. Unknown fields are
// ignored on decode, so the rest of a node's wire schema costs nothing here.
//
// Every field below was verified present on the list stream's payload, which is
// the completeness requirement a subscription has to meet before a reconciler
// may read it instead of the detail endpoint: the reconciler previously fetched
// one node at a time, and a list DTO thinner than the detail one would have
// silently zeroed whatever it omitted.
type NodeDTO struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	ManagementIP  string `json:"mgmt_ip"`
	HealthCheck   bool   `json:"health_check"`
	Hostname      string `json:"hostname"`
	CPUCount      int32  `json:"cpu_spdk_count"`
	Volumes       int32  `json:"lvols"`
	RPCPort       int32  `json:"rpc_port"`
	LvolPort      int32  `json:"lvol_subsys_port"`
	NVMeOFPort    int32  `json:"nvmf_port"`
	FailureDomain int    `json:"failure_domain"`
}

// NodeSubscription streams a cluster's storage nodes, decodes them into an
// in-memory cache, and enqueues a reconcile trigger naming the affected
// StorageNode object. It performs no Kubernetes writes. A reconciler consumes
// its cache ([NodeSubscription.Lookup]) and trigger channel
// ([NodeSubscription.Triggers]).
//
// The control plane knows nothing of Kubernetes object names, so the
// subscription keeps the backend-node-id-to-object mapping that the StorageNode
// controller registers once it has adopted a backend node. That is what lets
// Ingest name an object without reading the API, which it must do without
// blocking: it runs on the stream goroutine.
type NodeSubscription struct {
	store *cpinformer.Store[NodeDTO]
	ch    chan event.GenericEvent

	mu     sync.Mutex
	nodes  map[string]types.NamespacedName // backend node id -> StorageNode object
	synced map[string]bool                 // scopeKey -> first snapshot applied
}

// NewNodeSubscription returns a storage-node subscription. It is told no
// namespace: a StorageNode object already exists before its backend node is
// adopted, so the object it belongs to is supplied by RegisterNode rather than
// derived from an id.
func NewNodeSubscription() *NodeSubscription {
	return &NodeSubscription{
		store:  cpinformer.NewStore(func(n NodeDTO) string { return n.ID }),
		ch:     make(chan event.GenericEvent, 1024),
		nodes:  map[string]types.NamespacedName{},
		synced: map[string]bool{},
	}
}

// Name implements cpinformer.Subscription.
func (s *NodeSubscription) Name() string { return "storagenode" }

// Path implements cpinformer.Subscription: nodes are scoped per cluster. One
// stream carries every node of the cluster, so the scope has a single element.
func (s *NodeSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", scope[0])
}

// RegisterNode records the StorageNode object that a backend node id belongs to.
// The StorageNode controller calls it as soon as it knows the id, which is what
// makes the node's events nameable.
func (s *NodeSubscription) RegisterNode(nodeID string, node types.NamespacedName) {
	s.mu.Lock()
	s.nodes[nodeID] = node
	s.mu.Unlock()
}

// UnregisterNode drops a node's mapping, which stops naming events after a
// StorageNode that is going away.
func (s *NodeSubscription) UnregisterNode(nodeID string) {
	s.mu.Lock()
	delete(s.nodes, nodeID)
	s.mu.Unlock()
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then enqueues a reconcile trigger for each affected node's StorageNode
// object. It performs no API I/O, so it never stalls the stream loop.
func (s *NodeSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	switch ev.Kind {
	case cpinformer.EventSnapshot:
		var dtos []NodeDTO
		if err := json.Unmarshal(ev.Data, &dtos); err != nil {
			return fmt.Errorf("decode snapshot: %w", err)
		}
		present, removed := s.store.Replace(ev.Scope, dtos)
		s.markSynced(ev.Scope)
		// Re-sync every current node, and every one that vanished while
		// disconnected: a node removed from the cluster still has a CR, and only
		// a reconcile can decide what that CR should now say.
		for _, id := range append(present, removed...) {
			s.enqueue(ctx, id)
		}

	case cpinformer.EventCreated, cpinformer.EventUpdated:
		var dto NodeDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode %s: %w", ev.Kind, err)
		}
		s.store.Upsert(ev.Scope, dto)
		s.enqueue(ctx, dto.ID)

	case cpinformer.EventDeleted:
		var dto NodeDTO
		if err := json.Unmarshal(ev.Data, &dto); err != nil {
			return fmt.Errorf("decode deleted: %w", err)
		}
		if dto.ID == "" {
			return nil // an empty delete carries no id, so the relist covers it
		}
		s.store.Remove(ev.Scope, dto.ID)
		s.enqueue(ctx, dto.ID)
	}
	return nil
}

// enqueue pushes a reconcile trigger naming the node's StorageNode object,
// giving up only on shutdown rather than dropping it when the channel is full
// (see [cpinformer.Subscription] on why waiting is the right side to err on).
//
// A node with no registered object yields no trigger: the operator either has
// not adopted it yet, or it belongs to a cluster this operator does not manage.
// Adoption is followed by a reconcile of its own, so nothing is lost.
func (s *NodeSubscription) enqueue(ctx context.Context, nodeID string) {
	key, ok := s.objectKey(nodeID)
	if !ok {
		return
	}
	sn := &simplyblockv1alpha1.StorageNode{}
	sn.SetNamespace(key.Namespace)
	sn.SetName(key.Name)
	select {
	case s.ch <- event.GenericEvent{Object: sn}:
	case <-ctx.Done():
	}
}

func (s *NodeSubscription) objectKey(nodeID string) (types.NamespacedName, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.nodes[nodeID]
	return key, ok
}

// Triggers is the reconcile-trigger channel, which the reconciler attaches via
// source.Channel. Each event names the StorageNode object to reconcile.
func (s *NodeSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached node with the given backend id, or ok=false when
// the control plane no longer reports it. It takes the id rather than an object
// key because the reconciler holds the id in the CR's own status, so no reverse
// mapping is needed on the read path.
func (s *NodeSubscription) Lookup(nodeID string) (cpinformer.Scope, NodeDTO, bool) {
	if nodeID == "" {
		return nil, NodeDTO{}, false
	}
	return s.store.Find(nodeID)
}

// Synced reports whether a scope has received its initial snapshot. Until it
// has, an absent node is an absence of information rather than evidence that
// the control plane has forgotten it.
func (s *NodeSubscription) Synced(scope cpinformer.Scope) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[scope.Key()]
}

func (s *NodeSubscription) markSynced(scope cpinformer.Scope) {
	s.mu.Lock()
	s.synced[scope.Key()] = true
	s.mu.Unlock()
}

var _ cpinformer.Subscription = (*NodeSubscription)(nil)
