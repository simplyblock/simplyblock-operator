// The mapping from a backend node id to the StorageNode object it was adopted
// as, which the StorageNode controller registers and two subscriptions read.
//
// It lives beside them rather than inside either because the mapping is one
// fact with one writer: the control plane knows nothing of Kubernetes object
// names, so the only place both ids are known at once is the reconciler that
// adopted the node, and it registers the same mapping with everything that
// names objects after a node.

package subscriptions

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// NodeRegistry records which StorageNode object a backend node id belongs to.
// It is safe for concurrent use: a reconciler registers while the stream
// goroutine reads.
type NodeRegistry struct {
	mu    sync.Mutex
	nodes map[string]types.NamespacedName
}

func newNodeRegistry() NodeRegistry {
	return NodeRegistry{nodes: map[string]types.NamespacedName{}}
}

// RegisterNode records the object a backend node id names. The StorageNode
// controller calls it as soon as it knows the id, which is what makes the
// node's events nameable.
func (r *NodeRegistry) RegisterNode(nodeID string, node types.NamespacedName) {
	r.mu.Lock()
	r.nodes[nodeID] = node
	r.mu.Unlock()
}

// UnregisterNode drops a node's mapping, which stops naming events after a
// StorageNode that is going away.
func (r *NodeRegistry) UnregisterNode(nodeID string) {
	r.mu.Lock()
	delete(r.nodes, nodeID)
	r.mu.Unlock()
}

// node returns the object a backend node id names, or ok=false when the
// operator has not adopted that node.
func (r *NodeRegistry) node(nodeID string) (types.NamespacedName, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.nodes[nodeID]
	return key, ok
}
