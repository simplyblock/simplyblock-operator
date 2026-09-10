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
	"fmt"

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
	*Cache[NodeDTO]
	NodeRegistry

	ch chan event.GenericEvent
}

// NewNodeSubscription returns a storage-node subscription. It is told no
// namespace: a StorageNode object already exists before its backend node is
// adopted, so the object it belongs to is supplied by RegisterNode rather than
// derived from an id.
func NewNodeSubscription() *NodeSubscription {
	return &NodeSubscription{
		Cache:        NewCache(func(n NodeDTO) string { return n.ID }),
		NodeRegistry: newNodeRegistry(),
		ch:           make(chan event.GenericEvent, 1024),
	}
}

// Name implements cpinformer.Subscription.
func (s *NodeSubscription) Name() string {
	return "storagenode"
}

// Path implements cpinformer.Subscription: nodes are scoped per cluster. One
// stream carries every node of the cluster, so the scope has a single element.
func (s *NodeSubscription) Path(scope cpinformer.Scope) string {
	return fmt.Sprintf("/api/v2/clusters/%s/storage-nodes/", scope[0])
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then enqueues a reconcile trigger for each affected node's StorageNode
// object. It performs no API I/O, so it never stalls the stream loop.
func (s *NodeSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	return s.Cache.Ingest(ev, func(_ cpinformer.Scope, nodeID string, _ bool) {
		// A node that left the cluster still has a CR, and only a reconcile can
		// decide what that CR should now say, so its disappearance is triggered
		// as readily as its arrival.
		s.enqueue(ctx, nodeID)
	})
}

// enqueue pushes a reconcile trigger naming the node's StorageNode object,
// giving up only on shutdown rather than dropping it when the channel is full
// (see [cpinformer.Subscription] on why waiting is the right side to err on).
//
// A node with no registered object yields no trigger: the operator either has
// not adopted it yet, or it belongs to a cluster this operator does not manage.
// Adoption is followed by a reconcile of its own, so nothing is lost.
func (s *NodeSubscription) enqueue(ctx context.Context, nodeID string) {
	key, ok := s.node(nodeID)
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

// Triggers is the reconcile-trigger channel, which the reconciler attaches via
// source.Channel. Each event names the StorageNode object to reconcile.
func (s *NodeSubscription) Triggers() <-chan event.GenericEvent {
	return s.ch
}

// Lookup returns the cached node with the given backend id, or ok=false when
// the control plane no longer reports it. It takes the id rather than an object
// key because the reconciler holds the id in the CR's own status, so no reverse
// mapping is needed on the read path.
func (s *NodeSubscription) Lookup(nodeID string) (cpinformer.Scope, NodeDTO, bool) {
	return s.Find(nodeID)
}

var _ cpinformer.Subscription = (*NodeSubscription)(nil)
