// The cluster subscription: it streams every cluster the control plane holds,
// decodes and caches them, and enqueues a reconcile trigger naming the
// StorageCluster object each one backs. It lives here rather than in the
// controller package because retrieval, decoding, and caching are the
// subscription's concerns. Writing Kubernetes objects is the reconciler's.
//
// It is the only subscription in this package with no scope at all. The control
// plane serves every cluster from one route, so a single stream covers the
// whole installation, and one subscription therefore serves every
// StorageCluster in every namespace (design-storagecluster.md §4.4). The empty
// scope is what cpinformer.Scope's doc comment already reserves for it, and it
// is added once at startup rather than by any reconciler: there is no object
// whose arrival opens it and none whose departure closes it.
//
// One field of the wire schema is deliberately absent. ClusterDTO.secret is
// marked write-only in the control plane's own OpenAPI document, so the stream
// never carries it, and the creation path keeps reading it from the response to
// its POST and from the per-cluster Secret it writes. A mirror is a status
// source, not a credential source.

package subscriptions

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/event"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
)

// ClusterDTO is the operator's view of a control-plane cluster, matching the
// fields StorageCluster.status publishes and no others. Unknown fields are
// ignored on decode, so the rest of a cluster's wire schema costs nothing here.
//
// Every field below is required by the control plane's ClusterDTO schema, which
// is the completeness requirement a subscription has to meet before a
// reconciler may read it instead of the detail endpoint: the reconciler
// previously fetched one cluster at a time, and a list DTO thinner than the
// detail one would have silently zeroed whatever it omitted.
type ClusterDTO struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	NQN               string `json:"nqn"`
	Status            string `json:"status"`
	Rebalancing       bool   `json:"is_re_balancing"`
	NDCS              int    `json:"distr_ndcs"`
	NPCS              int    `json:"distr_npcs"`
	MaxFaultTolerance int    `json:"max_fault_tolerance"`
}

// ClusterSubscription streams every cluster, decodes them into an in-memory
// cache, and enqueues a reconcile trigger naming the affected StorageCluster
// object. It performs no Kubernetes writes. A reconciler consumes its cache
// ([ClusterSubscription.Lookup]) and trigger channel
// ([ClusterSubscription.Triggers]).
//
// The control plane knows nothing of Kubernetes object names, so the
// subscription keeps the backend-cluster-id-to-object mapping that the
// StorageCluster controller registers once it has created or adopted a backend
// cluster. That is what lets Ingest name an object without reading the API,
// which it must do without blocking: it runs on the stream goroutine.
type ClusterSubscription struct {
	*Cache[ClusterDTO]
	ClusterRegistry

	ch chan event.GenericEvent
}

// RootScope is the one scope this subscription streams. It is empty because the
// route takes no path parameter, and naming it is what keeps the caller that
// opens the stream and the reader that checks it synced from spelling the same
// empty literal twice.
var RootScope = cpinformer.Scope{}

// NewClusterSubscription returns a cluster subscription. It is told no
// namespace: a StorageCluster object exists before its backend cluster does, so
// the object each id belongs to is supplied by RegisterCluster rather than
// derived from the id.
func NewClusterSubscription() *ClusterSubscription {
	return &ClusterSubscription{
		Cache:           NewCache(func(c ClusterDTO) string { return c.ID }),
		ClusterRegistry: newClusterRegistry(),
		ch:              make(chan event.GenericEvent, 1024),
	}
}

// Name implements cpinformer.Subscription.
func (s *ClusterSubscription) Name() string { return "cluster" }

// Path implements cpinformer.Subscription. The scope is empty and is ignored:
// one stream carries every cluster.
func (s *ClusterSubscription) Path(cpinformer.Scope) string {
	return "/api/v2/clusters/"
}

// Ingest implements cpinformer.Subscription: it decodes the event into the
// cache, then enqueues a reconcile trigger for each affected cluster's
// StorageCluster object. It performs no API I/O, so it never stalls the stream
// loop.
func (s *ClusterSubscription) Ingest(ctx context.Context, ev cpinformer.Event) error {
	return s.Cache.Ingest(ev, func(_ cpinformer.Scope, clusterID string, _ bool) {
		// A cluster the control plane has stopped reporting still has a CR, and
		// only a reconcile can decide what that CR should now say, so its
		// disappearance is triggered as readily as its arrival.
		s.enqueue(ctx, clusterID)
	})
}

// enqueue pushes a reconcile trigger naming the cluster's StorageCluster
// object, giving up only on shutdown rather than dropping it when the channel
// is full (see [cpinformer.Subscription] on why waiting is the right side to
// err on).
//
// A cluster with no registered object yields no trigger: the operator either
// has not created or adopted it yet, or it belongs to an installation this
// operator does not manage. Adoption is followed by a reconcile of its own, so
// nothing is lost.
func (s *ClusterSubscription) enqueue(ctx context.Context, clusterID string) {
	key, ok := s.cluster(clusterID)
	if !ok {
		return
	}
	sc := &simplyblockv1alpha2.StorageCluster{}
	sc.SetNamespace(key.Namespace)
	sc.SetName(key.Name)
	select {
	case s.ch <- event.GenericEvent{Object: sc}:
	case <-ctx.Done():
	}
}

// Triggers is the reconcile-trigger channel, which the reconciler attaches via
// source.Channel. Each event names the StorageCluster object to reconcile.
func (s *ClusterSubscription) Triggers() <-chan event.GenericEvent { return s.ch }

// Lookup returns the cached cluster with the given backend id, or ok=false when
// the control plane no longer reports it. It takes the id rather than an object
// key because the reconciler holds the id in the CR's own status, so no reverse
// mapping is needed on the read path.
func (s *ClusterSubscription) Lookup(clusterID string) (ClusterDTO, bool) {
	_, dto, ok := s.Find(clusterID)
	return dto, ok
}

// SyncedRoot reports whether the one stream has delivered its snapshot. Until
// it has, a cluster missing from the cache is an absence of information rather
// than evidence the control plane has forgotten it.
func (s *ClusterSubscription) SyncedRoot() bool { return s.Synced(RootScope) }

var _ cpinformer.Subscription = (*ClusterSubscription)(nil)
