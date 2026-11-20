// The mapping from a backend cluster id to the StorageCluster object it was
// adopted as, which the StorageCluster controller registers and the backup
// subscriptions read.
//
// It is the cluster-level counterpart of the node mapping in registry.go, and
// exists for the same reason: the control plane knows nothing of Kubernetes
// object names, so a stream event carries a cluster id and the object it has to
// be turned into lives in a namespace only the operator knows. A backup and a
// backup policy are both scoped to one cluster and both become objects beside
// that cluster, so the one mapping serves both.

package subscriptions

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// ClusterRegistry records which StorageCluster object a backend cluster id
// belongs to. It is safe for concurrent use: a reconciler registers while the
// stream goroutine reads.
type ClusterRegistry struct {
	mu       sync.Mutex
	clusters map[string]types.NamespacedName
}

func newClusterRegistry() ClusterRegistry {
	return ClusterRegistry{clusters: map[string]types.NamespacedName{}}
}

// RegisterCluster records the object a backend cluster id names. The
// StorageCluster controller calls it as soon as it knows the id, which is what
// makes the cluster's backups nameable.
func (r *ClusterRegistry) RegisterCluster(clusterID string, cluster types.NamespacedName) {
	r.mu.Lock()
	r.clusters[clusterID] = cluster
	r.mu.Unlock()
}

// UnregisterCluster drops a cluster's mapping, which stops naming objects after
// a StorageCluster that is going away.
func (r *ClusterRegistry) UnregisterCluster(clusterID string) {
	r.mu.Lock()
	delete(r.clusters, clusterID)
	r.mu.Unlock()
}

// cluster returns the object a backend cluster id names, or ok=false when the
// operator has not adopted that cluster.
func (r *ClusterRegistry) cluster(clusterID string) (types.NamespacedName, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.clusters[clusterID]
	return key, ok
}
